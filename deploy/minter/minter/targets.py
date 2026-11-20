#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""目标注册表 —— **显式白名单**，不是自动发现。

为什么是白名单而不是"调用方传 URL"：本服务的产物（Turnstile token）能过掉站点的提交闸门，
等同于"过闸能力"。一旦允许调用方指定任意 URL，它就变成**公网浏览器即服务**——
既能被拿去铸造别处的 token，也能被拿来做 SSRF 探测。所以：

  · 目标只能**由部署方在配置文件里注册**（`targets.yaml` / `targets.json`）；
  · 请求只能**引用注册名**（`?target=<name>`），永远不接受 URL；
  · 未知名字 ⇒ 400，并带上已知名字（帮人拼对，但不做模糊匹配、不做自动发现）。

面向"几百个目标"的两个设计点：
  · **注册表热重载**（按 mtime）⇒ 加目标不用重启服务、不用重下 Chrome 镜像；
  · **池是 opt-in**（`prewarm: true`）⇒ 几百个目标不可能都预热，默认按需现铸。

解析失败时**保留上一份可用注册表**（fail-safe）：一次手滑改坏 YAML 不应该让正在服务的
目标集体 400。
"""
from __future__ import annotations

import json
import re
import time
from dataclasses import dataclass, field
from pathlib import Path

_NAME_RE = re.compile(r"^[a-z0-9][a-z0-9_-]{0,63}$")
_SITEKEY_RE = re.compile(r"^0x[0-9A-Za-z_-]{10,64}$")


class TargetError(ValueError):
    """注册表本身有问题（配错、重名、sitekey/URL 不合法）。"""


@dataclass(frozen=True)
class Target:
    """一个铸造目标 = 一组 (sitekey, 轻量同源页)。

    🔴 CF 的硬要求：`sitekey` 只在**它自己的 origin** 页面里能 `render()`（否则 110200），
    所以 `light_url` 必须与 `origin` **同源** —— 这条在加载时就校验，不留到运行期。
    `light_url` 刻意取"轻量页"（robots.txt / favicon 一类）：比加载整个应用页快一倍，
    且绕开"页面框架什么时候挂 widget"这个变量。
    """

    name: str
    sitekey: str
    light_url: str
    origin: str | None = None
    prewarm: bool = False
    note: str = ""
    # 每目标覆盖（None = 用全局默认）。为什么需要：全局 POOL_TARGET/TOKEN_TTL_S 是
    # 按"典型目标"定的，但几百个目标里必然有异类 —— 主力站点该有更深的池，
    # 低频站点池深 1 就够（占着池深等于白烧挑战额度）。而 TTL 是**站点侧属性**：
    # CF 给的 token 寿命约 300s，但各站点自己的校验窗口更短且各不相同，
    # 按最保守的那个站点设全局值，会让宽松站点的 token 被过早丢弃、白铸一轮。
    # ⚠️ `?reserve=N` 只解决**临时**尖峰，这两个覆盖解决**长期**差异，互不替代。
    pool_target: int | None = None
    ttl_s: float | None = None

    def as_public(self) -> dict:
        """对外（`GET /targets`）可见的字段 —— 刻意不含任何凭据。"""
        return {
            "name": self.name,
            "sitekey": self.sitekey,
            "light_url": self.light_url,
            "origin": self.origin,
            "prewarm": self.prewarm,
            "note": self.note,
            "pool_target": self.pool_target,
            "ttl_s": self.ttl_s,
        }


def _origin_of(url: str) -> str:
    m = re.match(r"^(https?://[^/]+)", url)
    return m.group(1) if m else ""


def _validate(raw: dict, *, index: int) -> Target:
    if not isinstance(raw, dict):
        raise TargetError(f"targets[{index}] 必须是对象，实际是 {type(raw).__name__}")

    name = str(raw.get("name") or "").strip()
    if not _NAME_RE.match(name):
        raise TargetError(
            f"targets[{index}].name={name!r} 不合法：只允许小写字母/数字/下划线/连字符，"
            "且以字母或数字开头（长度 1..64）。"
        )

    sitekey = str(raw.get("sitekey") or "").strip()
    if not _SITEKEY_RE.match(sitekey):
        raise TargetError(f"target {name}: sitekey={sitekey!r} 不像 Cloudflare sitekey（应以 0x 开头）。")

    light_url = str(raw.get("light_url") or "").strip()
    if not light_url.startswith(("http://", "https://")):
        raise TargetError(f"target {name}: light_url 必须是 http(s) 绝对地址。")

    origin = str(raw.get("origin") or "").strip() or _origin_of(light_url) or None
    if origin and _origin_of(light_url) != origin:
        raise TargetError(
            f"target {name}: light_url 与 origin 不同源（{_origin_of(light_url)} != {origin}）—— "
            "CF 要求 sitekey 只在同源页面里 render（否则 110200）。"
        )
    if light_url.startswith("http://") and "localhost" not in light_url and "127.0.0.1" not in light_url:
        raise TargetError(
            f"target {name}: light_url 用的是明文 http:// —— 只允许 localhost/127.0.0.1 例外（本地联调）。"
        )

    prewarm = raw.get("prewarm", False)
    if not isinstance(prewarm, bool):
        raise TargetError(f"target {name}: prewarm 必须是布尔（true/false），实际是 {prewarm!r}。")

    # 覆盖项：**写错就报错**，不静默回落到全局默认 —— 一个手滑的 pool_target: "4"
    # （字符串）若被当成"没配"，表现是池深莫名不对，而配置看起来完全正常。
    pool_target = raw.get("pool_target")
    if pool_target is not None:
        if isinstance(pool_target, bool) or not isinstance(pool_target, int):
            raise TargetError(
                f"target {name}: pool_target 必须是整数（实际 {pool_target!r}）—— "
                "留空则使用全局 POOL_TARGET。"
            )
        if not 0 <= pool_target <= 64:
            raise TargetError(
                f"target {name}: pool_target={pool_target} 超出 0..64。池深直接决定"
                "对该站点的持续挑战频率，给一个荒谬的值等于自我拉黑。"
            )

    ttl_s = raw.get("ttl_s")
    if ttl_s is not None:
        if isinstance(ttl_s, bool) or not isinstance(ttl_s, (int, float)):
            raise TargetError(
                f"target {name}: ttl_s 必须是数字（实际 {ttl_s!r}）—— 留空则使用全局 TOKEN_TTL_S。"
            )
        # 上界 300：CF 侧 token 寿命约 300s，配更大只会让池里堆着**已经失效**的 token，
        # 调用方拿到后在站点侧被拒，而本服务这边看起来一切正常（最难查的一类）。
        if not 10 <= float(ttl_s) <= 300:
            raise TargetError(
                f"target {name}: ttl_s={ttl_s} 超出 10..300 秒（CF 侧 token 寿命约 300s，"
                "配更大等于在池里存放已失效的 token）。"
            )
        ttl_s = float(ttl_s)

    return Target(
        name=name,
        sitekey=sitekey,
        light_url=light_url,
        origin=origin,
        prewarm=prewarm,
        note=str(raw.get("note") or ""),
        pool_target=pool_target,
        ttl_s=ttl_s,
    )


def _load_file(path: Path) -> list[Target]:
    text = path.read_text(encoding="utf-8")
    suffix = path.suffix.lower()
    if suffix in (".yaml", ".yml"):
        try:
            import yaml  # 可选依赖；没装也能用 .json
        except ImportError as exc:  # pragma: no cover - 取决于部署是否装 PyYAML
            raise TargetError(
                f"{path.name} 是 YAML，但环境里没有 PyYAML。两条出路："
                "① `pip install PyYAML`；② 把注册表改成 .json（本服务两种都认）。"
            ) from exc
        data = yaml.safe_load(text)
    else:
        data = json.loads(text)

    if isinstance(data, dict) and "targets" in data:
        data = data["targets"]
    elif isinstance(data, dict):
        # 也接受 {name: {sitekey, light_url, ...}} 的紧凑写法
        data = [dict(v, name=k) for k, v in data.items()]
    if not isinstance(data, list):
        raise TargetError("注册表根节点必须是列表，或含 targets 列表的映射。")
    if not data:
        raise TargetError("注册表里一个 target 都没有。")

    targets = [_validate(raw, index=i) for i, raw in enumerate(data)]
    seen: set[str] = set()
    for t in targets:
        if t.name in seen:
            raise TargetError(f"注册表里 target 名字重复：{t.name!r}（会在白名单上产生歧义）。")
        seen.add(t.name)
    return targets


@dataclass
class TargetRegistry:
    """注册表 + 热重载 + 默认目标解析。

    `default_name`：`/v1/turnstile/mint` 不带 `?target=` 时用谁。
      · 显式给了名字 ⇒ 必须在注册表里（写错就是配置错误，启动即报）；
      · 没给 + 注册表里**只有一个**目标 ⇒ 就是它（这是 aivideomaker 那种"单目标部署"的
        平滑路径：它的客户端不发 target，行为与旧服务逐字一致）；
      · 没给 + 多个目标 ⇒ **没有默认**，调用方必须显式指定（拒绝猜）。
    """

    path: Path | None = None
    default_name: str | None = None
    _targets: dict[str, Target] = field(default_factory=dict)
    _default: Target | None = None
    _mtime: float = 0.0
    _loaded_at: float = 0.0
    load_error: str | None = None

    def load(self) -> None:
        if self.path is None:
            self._targets, self._default = {}, self._resolve_default(None)
            self.load_error = None
            return
        if not self.path.exists():
            raise TargetError(f"注册表文件不存在：{self.path}")
        targets = _load_file(self.path)
        self._targets = {t.name: t for t in targets}
        self._default = self._resolve_default(self.default_name)
        self._mtime = self.path.stat().st_mtime
        self._loaded_at = time.time()
        self.load_error = None

    def _resolve_default(self, name: str | None) -> Target | None:
        if not self._targets:
            return None
        if name:
            if name not in self._targets:
                raise TargetError(
                    f"MINTER_DEFAULT_TARGET={name!r} 不在注册表里（已知：{sorted(self._targets)}）。"
                )
            return self._targets[name]
        if len(self._targets) == 1:
            return next(iter(self._targets.values()))
        return None      # 多目标且未指定 ⇒ 没有默认，别猜

    def maybe_reload(self) -> bool:
        """按 mtime 热重载。**解析失败不动现有注册表**（只记 load_error）。"""
        if self.path is None or not self.path.exists():
            return False
        try:
            mtime = self.path.stat().st_mtime
        except OSError:
            return False
        if mtime == self._mtime:
            return False
        try:
            targets = _load_file(self.path)
            keep_default = self._default.name if self._default else self.default_name
            names = {t.name: t for t in targets}
            new_default = (
                names[keep_default] if keep_default in names
                else (next(iter(names.values())) if len(names) == 1 else None)
            )
        except Exception as exc:  # noqa: BLE001 - 坏配置不能拖垮正在服务的目标
            self.load_error = f"{type(exc).__name__}: {exc}"[:300]
            self._mtime = mtime      # 别反复重试同一个坏文件
            return False
        self._targets, self._default, self.load_error = names, new_default, None
        self._mtime = mtime
        self._loaded_at = time.time()
        return True

    # ------------------------------------------------------------------ 读 --
    def names(self) -> list[str]:
        return sorted(self._targets)

    def get(self, name: str) -> Target:
        try:
            return self._targets[name]
        except KeyError as exc:
            raise KeyError(name) from exc

    def default(self) -> Target | None:
        return self._default

    def all_targets(self) -> list[Target]:
        return [self._targets[n] for n in self.names()]

    def state(self) -> dict:
        return {
            "path": str(self.path) if self.path else None,
            "count": len(self._targets),
            "names": self.names(),
            "default": self._default.name if self._default else None,
            "loaded_at": self._loaded_at or None,
            "load_error": self.load_error,
        }


__all__ = ["Target", "TargetError", "TargetRegistry"]
