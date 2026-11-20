#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""minter-service —— 通用的 **Cloudflare Turnstile 铸造服务**（多目标）。

把 `engine.py`（CDP 引擎，来自已实战七轮的 aivideomaker 实现）包成一个常驻 HTTP 服务，
并按**目标注册表**支持多个站点共用一个进程。

## 为什么是"一个进程服务多个目标"

  · 一个目标 = 一组 `(sitekey, 同源轻量页)`；CF 的 token 绑 sitekey + 域名，**不绑账号**
    ⇒ 同一台出口上完全可以服务多个站点；
  · 浏览器只有**一个**（Chrome 进程 + 每目标一个 tab）⇒ 几百个目标也不会几百个 Chrome；
  · **懒启动 + LRU**：只有被用过的目标才占 tab，`MAX_WARM_TARGETS` 之外的最久未用者被回收
    （池子不回收，只回收热页）；
  · **池是 opt-in**（注册表里 `prewarm: true`）：几百个目标不可能都预热，默认按需现铸。

## 契约（与 aivideomaker 的既有客户端**向后兼容**）

    GET  /healthz                       整体状态 + 每目标状态（`?targets=0` 只看整体）
    GET  /targets                       注册表（名字/sitekey/light_url，无凭据）
    POST /v1/turnstile/mint             铸一个：默认目标（单目标部署时可省略 target）
    POST /v1/turnstile/mint?target=<名>  指定目标；**名字不在注册表 ⇒ 400**（不做自动发现）
    POST /v1/turnstile/mint?n=4         批量（同一目标）
    POST /v1/turnstile/mint?reserve=20  预约：声明"之后还要 20 个"，把池深临时抬高。
                                        预约会随 MINT_RESERVE_TTL_S 过期，取用时逐个核销。
      头：X-Minter-Key: <key>
      → 200 {"token","source","age_ms","target"} ／ 401 头不对 ／ 400 目标未知 ／ 503 铸造失败

## 保留自原实现的关键纪律（都是实机踩出来的，别"简化"掉）

  1. **两把锁分开**：铸造锁可能被持有数十秒到两分钟（冷启动），状态锁毫秒级。
     曾经共用一把 ⇒ `/healthz` 被正在进行的铸造阻塞、探活超时 —— 服务看着"挂了"，其实只是忙。
  2. **补货线程必须连 `SystemExit` 一起接**：`ensure()` 起不来 Chrome 抛的是 SystemExit
     （BaseException 子类），只 catch Exception 会让补货线程**静默死亡**
     （现象：ready=false / minting=false / last_error=null）。
  3. **失败后只换热页、不重启 Chrome**：CF 对 profile 信誉的判定落在 profile 上，
     杀 Chrome 重启不会重置它，只会白付一次冷启动。
  4. **`ready` ≠ `warmed`**：前者是"Chrome/页面就绪"，后者是"真的铸出过 token"。
     只看 ready 会把"一直铸不出来"误判成"服务是好的"。
  5. **TZ 不做隐式兜底**：缺 TZ 的表现是"服务照常 ready、铸造永远 interactive"，
     是最难从现象反推配置的一种。这里只把它做成一眼可见（`/healthz.tz`），不悄悄替人设好。
"""
from __future__ import annotations

import json
import os
import random
import signal
import sys
import threading
import time
import urllib.request
from collections import deque
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlparse

# 🔴 **必须在 import 引擎之前**设定默认 CDP 端口（引擎在 import 时读 env 并把值钉成模块常量）。
#
# 为什么服务的默认不是引擎的 9222：9222 是"人手一份"的调试端口，**开发机/共享机上常年被
# 别的 Chrome 调试实例占着**（实测：宿主机 9222 上同时叠着 IPv4 与 IPv6 两个外来实例）。
# 而引擎清理残留实例的判据是"端口不再回应 CDP"—— 外来实例对 `/json/version` 回 **404**，
# 这个异常被当成"端口已释放"⇒ 新 Chrome 绑不上 9222，随后 90 秒轮询全失败，
# 最终以一句 `CDP 起不来` 收场，**根因（端口被外来实例占）在现象里看不见**。
# 选 9333 是为了把这类碰撞概率降下来；显式设 `CDP_PORT` 仍然优先（setdefault 不覆盖）。
os.environ.setdefault("CDP_PORT", "9333")

from . import engine as M  # noqa: E402  （必须在上面那行之后）
from .targets import Target, TargetError, TargetRegistry  # noqa: E402

# --------------------------------------------------------------------- 配置 --
PORT = int(os.environ.get("PORT", "8899"))
# ⚠️ 默认只绑回环；**容器里必须绑 0.0.0.0**，否则 docker-proxy 转发进来的连接无人接收 ——
#    宿主侧表现为 `Connection reset by peer`，而容器内自测却是好的（最迷惑的一种）。
BIND_HOST = os.environ.get("BIND_HOST", "127.0.0.1")
KEY = os.environ.get("MINTER_KEY", "")
POOL_TARGET = int(os.environ.get("POOL_TARGET", "4"))
TTL_S = float(os.environ.get("TOKEN_TTL_S", "120"))      # 保守：CF 侧约 300s，站点校验按更短算
# 预约（`?reserve=N`）：调用方声明"之后还要 N 个"，把池深临时抬到 POOL_TARGET+N。
#
# 为什么值得做：批量任务的调用方**发起时就知道要多少个**，而这个信息原先被丢掉了，
# 服务只能等池被抽干后逐个现铸（每个 1-1.7s 串行）。预约把"未来需求"提前告知，
# 让补货线程在调用方还在处理前几个时就把后几个铸好 —— 同样的产能，命中率不同。
#
# 两道闸必须同时在，缺一个都会变成长期伤害：
#   · RESERVE_MAX  —— 不夹取的话 reserve=100000 会让补货线程长期钉在一个目标上，
#                     既饿死其他目标，又变成对同一站点的持续高频挑战（抬风控分）。
#   · RESERVE_TTL_S —— 预约**必须会过期**。调用方崩了/放弃了却留下永久预约，
#                     服务会永远维持超深池子并持续铸造，白烧挑战额度还拉高画像。
RESERVE_MAX = max(0, int(os.environ.get("MINT_RESERVE_MAX", "32")))
RESERVE_TTL_S = float(os.environ.get("MINT_RESERVE_TTL_S", "120"))
# 不传 `?reserve=` 时的缺省预约数。为什么默认 2 而不是 0：
#   取 token 的调用方**几乎从不只取一个** —— 批量任务、重试、多步流程都会接着再要。
#   默认 0 等于每次都假装"这是最后一个"，于是池刚被取空就停止补货，下一个请求必走 live
#   （1.7~4s 串行现铸）。默认 2 让"取完还会再来"成为默认假设，把常见情形的等待前移到
#   调用方处理当前 token 的那段时间里。
# 为什么不更大：预约会抬高池深并驱动真实铸造，默认值是**所有调用方都会付的成本**。
#   2 只覆盖"再要一两个"的常见情形；真要批量的调用方应显式传 `reserve=N` 表达意图。
# 设 0 即恢复旧行为（不预约）。
RESERVE_DEFAULT = max(0, int(os.environ.get("MINT_RESERVE_DEFAULT", "2")))
# 有预约在身时补货之间的喘息（秒，随机区间的下界与跨度）。比无预约时的 2-8s 短：
# 预约代表**真实且已知**的近期需求，池空后调用方会退化到 live 路径（那条路径根本不喘息），
# 所以缩短这里其实比让池跑干更低风险。仍然保持随机，避免形成机械节拍。
RESERVE_BREATHER_MIN = float(os.environ.get("MINT_RESERVE_BREATHER_MIN_S", "1"))
RESERVE_BREATHER_SPAN = float(os.environ.get("MINT_RESERVE_BREATHER_SPAN_S", "3"))
MAX_WARM_TARGETS = max(1, int(os.environ.get("MAX_WARM_TARGETS", "8")))
# 同时在铸造入口内的请求数上限（详见下方 _GATE）。0 = 不限制。
MAX_INFLIGHT = max(0, int(os.environ.get("MINT_MAX_INFLIGHT", "3")))
TARGETS_FILE = os.environ.get("TARGETS_FILE", "targets.yaml")
DEFAULT_TARGET_NAME = os.environ.get("MINTER_DEFAULT_TARGET", "") or None
# 身份标签 —— 只进 /metrics 的标签集与 /healthz.identity，**不参与任何铸造逻辑**。
#
# 为什么需要：`render_metrics` 每目标一条序列，但多实例按出口部署后，几个实例导出的序列
# 标签集完全相同 ⇒ Prometheus 里无法把 interactive 占比按出口拆开，而"换出口有没有用"
# 恰恰只能靠这个拆分来判读。没有这层标签，住宅出口实验拿不到可比较的结论。
#
# 两个维度刻意分开：instance 是"哪个进程"（排障用，通常给主机名/容器名），
# egress 是"哪条出口画像"（结论用，同一出口的多个实例应共享同一个 egress 值）。
INSTANCE_ID = os.environ.get("MINTER_INSTANCE_ID", "").strip()
EGRESS_LABEL = os.environ.get("MINTER_EGRESS", "").strip()
# 兼容 aivideomaker 的单目标部署写法：把 SITEKEY/LIGHT_URL 直接给在 env 里。
# 这条只为"平滑迁移"，新部署请用注册表文件。
LEGACY_SITEKEY = os.environ.get("SITEKEY", "").strip()
LEGACY_LIGHT_URL = os.environ.get("LIGHT_URL", "").strip()
LEGACY_TARGET_NAME = os.environ.get("LEGACY_TARGET_NAME", "default")

TZ_ENV = os.environ.get("TZ", "").strip()
TZ_DEFAULT = "Asia/Shanghai"
LOOPBACK_HOSTS = frozenset({"127.0.0.1", "::1", "localhost"})


def env_flag(raw: str) -> bool:
    """环境变量字面值 → 布尔。**只认开启值**（安全开关，判错的代价不对称：

    把关闭误判成开启会直接暴露铸造能力；反之只是让人多设一个 MINTER_KEY。
    `0`/`false`/`no`/`off`/空串/拼错的值一律判为关闭。）
    """
    return str(raw).strip().lower() in ("1", "true", "yes", "on")


ALLOW_INSECURE = env_flag(os.environ.get("MINTER_ALLOW_INSECURE", ""))
# widget 复用（turnstile.reset 而非重开页）。默认开：省掉每次 ~1.5s 开页 + 脚本冷启。
# 设 0 回到"每次新页新 widget"的老形态，用于隔离"复用是否引入新失败形态"。
REUSE_WIDGET = env_flag(os.environ.get("MINT_REUSE_WIDGET", "1"))


def tz_state() -> dict:
    """生效时区的**可验证**三元组：env 原文 / libc 偏移 / 该名字在镜像里存不存在。

    三者分开报，因为它们会互相独立地出错，且现象都是"铸造失败"：
      · `env` 为空         ⇒ 部署漏配；
      · `offset` 是 +0000  ⇒ 名字递到了 libc，但镜像没有对应 zoneinfo ⇒ 静默按 UTC；
      · `zoneinfo_present` ⇒ 上面那条的归因（缺 tzdata，还是时区名写错）。
    """
    name = TZ_ENV or TZ_DEFAULT
    return {
        "env": TZ_ENV or None,
        "assumed_default": TZ_DEFAULT if not TZ_ENV else None,
        "name": name,
        "offset": time.strftime("%z") or None,
        "zoneinfo_present": os.path.exists("/usr/share/zoneinfo/" + name),
    }


def bind_guard_error(bind_host: str, key: str, allow_insecure: bool = False) -> str | None:
    """非回环绑定且无凭据 ⇒ 返回拒绝启动的原因；`None` 表示放行。

    为什么**拒绝启动**而不是打条告警：本服务的产物能直接过掉站点的提交闸门，
    等同于"不限次数的免费提交额度"。容器形态下 `BIND_HOST=0.0.0.0` + host 网络
    ⇒ 0.0.0.0 就是宿主公网地址，匿名一个 curl 就能把池子刷干、也把账号推到风控面前。
    """
    if key.strip() or bind_host.strip() in LOOPBACK_HOSTS or allow_insecure:
        return None
    return (
        f"拒绝启动：BIND_HOST={bind_host!r} 是非回环地址，但 MINTER_KEY 未设置 ——\n"
        "  任何能访问该端口的人都能**匿名领取 Turnstile token**，等于把「过闸能力」分发出去。\n"
        "  出路一（推荐）：设 MINTER_KEY=<随机串>，调用方带 X-Minter-Key 头。\n"
        "  出路二（仅私有网络/已有外层 ACL）：显式设 MINTER_ALLOW_INSECURE=1 自担风险。\n"
        "  出路三（仅本机自用）：BIND_HOST=127.0.0.1（默认值）。"
    )


def backoff_delay(fail_streak: int) -> float:
    """连续失败后的补货退避：8s 起步指数翻倍、封顶 300s，再叠 ±25% 抖动。

    固定短周期重试会把"卡死的铸造"变成宿主上无休止的 Chrome 杀/起循环
    （E2E-AVM-004 实测形态：一个周期 ~2.5 分钟，宿主背着上千个 chrome 进程却永无产出）。
    """
    base = min(300.0, 8.0 * (2 ** max(0, fail_streak - 1)))
    return base * (0.75 + random.random() * 0.5)


# -------------------------------------------------------------------- 观测 --
# 滚动窗口容量：只保留最近 N 次铸造的结果标签。
#
# 为什么要滚动窗口，而不是只看累计 stats：累计计数对**趋势**不敏感。服务跑一周后
# minted 上万，今天即使全军覆没，累计通过率也只挪动一两个百分点 —— 而"通过率在掉"
# 恰恰是判断"是否被针对"的唯一早期信号。累计值回答"历史总体如何"，窗口回答
# "**现在**如何"，后者才是选型与告警的依据。
OUTCOME_WINDOW = max(20, int(os.environ.get("OUTCOME_WINDOW", "50")))


def outcome_summary(window) -> dict:
    """把最近若干次铸造结果压成可判读的摘要：窗口通过率 + 失败原因分布。

    分桶键直接沿用 `engine.failure_reason()` 的标签（timeout / interactive /
    error / exception / no-result / …），因为那套分类已经是"对症下药"的口径：

      · interactive 占比升高 ⇒ 挑战被升级成"要人点"，是**出口/画像信誉下滑**的信号，
        加预算和重试都没用，该换出口或重铸 profile。
      · error 占比升高      ⇒ CF 主动拒绝，是最接近"被针对"的形态。
      · timeout 占比升高    ⇒ 多半是自身链路/资源问题，而非风控。

    把它们混在一个 failed 计数里，就等于把"该换出口"和"该查网络"两件事混成一件 ——
    这正是当前无法判断"要不要动反检测"的根因。
    """
    items = list(window)
    if not items:
        return {"window": 0, "pass_rate": None, "reasons": {}}
    reasons: dict[str, int] = {}
    ok = 0
    for r in items:
        if r == "ok":
            ok += 1
        else:
            reasons[r] = reasons.get(r, 0) + 1
    return {
        "window": len(items),
        "pass_rate": round(ok / len(items), 3),
        "reasons": dict(sorted(reasons.items(), key=lambda kv: -kv[1])),
    }


# --------------------------------------------------------------- 降级判定 --
# 判定前的最小样本量。窗口里只有两三次结果时，一次失败就能把通过率拉到 0.5 以下 ——
# 那不是"被针对"，那是样本太少。没有这道闸，服务每次重启都会先自曝一轮假 degraded。
DEGRADED_MIN_SAMPLES = max(5, int(os.environ.get("DEGRADED_MIN_SAMPLES", "10")))

# 窗口通过率低于此值 ⇒ 降级。0.5 是"还能用但明显不对"的分界。
DEGRADED_PASS_RATE = float(os.environ.get("DEGRADED_PASS_RATE", "0.5"))

# interactive 占比高于此值 ⇒ 降级，**即使通过率还不错**。
# 这条独立于通过率：挑战被升级成"要人点"时，加重试、加预算都无效，是出口/画像信誉
# 下滑的特征信号。等它把通过率也拖下去才告警就太晚了。
DEGRADED_INTERACTIVE_RATE = float(os.environ.get("DEGRADED_INTERACTIVE_RATE", "0.3"))

# 连续失败达到此值 ⇒ 降级，**不受最小样本闸门约束**。
# 这条是为了反应速度：刚被掐时窗口里可能只有 5-6 条结果，通过率判定还在等样本，
# 但"连着 5 次全废"本身已经是足够确定的信号 —— 它不依赖样本量，因为连续性排除了
# "小样本抖动"这种解释（20% 失败率连中 5 次的概率约万分之三）。
DEGRADED_FAIL_STREAK = max(3, int(os.environ.get("DEGRADED_FAIL_STREAK", "5")))


def degraded_state(summary: dict, fail_streak: int = 0) -> dict:
    """把 outcome_summary 的摘要判成"是否该摘流量"，附可读理由。

    纯函数、只吃摘要 dict：这样判定逻辑能被单测完全覆盖，不必起 Chrome、不必真铸造
    —— 在"零消耗"约束下，这是唯一能把防封策略验证到位的形态。

    与 `ok` 字段的分工（**不要合并**）：
      · ok=False       ⇒ 服务坏了（Chrome 没起来），重启能救；
      · degraded=True  ⇒ 服务是活的、铸造在跑，但**效果在掉** —— 重启救不了，
                          该换出口/改指纹。编排层对这两种应做不同动作，混成一个
                          字段就等于把"重启我"和"别给我流量"混成一件事。
    """
    window = summary.get("window") or 0

    # 连续失败先判，且**故意放在最小样本闸门之前**：
    # 闸门防的是"小样本里比率抖动"，而连续性本身已经排除了抖动这个解释。
    # 放在闸门之后就等于"刚被掐时还要再烧够 10 次才肯报" —— 白烧的正是要省的那批。
    if fail_streak >= DEGRADED_FAIL_STREAK:
        return {
            "degraded": True,
            "reasons": [
                f"fail_streak {fail_streak} >= {DEGRADED_FAIL_STREAK} "
                "（连续失败 ⇒ 刚崩或刚被掐，建议立即摘流量）"
            ],
            "samples": window,
            "min_samples": DEGRADED_MIN_SAMPLES,
        }

    if window < DEGRADED_MIN_SAMPLES:
        return {"degraded": False, "reasons": [], "samples": window,
                "min_samples": DEGRADED_MIN_SAMPLES}
    reasons: list[str] = []
    pass_rate = summary.get("pass_rate")
    if pass_rate is not None and pass_rate < DEGRADED_PASS_RATE:
        reasons.append(f"pass_rate {pass_rate:.3f} < {DEGRADED_PASS_RATE:.3f}")
    inter = (summary.get("reasons") or {}).get("interactive", 0)
    inter_rate = inter / window
    if inter_rate > DEGRADED_INTERACTIVE_RATE:
        reasons.append(
            f"interactive {inter_rate:.3f} > {DEGRADED_INTERACTIVE_RATE:.3f} "
            "（挑战被升级为需人工交互 ⇒ 建议换出口/重铸 profile）"
        )
    return {"degraded": bool(reasons), "reasons": reasons, "samples": window,
            "min_samples": DEGRADED_MIN_SAMPLES}


# ------------------------------------------------------------------ 目标状态 --
@dataclass
class TargetState:
    """一个目标的全部运行期状态。**池子与热页是分开的生命周期**：

    · 池子（tokens）随 TTL 过期，与页面无关；
    · 热页（page）在 LRU 超额时被回收 —— 回收热页不丢池子，下次用时重建（约 1.5s）。
    """

    target: Target
    pool: list[tuple[str, float]] = field(default_factory=list)
    stats: dict = field(
        default_factory=lambda: {"minted": 0, "failed": 0, "served": 0, "reused": 0}
    )
    last_error: str | None = None
    last_failure: dict | None = None
    # 最近 OUTCOME_WINDOW 次铸造的结果标签（"ok" 或 failure_reason 的枚举）。
    # 只存标签、不存 token 与页面状态：这份数据会进 /healthz，不能含敏感内容。
    outcomes: deque = field(default_factory=lambda: deque(maxlen=OUTCOME_WINDOW))
    page: tuple | None = None          # (targetId, sessionId)
    warmed: bool = False               # 真的铸出过 token（≠ 页面就绪）
    fail_streak: int = 0
    last_used: float = field(default_factory=time.time)
    # 预约账本：[(剩余个数, 登记时刻)]。**不能合并成一个整数**——那样无法按登记时间
    # 分别过期，一条新预约会把一条早该作废的旧预约一起续命。
    reservations: list[list[float]] = field(default_factory=list)

    def ttl(self) -> float:
        """该目标的 token 寿命：每目标覆盖优先，否则全局 TTL_S。

        TTL 是**站点侧属性**（各站点校验窗口不同），所以必须允许按目标配，
        否则只能按最保守的站点设全局值，宽松站点的 token 被白白提前丢弃。
        """
        return self.target.ttl_s if self.target.ttl_s is not None else TTL_S

    def base_pool(self) -> int:
        """基础池深：每目标覆盖优先。

        🔴 显式配了 `pool_target` 就**视为要预热**，不再要求同时写 `prewarm: true` ——
        "配了池深却因为忘写 prewarm 而池深为 0"是纯粹的陷阱，且两种写法表达的是同一意图。
        反之 `pool_target: 0` 是明确的"不要预热"，即使 prewarm 为 true 也尊重它
        （否则没有任何办法为单个目标关掉预热）。
        """
        if self.target.pool_target is not None:
            return self.target.pool_target
        return POOL_TARGET if self.target.prewarm else 0

    def prune(self) -> None:
        now = time.time()
        ttl = self.ttl()
        self.pool = [p for p in self.pool if now - p[1] < ttl]
        # 预约同池子一起过期：调用方可能崩了或放弃了，留下的预约不能永久生效。
        self.reservations = [
            r for r in self.reservations if r[0] > 0 and now - r[1] < RESERVE_TTL_S
        ]

    def reserved(self) -> int:
        """当前仍生效的预约总数（调用方须先 prune）。"""
        return int(sum(r[0] for r in self.reservations))

    def want(self) -> int:
        """目标池深 = 基础池深 + 预约。未开预热的目标基础池深为 0，但**预约仍然生效**——
        否则「按需现铸」的目标永远享受不到批量提前量，而它们恰恰最需要。
        """
        return self.base_pool() + self.reserved()

    def consume_reservation(self) -> None:
        """取走一个 token 时核销一个预约额度（最早登记的先核销）。"""
        for r in self.reservations:
            if r[0] > 0:
                r[0] -= 1
                break
        self.reservations = [r for r in self.reservations if r[0] > 0]

    def health(self) -> dict:
        recent = outcome_summary(self.outcomes)
        return {
            "sitekey": self.target.sitekey,
            "light_url": self.target.light_url,
            "prewarm": self.target.prewarm,
            "warm_page": self.page is not None,
            "warmed": self.warmed,
            "pool": {
                "size": len(self.pool),
                # target 是**动态**的：基础池深 + 生效中的预约。判读 pool_size 时必须
                # 跟它比，跟固定的 POOL_TARGET 比会把"预约期内正常的欠额"误读成故障。
                "target": self.want(),
                "base": self.base_pool(),
                "reserved": self.reserved(),
                # 报**实际生效**的 TTL（可能被每目标覆盖），不是全局值 ——
                # 报全局值会让"为什么这个目标的 token 活得更久"永远查不出来。
                "ttl_s": self.ttl(),
                "overrides": {                      # 显式暴露覆盖，便于确认配置真的生效了
                    "pool_target": self.target.pool_target,
                    "ttl_s": self.target.ttl_s,
                },
            },
            "stats": dict(self.stats),
            # 累计 stats 回答"历史总体"，recent 回答"现在" —— 判断趋势看后者。
            "recent": recent,
            # 目标级降级：多目标下不同目标的出口画像可能天差地别，全局一个标志会把
            # "某一个目标被针对"稀释掉。编排层要能按目标摘流量，判定就得落到目标上。
            "degraded": degraded_state(recent, self.fail_streak),
            "fail_streak": self.fail_streak,
            "last_used_age_s": int(time.time() - self.last_used),
            "last_error": self.last_error,
            "last_failure": self.last_failure,
        }


# --------------------------------------------------------------------- 核心 --
_GL_STATE = {
    "browser": None,
    "ready": False,
    "last_error": None,
    "spawned_at": time.time(),
    # Chrome 进程启动次数（首次启动记 1）。**持续增长 = 杀/起循环**，这种形态下
    # ready 会在 true/false 之间抖，只看 ready 会把"一直在重启"读成"偶发失败"。
    "chrome_starts": 0,
    # 当前 Chrome 进程起来的时刻（每次重启刷新）。与 spawned_at（服务启动时刻）区分开：
    # 原先 uptime_s 只看 spawned_at，Chrome 重启后仍报一个很大的值 —— 看着很稳，实则在抖。
    "chrome_spawned_at": None,
}
_STATES: dict[str, TargetState] = {}

# 🔴 两把锁**必须分开**：铸造锁可能被持有数十秒到两分钟（冷启动/卡死），状态锁毫秒级。
#    共用一把会让 /healthz 被正在进行的铸造阻塞 ⇒ 探活超时 —— 服务看着"挂了"，其实只是忙。
_MINT_LOCK = threading.Lock()
_STATE_LOCK = threading.RLock()
_WAKE = threading.Event()

# 入口闸门：ThreadingHTTPServer 每连接开一个线程，而铸造被 _MINT_LOCK 串行化。
# 没有闸门时 N 个并发请求会全部挂在锁上各等几分钟，调用方超时重试又继续堆线程
# （线程泄漏 + 队列越排越长）。超过闸门直接快速失败（503 + Retry-After），让调用方
# **立刻**知道要退避，这比"排队几分钟后超时"对上游友好得多。
# MINT_MAX_INFLIGHT=0 ⇒ 不限制（留给压测对照，别在生产用）。
_GATE: threading.BoundedSemaphore | None = (
    threading.BoundedSemaphore(MAX_INFLIGHT) if MAX_INFLIGHT > 0 else None
)
_inflight = 0                 # 仅观测用；读写都在 _STATE_LOCK 里


def _gate_acquire() -> bool:
    """非阻塞抢一个铸造名额。True=拿到，False=满了（调用方应立刻 503）。

    刻意用 blocking=False：阻塞等待会把 HTTP 线程按在这里，等到调用方超时都还在排队，
    排到了也已经没人要那个 token —— 白烧一次挑战，还抬高风控分。
    """
    global _inflight
    if _GATE is not None and not _GATE.acquire(blocking=False):
        return False
    with _STATE_LOCK:
        _inflight += 1
    return True


def _gate_release() -> None:
    global _inflight
    with _STATE_LOCK:
        _inflight = max(0, _inflight - 1)
    if _GATE is not None:
        _GATE.release()


class Browser:
    """**全局唯一**的 Chrome + 每目标一个热页（LRU 有界）。

    为什么不每目标一个 Chrome：几百个目标 × 每个 Chrome 数百 MB = 不可能。
    一个 Chrome 可以开很多 tab，而 CF 的 sitekey 是按页面 origin 生效的 ⇒ 天然隔离。
    """

    def __init__(self) -> None:
        self.bc = None

    def ensure(self) -> None:
        if self.bc is not None:
            return
        M.start_chrome()
        ver = M.http_json(f"http://127.0.0.1:{M.PORT}/json/version")
        self.bc = M.CDP(ver["webSocketDebuggerUrl"])
        with _STATE_LOCK:
            _GL_STATE["ready"] = True
            _GL_STATE["browser"] = ver.get("Browser")
            # 记在 Chrome 真起来之后：起失败不该计数，否则分不清"重启多次"和"一直起不来"。
            _GL_STATE["chrome_starts"] = int(_GL_STATE["chrome_starts"]) + 1
            _GL_STATE["chrome_spawned_at"] = time.time()

    def reset(self) -> None:
        self.bc = None
        with _STATE_LOCK:
            _GL_STATE["ready"] = False
            for st in _STATES.values():
                st.page = None

    def open_page(self, st: TargetState) -> None:
        """为某目标开/换一个干净热页（轻量同源页 + 注入 CF api.js）。"""
        assert self.bc is not None
        if st.page is not None:
            try:
                self.bc.call("Target.closeTarget", targetId=st.page[0])
            except Exception:  # noqa: BLE001 - 关不掉旧页不是错误，继续开新的
                pass
            st.page = None
        st.page = M.open_warm_page(self.bc, light_url=st.target.light_url)

    def page_for(self, st: TargetState):
        """拿到该目标的热页，必要时新建；超额时按 LRU 回收别人的热页。"""
        if st.page is not None:
            return st.page
        self._evict_if_needed(keep=st.target.name)
        self.open_page(st)
        return st.page

    def close_page(self, page: tuple) -> None:
        """回收一个热页。给 sync_registry 用：注册表改/删目标时旧 tab 必须关掉。

        Chrome 没起来（bc is None）时直接返回——那种情况下 tab 本来就不存在。
        """
        if self.bc is None:
            return
        try:
            self.bc.call("Target.closeTarget", targetId=page[0])
        except Exception:  # noqa: BLE001 - 关不掉只是泄漏一个 tab，不能反过来影响调用方
            pass

    def _evict_if_needed(self, *, keep: str) -> None:
        warm = [s for s in _STATES.values() if s.page is not None and s.target.name != keep]
        if len(warm) < MAX_WARM_TARGETS:
            return
        # 回收最久未用的热页（只回收页面，池子留着）
        for victim in sorted(warm, key=lambda s: s.last_used)[: len(warm) - MAX_WARM_TARGETS + 1]:
            try:
                if self.bc is not None:
                    self.bc.call("Target.closeTarget", targetId=victim.page[0])
            except Exception:  # noqa: BLE001
                pass
            victim.page = None


CORE = Browser()


def _state_for(name: str) -> TargetState:
    st = _STATES.get(name)
    if st is None:
        raise KeyError(name)
    return st


def mint_live(st: TargetState) -> str:
    """现铸一个（串行化；失败按目标记账并抛出）。"""
    with _MINT_LOCK:
        CORE.ensure()
        page = CORE.page_for(st)
        budget = M.MINT_TIMEOUT_MS if st.warmed else M.COLD_MINT_TIMEOUT_MS
        # widget 复用只在**这一页已经铸成过**时才有意义（首铸必然 NO_WIDGET，白跑一次求值）。
        # MINT_REUSE_WIDGET=0 可整体关掉，用于排查"复用是否引入了新的失败形态"。
        tok, _dt, val = M.mint_on(CORE.bc, page[1], 0, timeout_ms=budget, sitekey=st.target.sitekey,
                                  reuse=REUSE_WIDGET and st.warmed)
        if isinstance(val, dict) and val.get("ok") and (val.get("state") or {}).get("reused"):
            with _STATE_LOCK:
                st.stats["reused"] = st.stats.get("reused", 0) + 1
        if not tok:
            reason = M.failure_reason(val)
            state = val.get("state") if isinstance(val, dict) else None
            with _STATE_LOCK:
                st.stats["failed"] += 1
                st.outcomes.append(reason)
                st.last_error = f"mint failed: {reason}" + (f" state={state}" if state else "")
                # 失败分类 + 页面内状态采样：排障不必再翻容器日志
                #   interactive ⇒ 挑战要人点（低信誉出口/profile 的典型形态），等预算没意义
                #   timeout     ⇒ 打满预算仍无回调（state 里能看出 iframe 是否挂上）
                #   error       ⇒ CF 主动报错，带原始错误码
                st.last_failure = {
                    "reason": reason,
                    "why": val.get("why") if isinstance(val, dict) else None,
                    "state": state,
                    "at": time.time(),
                }
            # ★ 失败后**保留浏览器**、只换页面：见类文档纪律 3。
            try:
                CORE.open_page(st)
            except Exception as e:  # noqa: BLE001 - 换页失败 ⇒ 降级整体 reset
                with _STATE_LOCK:
                    st.last_error = f"reload_page failed: {type(e).__name__}: {e}"[:200]
                CORE.reset()
            raise RuntimeError(f"mint failed: {reason}")
        st.warmed = True        # 真的铸出过（heat ≠ ready）
        with _STATE_LOCK:
            st.stats["minted"] += 1
            st.outcomes.append("ok")
        return tok


def take_token(st: TargetState) -> dict:
    """优先池子（仅 prewarm 目标有池）；池空则现铸。"""
    with _STATE_LOCK:
        st.prune()
        st.last_used = time.time()
        if st.pool:
            tok, at = st.pool.pop(0)
            st.stats["served"] += 1
            # 核销预约：预约表达的是"还要 N 个"，取走一个就该少一个。不核销的话
            # 池深目标会在整个 TTL 窗口里虚高，补货线程持续超铸。
            st.consume_reservation()
            _WAKE.set()
            return {
                "token": tok,
                "source": "pool",
                "age_ms": int((time.time() - at) * 1000),
                "target": st.target.name,
            }
    t0 = time.time()
    tok = mint_live(st)
    with _STATE_LOCK:
        st.stats["served"] += 1
        # live 路径同样核销：它也是一次已交付。漏掉这里会让预约在池空期间只增不减，
        # 等池子恢复后补货线程按虚高的目标继续超铸。
        st.consume_reservation()
    return {
        "token": tok,
        "source": "live",
        "age_ms": int((time.time() - t0) * 1000),
        "target": st.target.name,
    }


def refill_loop(registry: TargetRegistry) -> None:
    """补货流水线：**只给 `prewarm: true` 的目标**维持池子，每次补一个、轮转。

    为什么只补 prewarm 目标：几百个目标不可能都预热（那等于持续对几百个站点发挑战，
    既吃 CPU 也**主动抬高风控分**——本文件开头就写了池是 opt-in）。默认按需现铸。
    """
    while True:
        _WAKE.wait(timeout=1.0)
        _WAKE.clear()
        try:
            with _STATE_LOCK:
                for st in _STATES.values():
                    st.prune()
                # 候选 = 预热目标 ∪ 有生效预约的目标。后者让"按需现铸"的目标也能吃到
                # 批量提前量（prewarm=false 只表示"不常态占额度"，不表示"拒绝被预约"）。
                candidates = [
                    s for s in _STATES.values() if s.target.prewarm or s.reservations
                ]
                need = [s for s in candidates if len(s.pool) < s.want()]
                # 有预约的排前面：预约是**已知的真实需求**，而基础池深只是预备量。
                # 同组之内仍按 last_used 轮转，避免单个目标长期霸占补货线程。
                need.sort(key=lambda s: (0 if s.reservations else 1, s.last_used))
                has_reservation = bool(need and need[0].reservations)
            if not need:
                continue
            st = need[0]
            tok = mint_live(st)
            with _STATE_LOCK:
                st.pool.append((tok, time.time()))
                st.fail_streak = 0
            # 每次成功铸造之间随机喘息，避免形成"每 3.5 秒一个挑战"的机械流量。
            # 有预约时缩短（但不取消）喘息 —— 理由见 RESERVE_BREATHER_MIN 处的说明。
            if has_reservation:
                time.sleep(RESERVE_BREATHER_MIN + random.random() * RESERVE_BREATHER_SPAN)
            else:
                time.sleep(2 + random.random() * 6)
        except (Exception, SystemExit) as e:  # noqa: BLE001
            # ⚠️ **必须连 SystemExit 一起接**：ensure() 起不来 Chrome 时抛 SystemExit，
            #    它是 BaseException 的子类 —— 只 catch Exception 会让补货线程**静默死亡**，
            #    表现为 ready=false / minting=false / last_error=null。
            name = "?"
            try:
                name = st.target.name          # type: ignore[possibly-undefined]
            except Exception:  # noqa: BLE001
                pass
            with _STATE_LOCK:
                _GL_STATE["last_error"] = f"[{name}] {type(e).__name__}: {e}"[:200]
                try:
                    st.fail_streak += 1            # type: ignore[possibly-undefined]
                except Exception:  # noqa: BLE001
                    pass
            time.sleep(backoff_delay(getattr(st, "fail_streak", 1) if "st" in dir() else 1))


def reload_loop(registry: TargetRegistry) -> None:
    """注册表热重载（2s 一跳）+ 把注册表变化同步进运行期状态。"""
    while True:
        try:
            if registry.maybe_reload():
                sync_registry(registry)
        except Exception as e:  # noqa: BLE001 - 重载失败不能拖垮服务
            with _STATE_LOCK:
                _GL_STATE["last_error"] = f"registry reload: {type(e).__name__}: {e}"[:200]
        time.sleep(2.0)


def _page_identity(tgt) -> tuple:
    """决定"热页还能不能用"的字段。只有这些变了才值得作废热页（重开一页 ~1.5s）。"""
    return (tgt.sitekey, tgt.light_url)


def sync_registry(registry: TargetRegistry) -> None:
    """把注册表里的目标同步成 TargetState（新增即建；删除即撤，热页一并关掉）。

    两条纪律：
      1. **只有 sitekey/light_url 真的变了才作废热页**。改一个目标不该让其余目标
         集体重付开页成本（历史实现里"任何变化全量作废"是个放大器）。
      2. 作废或删除前先回收旧 tab。不关就是泄漏——Chrome 里的 tab 只增不减，
         几百个目标热重载几轮就能把内存吃穿。
    """
    stale: list = []                      # 待关的旧页，收集到锁外再关（关页要走 CDP，可能慢）
    with _STATE_LOCK:
        wanted = {t.name: t for t in registry.all_targets()}
        for name, tgt in wanted.items():
            st = _STATES.get(name)
            if st is None:
                _STATES[name] = TargetState(target=tgt)
                continue
            changed = _page_identity(st.target) != _page_identity(tgt)
            st.target = tgt
            if changed and st.page is not None:
                stale.append(st.page)
                st.page = None
                st.warmed = False         # 新 sitekey 的页还没热过，别谎报
        for name in list(_STATES):
            if name not in wanted:
                dead = _STATES.pop(name, None)
                if dead is not None and dead.page is not None:
                    stale.append(dead.page)
    for page in stale:
        CORE.close_page(page)             # 自身已吞异常：回收失败只是泄漏一个 tab


def merge_outcomes(states) -> dict:
    """把多个目标的窗口合成一份全局摘要。

    🔴 **按原始计数合并，不对各目标的 pass_rate 取平均**：目标之间样本量可以差一个
    数量级（一个预热目标铸了 50 次，一个冷目标只铸了 2 次）。取平均会让那个只有 2 次
    样本的目标拥有和 50 次样本同等的话语权 —— 一次偶发失败就能把全局判成 degraded。
    """
    merged: deque = deque()
    for st in states:
        merged.extend(st.outcomes)
    return outcome_summary(merged)


def health(registry: TargetRegistry, *, with_targets: bool = True) -> dict:
    """只用状态锁（毫秒级）。铸造锁可能被持有数十秒，绝不能在这里等。"""
    with _STATE_LOCK:
        for st in _STATES.values():
            st.prune()
        st_global = int(time.time() - _GL_STATE["spawned_at"])
        recent_all = merge_outcomes(_STATES.values())
        payload = {
            "ok": bool(_GL_STATE["ready"]),
            "minting": _MINT_LOCK.locked(),
            "browser": _GL_STATE["browser"],
            "ready": _GL_STATE["ready"],
            "warmed": any(s.warmed for s in _STATES.values()),
            "render_timeout_ms": {"cold": M.COLD_MINT_TIMEOUT_MS, "normal": M.MINT_TIMEOUT_MS},
            "tz": tz_state(),
            "pool": {
                "size": sum(len(s.pool) for s in _STATES.values()),
                # 全局**默认**值。带覆盖的目标其实际生效值在 targets[].pool.{base,ttl_s}，
                # 名字带 default_ 是为了防止有人把这里当成"所有目标都是这个值"。
                "default_per_target": POOL_TARGET,
                "default_ttl_s": TTL_S,
                "reserve_default": RESERVE_DEFAULT,
                # 实际在预热的目标 = base_pool() > 0，而不是 `prewarm` 标志 ——
                # 只配了 pool_target 的目标同样在预热，按标志筛会把它们漏掉。
                "prewarm_targets": sorted(
                    s.target.name for s in _STATES.values() if s.base_pool() > 0
                ),
                # 有覆盖的目标单独列出：排查"池深/TTL 跟我以为的不一样"时第一眼就能看到。
                "overridden_targets": sorted(
                    s.target.name for s in _STATES.values()
                    if s.target.pool_target is not None or s.target.ttl_s is not None
                ),
                # 全局在途预约。持续大于 0 且 size 长期上不来 ⇒ 预约量超出稳态产能，
                # 该横向扩实例而不是继续加大 reserve。
                "reserved": sum(s.reserved() for s in _STATES.values()),
                "reserve_max": RESERVE_MAX,
            },
            # 全局窗口与降级标志。**与 ok 分开读**：ok=False 是"重启我"，
            # degraded=True 是"别给我流量，但重启没用" —— 见 degraded_state 的说明。
            "recent": recent_all,
            # fail_streak 取各目标的**最大值**而不是求和或平均：连续失败是"某个目标正在
            # 被掐"的信号，求和会让两个各失败 3 次的目标凑出假的 6 连败，平均则会被
            # 健康目标稀释掉。要按目标摘流量就读目标级的 degraded。
            "degraded": degraded_state(
                recent_all,
                max((s.fail_streak for s in _STATES.values()), default=0),
            ),
            "uptime_s": st_global,
            # chrome_starts 持续增长 = 杀/起循环；配合 chrome_uptime_s（当前进程存活时长）看：
            # starts 大而 chrome_uptime_s 一直很小，就是在抖，别被服务级 uptime_s 的大数字骗了。
            "chrome_starts": _GL_STATE["chrome_starts"],
            "chrome_uptime_s": (
                int(time.time() - _GL_STATE["chrome_spawned_at"])
                if _GL_STATE["chrome_spawned_at"] else None
            ),
            "inflight": {"current": _inflight, "max": MAX_INFLIGHT or None},
            "registry": registry.state(),
            "targets_count": len(_STATES),
            # 纯标识，不含凭据；未设则为 None（让 /healthz 明确显示"这实例没标身份"，
            # 比给空串更容易在多实例排障时发现漏配）。
            "identity": {"instance": INSTANCE_ID or None, "egress": EGRESS_LABEL or None},
            "last_error": _GL_STATE["last_error"],
        }
        if with_targets:
            payload["targets"] = {name: st.health() for name, st in sorted(_STATES.items())}
        return payload


# --------------------------------------------------------------- 指标导出 --
# 失败原因标签集合**固定导出**（哪怕本窗口计数为 0）。原因：Prometheus 里
# "序列消失"与"值为 0"语义完全不同 —— 序列消失时 rate() 返回空，告警规则
# `interactive 占比 > 阈值` 会静默不触发而不是判假。把标签集钉死，让 0 是个真实的 0。
METRIC_REASONS = ("timeout", "interactive", "error", "exception", "no-result")


def _esc(v: str) -> str:
    """Prometheus 标签值转义：反斜杠、双引号、换行。目标名来自注册表（用户可控），
    不转义会让一个带引号的名字**破坏整份导出**（而不只是那一行）。"""
    return str(v).replace("\\", "\\\\").replace('"', '\\"').replace("\n", "\\n")


def _fmt(v) -> str | None:
    """布尔 → 1/0；None → None（调用方跳过该行，不写 NaN）。"""
    if v is None:
        return None
    if isinstance(v, bool):
        return "1" if v else "0"
    return repr(float(v)) if isinstance(v, float) else str(v)


def identity_labels(payload: dict) -> str:
    """把 `identity` 载荷转成 Prometheus 标签片段（不含花括号），空则返回空串。

    做成纯函数以便零成本单测。只导出**非空**字段：给 `egress=""` 这种空值序列
    会让 Prometheus 里多一个永远匹配不上的维度，比不导更难查。
    """
    ident = payload.get("identity") or {}
    parts = [
        f'{k}="{_esc(str(ident[k]))}"'
        for k in ("instance", "egress")
        if str(ident.get(k) or "").strip()
    ]
    return ",".join(parts)


def _with_identity(lbl: str, ident: str) -> str:
    """把身份标签并入既有标签片段。`lbl` 是 `""` 或 `{a="b"}` 两种形态之一。"""
    if not ident:
        return lbl
    if not lbl:
        return "{" + ident + "}"
    return "{" + ident + "," + lbl[1:-1] + "}"


def render_metrics(payload: dict) -> str:
    """把 `health()` 的载荷转成 Prometheus 文本格式（OpenMetrics 兼容子集）。

    做成**纯函数**（吃 dict、吐 str）而不是直接读全局状态：它因此能在零消耗前提下
    被完整单测，且与锁无关 —— 采集路径不会把状态锁多持有一次。

    两条口径：
      1. `pass_rate` 在空窗口是 `None`（不是 0.0）⇒ **跳过该行**而非写 0。
         写 0 会让每次重启都自触发"通过率暴跌"告警，这正是 health 里特意区分 None 的原因。
      2. 每目标一条序列（`target=` 标签），因为不同目标的出口画像可能天差地别；
         只导全局聚合会把"某一个目标被针对"稀释掉 —— 与 degraded 落到目标级同理。
    """
    out: list[str] = []
    # 在 emit 内统一注入身份标签：所有序列（含每目标那批）都会带上，
    # 不必在每个调用点重复拼 —— 漏一处就会出现"少数序列无法按出口聚合"的坑。
    ident = identity_labels(payload)

    def emit(name: str, help_text: str, mtype: str, samples: list[tuple[str, object]]) -> None:
        rows = [(_with_identity(lbl, ident), _fmt(v)) for lbl, v in samples]
        rows = [(lbl, s) for lbl, s in rows if s is not None]
        if not rows:
            return                      # 全员无值：连 HELP 都不写，避免空指标污染
        out.append(f"# HELP {name} {help_text}")
        out.append(f"# TYPE {name} {mtype}")
        out.extend(f"{name}{lbl} {s}" for lbl, s in rows)

    recent = payload.get("recent") or {}
    degraded = payload.get("degraded") or {}
    pool = payload.get("pool") or {}
    inflight = payload.get("inflight") or {}

    emit("minter_up", "服务自认就绪（1=ready）。与 degraded 分开读。", "gauge",
         [("", payload.get("ok"))])
    emit("minter_degraded", "质量降级（1=别给流量，但重启没用）。", "gauge",
         [("", degraded.get("degraded"))])
    emit("minter_minting", "当前是否有铸造在进行。", "gauge", [("", payload.get("minting"))])
    emit("minter_uptime_seconds", "服务进程存活秒数。", "gauge",
         [("", payload.get("uptime_s"))])
    # chrome_starts 持续增长 = 杀/起循环。配合 chrome_uptime_seconds 一起看：
    # starts 递增而 chrome_uptime 一直很小，就是在抖 —— 服务级 uptime 看不出来。
    emit("minter_chrome_starts_total", "Chrome 进程启动次数（递增=杀/起循环）。", "counter",
         [("", payload.get("chrome_starts"))])
    emit("minter_chrome_uptime_seconds", "当前 Chrome 进程存活秒数。", "gauge",
         [("", payload.get("chrome_uptime_s"))])
    emit("minter_pool_size", "池中可用 token 总数。", "gauge", [("", pool.get("size"))])
    emit("minter_inflight", "铸造入口内的请求数。", "gauge", [("", inflight.get("current"))])
    emit("minter_inflight_max", "入口闸门上限（0/未设=不限）。", "gauge",
         [("", inflight.get("max"))])
    emit("minter_targets", "注册表中的目标数。", "gauge", [("", payload.get("targets_count"))])
    emit("minter_recent_window", "全局滚动窗口样本数。", "gauge", [("", recent.get("window"))])
    emit("minter_recent_pass_rate", "全局窗口通过率（空窗口不导出，不是 0）。", "gauge",
         [("", recent.get("pass_rate"))])
    emit("minter_fail_streak", "连续失败次数（全局取各目标最大值）。", "gauge",
         [("", degraded.get("fail_streak"))])

    targets: dict = payload.get("targets") or {}
    if targets:
        rows_pool, rows_rate, rows_win, rows_streak = [], [], [], []
        rows_warm, rows_deg, rows_age = [], [], []
        rows_minted, rows_failed, rows_served, rows_reused = [], [], [], []
        rows_want, rows_res = [], []
        rows_reason: list[tuple[str, object]] = []
        for name, t in sorted(targets.items()):
            lbl = f'{{target="{_esc(name)}"}}'
            t_recent = t.get("recent") or {}
            t_pool = t.get("pool") or {}
            t_stats = t.get("stats") or {}
            t_deg = t.get("degraded") or {}
            rows_pool.append((lbl, t_pool.get("size")))
            rows_want.append((lbl, t_pool.get("target")))
            rows_res.append((lbl, t_pool.get("reserved")))
            rows_rate.append((lbl, t_recent.get("pass_rate")))
            rows_win.append((lbl, t_recent.get("window")))
            rows_streak.append((lbl, t.get("fail_streak")))
            rows_warm.append((lbl, t.get("warmed")))
            rows_deg.append((lbl, t_deg.get("degraded")))
            rows_age.append((lbl, t.get("last_used_age_s")))
            rows_minted.append((lbl, t_stats.get("minted")))
            rows_failed.append((lbl, t_stats.get("failed")))
            rows_served.append((lbl, t_stats.get("served")))
            rows_reused.append((lbl, t_stats.get("reused")))
            reasons = t_recent.get("reasons") or {}
            for r in METRIC_REASONS:
                rows_reason.append(
                    (f'{{target="{_esc(name)}",reason="{r}"}}', reasons.get(r, 0))
                )
            for r, c in reasons.items():   # 引擎新增的标签也要导，别让新形态失踪
                if r not in METRIC_REASONS:
                    rows_reason.append(
                        (f'{{target="{_esc(name)}",reason="{_esc(r)}"}}', c)
                    )
        emit("minter_target_pool_size", "每目标池内 token 数。", "gauge", rows_pool)
        # 配套三件套：size / want / reserved。告警写 want - size 持续 > 0，
        # 别写 POOL_TARGET - size —— 后者在预约生效期间恒为负、永不触发。
        emit("minter_target_pool_want", "每目标期望池深（基础池深＋在途预约）。", "gauge", rows_want)
        emit("minter_target_reserved", "每目标在途预约数（未过期未核销）。", "gauge", rows_res)
        emit("minter_target_pass_rate", "每目标窗口通过率（空窗口不导出）。", "gauge", rows_rate)
        emit("minter_target_recent_window", "每目标窗口样本数。", "gauge", rows_win)
        emit("minter_target_fail_streak", "每目标连续失败次数。", "gauge", rows_streak)
        emit("minter_target_warmed", "该目标是否真铸出过 token（≠页面就绪）。", "gauge", rows_warm)
        emit("minter_target_degraded", "该目标是否降级（按目标摘流量的依据）。", "gauge", rows_deg)
        emit("minter_target_last_used_age_seconds", "距上次使用的秒数。", "gauge", rows_age)
        emit("minter_target_minted_total", "累计铸造成功次数。", "counter", rows_minted)
        emit("minter_target_failed_total", "累计铸造失败次数。", "counter", rows_failed)
        emit("minter_target_served_total", "累计交付 token 次数。", "counter", rows_served)
        emit("minter_target_reused_total", "累计命中池复用次数。", "counter", rows_reused)
        # interactive 占比升高 ⇒ 出口/画像信誉下滑（换出口，加重试没用）；
        # error 升高 ⇒ 最接近"被针对"；timeout 升高 ⇒ 多半是自身链路。别混成一个 failed。
        emit("minter_target_failure_reason", "窗口内各失败原因计数（标签集固定）。", "gauge",
             rows_reason)

    return "\n".join(out) + "\n"


# ------------------------------------------------------------------ 自检 --
_HEALTH_WILDCARD_HOSTS = frozenset({"0.0.0.0", "::", ""})


def health_host() -> str:
    """自检该往**哪个地址**打 —— 口径与绑定一致，通配地址换成回环。

    不写死 127.0.0.1：`BIND_HOST` 是可配的（文档里就有绑 `172.17.0.1` 的收窄写法），
    收到具体地址时容器里**没有**套接字监听回环，写死回环的自检会 100% 假 unhealthy。
    """
    host = BIND_HOST.strip()
    if host in LOOPBACK_HOSTS or host in _HEALTH_WILDCARD_HOSTS:
        return "127.0.0.1"
    return host


def health_url(port: int | None = None) -> str:
    return f"http://{health_host()}:{PORT if port is None else port}/healthz"


def selfcheck(timeout: float = 3.0) -> tuple[bool, str]:
    """打一次自己的 `/healthz`，返回 `(是否健康, 说明)`。给容器 HEALTHCHECK 用。

    判据刻意是 **HTTP 200**，**不**要求 `ready=true`：`ready` 是"Chrome/页面就绪"，
    冷启动那一两分钟它本来就该是 false；把"铸造就绪"当存活判据会让容器在整个预热期
    被反复重启（`start-period` 也就白设了）。

    两条防御（都不是理论风险，本项目各踩过一次）：
      · `ProxyHandler({})` —— 环境里的 `HTTP_PROXY` 会把**回环**地址也拐走，
        返回的是代理网关的错误体而不是 connection refused（极误导）；
      · 报文必须含 `ready` 键 —— 挡下"打到别的服务/网关却拿到 200"。
    """
    url = health_url()
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        with opener.open(url, timeout=timeout) as resp:
            raw = resp.read() or b""
    except Exception as e:  # noqa: BLE001 - 自检只关心"通不通"
        return False, f"连不上 {url} —— {type(e).__name__}: {str(e)[:160]}"
    try:
        body = json.loads(raw.decode("utf-8", "replace"))
    except Exception:  # noqa: BLE001
        body = None
    if not isinstance(body, dict) or "ready" not in body:
        return False, f"{url} 有响应但不是本服务的 /healthz（缺 ready 字段）：{raw[:80]!r}"
    return True, f"ok {url}（ready={body.get('ready')}）"


def _cli_selfcheck() -> int:
    ok, why = selfcheck()
    print(("[ok] " if ok else "[fail] ") + why, file=sys.stdout if ok else sys.stderr, flush=True)
    return 0 if ok else 1


# ------------------------------------------------------------------ HTTP --
class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    registry: TargetRegistry = None      # type: ignore[assignment]  # 由 main() 注入

    def _send(self, code: int, payload: dict, *, extra_headers: dict | None = None):
        body = json.dumps(payload, ensure_ascii=False).encode()
        self.send_response(code)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(body)))
        for k, v in (extra_headers or {}).items():
            self.send_header(k, v)
        self.end_headers()
        try:
            self.wfile.write(body)
        except BrokenPipeError:      # 调用方超时先走了；不是错误
            pass

    def _authed(self) -> bool:
        # 空 KEY ⇒ 放行：**只在回环绑定或显式 MINTER_ALLOW_INSECURE 下可达**
        # （非回环 + 无 key 的组合已被 bind_guard_error() 拦在启动之前）。
        if not KEY:
            return True
        return self.headers.get("X-Minter-Key") == KEY

    def _resolve_target(self, query: dict) -> tuple[TargetState | None, dict | None]:
        """`?target=` ⇒ 白名单校验。返回 (状态, 错误载荷)。"""
        name = (query.get("target", [""])[0] or "").strip()
        if not name:
            default = self.registry.default()
            if default is None:
                return None, {
                    "error": "未指定 target，且本部署没有默认目标（注册表里有多个目标时"
                             "调用方必须显式指定）",
                    "known_targets": self.registry.names(),
                }
            name = default.name
        try:
            return _state_for(name), None
        except KeyError:
            return None, {
                "error": f"未知目标 {name!r} —— 目标只能由部署方在注册表里注册，"
                         "请求不接受任意 URL",
                "known_targets": self.registry.names(),
            }

    def do_GET(self):
        u = urlparse(self.path)
        if u.path == "/healthz":
            q = parse_qs(u.query)
            with_targets = q.get("targets", ["1"])[0] not in ("0", "false", "no")
            return self._send(200, health(self.registry, with_targets=with_targets))
        if u.path == "/metrics":
            # 🔴 与 /healthz 不同，/metrics **要鉴权**：它导出的是按目标拆开的运营画像
            #    （通过率、失败原因分布、池深、使用节奏）。/healthz 得留给编排系统的
            #    探活（探活多半不带头），而抓取端配一个头是标准做法，没有对称的代价。
            if not self._authed():
                return self._send(401, {"error": "bad or missing X-Minter-Key"})
            body = render_metrics(health(self.registry)).encode()
            self.send_response(200)
            # 版本号必须带：省掉会让部分抓取端按 OpenMetrics 解析而对 `# HELP` 报错。
            self.send_header("content-type", "text/plain; version=0.0.4; charset=utf-8")
            self.send_header("content-length", str(len(body)))
            self.end_headers()
            try:
                self.wfile.write(body)
            except BrokenPipeError:      # 抓取端超时先走了；不是错误
                pass
            return None
        if u.path == "/targets":
            return self._send(200, {
                "object": "list",
                "default": self.registry.default().name if self.registry.default() else None,
                "data": [t.as_public() for t in self.registry.all_targets()],
            })
        return self._send(404, {"error": "not found"})

    def do_POST(self):
        if not self._authed():
            return self._send(401, {"error": "bad or missing X-Minter-Key"})
        u = urlparse(self.path)
        if u.path != "/v1/turnstile/mint":
            return self._send(404, {"error": "not found"})
        q = parse_qs(u.query)
        try:
            n = int(q.get("n", ["1"])[0])
        except ValueError:
            return self._send(400, {"error": "n 必须是整数"})
        n = max(1, min(n, 32))            # 上限防呆：一次要太多会把调用方自己的超时打爆
        # 没传 ⇒ 用缺省值；显式传 `reserve=0` ⇒ 真的关掉（这两者必须能区分，
        # 否则想退回旧行为的调用方没有任何办法表达）。
        raw_reserve = q.get("reserve", [None])[0]
        try:
            reserve = RESERVE_DEFAULT if raw_reserve is None else int(raw_reserve)
        except ValueError:
            return self._send(400, {"error": "reserve 必须是整数"})
        reserve = max(0, min(reserve, RESERVE_MAX))
        st, err = self._resolve_target(q)
        if err is not None:
            return self._send(400, err)
        if reserve:
            # 先登记再取用：登记后立刻 _WAKE，补货线程在本请求还在铸第一个时就开始
            # 铸后续的。反过来（先取用后登记）会白白丢掉整个请求耗时的提前量。
            #
            # 🔴 隐式（缺省）与显式预约的登记语义**必须不同**：
            #   · 显式 `?reserve=N` 是调用方的主动声明，频次低，**累加**才能表达
            #     "两个批量任务同时在跑，各要 N 个"。
            #   · 隐式缺省是**每个请求都会登记**。若也累加，QPS×TTL 条 ×RESERVE_DEFAULT
            #     会把池深长期顶到 RESERVE_MAX（夹取只是挡住数字，铸造照样发生）——
            #     等于"只要有流量就永久满池铸造"，把一个便利默认值变成了持续的风控压力。
            #   所以隐式走**吸收式**：只在现有预约总数不足 RESERVE_DEFAULT 时补到地板，
            #   已有更深的预约（显式声明的）时什么都不做。
            with _STATE_LOCK:
                st.prune()                                   # type: ignore[union-attr]
                if raw_reserve is None:
                    shortfall = reserve - st.reserved()       # type: ignore[union-attr]
                    if shortfall > 0:
                        st.reservations.append([float(shortfall), time.time()])  # type: ignore[union-attr]
                else:
                    st.reservations.append([float(reserve), time.time()])        # type: ignore[union-attr]
            _WAKE.set()
        # 入口闸门：非阻塞抢名额，抢不到立刻回 503 —— 不排队。排队的代价是线程堆积，
        # 且调用方多半已经超时走了，等它排到再铸就是纯浪费（还白烧一次挑战）。
        if not _gate_acquire():
            return self._send(503, {
                "error": f"busy: 已有 {MAX_INFLIGHT} 个铸造在进行，请退避重试",
                "max_inflight": MAX_INFLIGHT,
            }, extra_headers={"Retry-After": "5"})
        try:
            # 逐个取，**不要用列表推导一把梭**：第 k 个失败时前 k-1 个已经是真 token
            # （其中从池里 pop 出来的那些丢了就真没了），整体抛弃等于白铸。
            got: list[dict] = []
            err_msg: str | None = None
            for _ in range(n):
                try:
                    got.append(take_token(st))           # type: ignore[arg-type]
                except (Exception, SystemExit) as e:     # noqa: BLE001
                    # SystemExit 必须一起接：engine.start_chrome()/kill_all_chrome() 抛的是它，
                    # 而 BaseHTTPRequestHandler 不拦 —— 漏出去就是连接被重置，调用方拿不到 503。
                    err_msg = f"{type(e).__name__}: {e}"[:200]
                    break
        finally:
            _gate_release()                              # 任何出口都必须还名额，否则闸门只减不增
        if not got:                                      # 一个都没拿到才算彻底失败
            return self._send(503, {
                "error": err_msg or "no token",
                "target": st.target.name,                # type: ignore[union-attr]
                **health(self.registry, with_targets=False),
            })
        # 回传预约现状：调用方传了 reserve=100 而 RESERVE_MAX=32 时必须能看出被夹了，
        # 否则它会按"100 个都在路上"规划自己的并发，然后困惑于为什么一直走 live。
        if reserve:
            with _STATE_LOCK:
                st.prune()                               # type: ignore[union-attr]
                res_state = {
                    "requested": reserve,
                    "accepted": min(reserve, RESERVE_MAX),
                    "outstanding": st.reserved(),        # type: ignore[union-attr]
                    "max": RESERVE_MAX,
                    "ttl_s": RESERVE_TTL_S,
                    # 让调用方能分辨"这是服务端的缺省"还是"我自己传的"——否则看到
                    # requested:2 会去翻自己的代码找那个不存在的 reserve=2。
                    "implicit": raw_reserve is None,
                }
        else:
            res_state = None
        if n == 1:
            body = dict(got[0])
            if res_state is not None:
                body["reserve"] = res_state
            return self._send(200, body)
        payload = {
            "tokens": [g["token"] for g in got],
            "sources": [g["source"] for g in got],
            "target": st.target.name,                    # type: ignore[union-attr]
        }
        if res_state is not None:
            payload["reserve"] = res_state
        if err_msg is not None:                          # 部分成功：如实告知少了几个
            payload["partial"] = True
            payload["requested"] = n
            payload["error"] = err_msg
        return self._send(200, payload)

    def log_message(self, fmt, *args):   # 静音默认访问日志（避免刷屏）
        pass


# --------------------------------------------------------------------- 启动 --
def build_registry() -> TargetRegistry:
    """注册表来源：文件优先；没有文件时回退到 env 单目标（兼容 aivideomaker 的写法）。"""
    path = Path(TARGETS_FILE) if TARGETS_FILE else None
    if path is not None and path.exists():
        reg = TargetRegistry(path=path, default_name=DEFAULT_TARGET_NAME)
        reg.load()
        return reg
    if LEGACY_SITEKEY:
        import tempfile

        # 防呆：SITEKEY 给了但 ORIGIN/LIGHT_URL 都没给时，旧写法会拼出 "/robots.txt"
        # 这种没有 scheme 的非法 URL，一路带到 Page.navigate 才炸，报错极难追溯来源。
        # 这里前置拦掉，并把三条出路写进错误信息。
        if not LEGACY_LIGHT_URL and not os.environ.get("ORIGIN"):
            raise SystemExit(
                "配置不完整：设了 SITEKEY 但既没有 ORIGIN 也没有 LIGHT_URL。\n"
                "  三条出路：① 设 ORIGIN=https://example.com（自动用 /robots.txt 当轻量页）；\n"
                "           ② 直接设 LIGHT_URL=https://example.com/robots.txt；\n"
                "           ③ 改用 TARGETS_FILE 多目标注册表（推荐，几百个 API 时唯一可维护的形态）。"
            )
        tmp = Path(tempfile.gettempdir()) / "minter-legacy-targets.json"
        tmp.write_text(json.dumps([{
            "name": LEGACY_TARGET_NAME,
            "sitekey": LEGACY_SITEKEY,
            "light_url": LEGACY_LIGHT_URL or f"{os.environ['ORIGIN'].rstrip('/')}/robots.txt",
            "origin": os.environ.get("ORIGIN") or None,
            "prewarm": env_flag(os.environ.get("POOL_PREWARM", "1")),
        }], ensure_ascii=False), encoding="utf-8")
        reg = TargetRegistry(path=tmp, default_name=LEGACY_TARGET_NAME)
        reg.load()
        return reg
    raise TargetError(
        f"没有目标可服务：既没有注册表文件（TARGETS_FILE={TARGETS_FILE}），"
        "也没有 SITEKEY/LIGHT_URL（兼容旧写法的两个 env）。"
        "见 targets.example.yaml。"
    )


_STOPPING = threading.Event()


def graceful_shutdown(srv, signame: str = "SIGTERM") -> None:
    """收到停机信号：停止收新连接 → 关掉本进程起的 Chrome → 退出。

    为什么要**主动关 Chrome**：Chrome 是 `nohup setsid` 起的**独立进程组**（见
    `engine.start_chrome`），不会随本进程退出而消失。不收这一手，`docker stop` /
    滚动更新后宿主上就留着孤儿 Chrome 占着 profile 和 CDP 端口 —— 下次启动虽有
    `kill_all_chrome` 兜底，但那是"启动时"才发生，中间这段时间进程白占内存，
    多次重启还会叠加（E2E-AVM-004 里"宿主背着上千个 chrome"就是这么攒出来的）。

    幂等：`docker stop` 之后常常紧跟 SIGKILL，信号也可能连发两次 —— 用 `_STOPPING`
    保证清理只做一次，否则第二次 `shutdown()` 会在已关闭的 server 上抛异常、
    把退出码搞成非 0，让编排系统误判成"崩溃退出"。
    """
    if _STOPPING.is_set():
        return
    _STOPPING.set()
    print(f"[shutdown] 收到 {signame}，停止接收新请求…", flush=True)
    threading.Thread(target=srv.shutdown, daemon=True).start()   # 不能在信号处理里直接调，会自锁
    _WAKE.set()                                                   # 唤醒补货线程，让它尽快看到停机
    try:
        M.kill_all_chrome()          # 返回 None（按 profile + 端口特征杀，不报个数）
        print("[shutdown] 已清理本项目的 Chrome 实例", flush=True)
    except (Exception, SystemExit) as e:                           # 纪律 #2：kill 路径会抛 SystemExit
        print(f"[shutdown] 清理 Chrome 失败（忽略，交给下次启动兜底）：{e!r}", flush=True)


def _install_signal_handlers(srv) -> None:
    """只在主线程装；装不上不算致命（退化成原来的"靠下次启动兜底"形态）。"""
    def _handler(signum, _frame):
        graceful_shutdown(srv, signal.Signals(signum).name)

    for sig in (signal.SIGTERM, signal.SIGINT):
        try:
            signal.signal(sig, _handler)
        except (ValueError, OSError) as e:      # 非主线程 / 平台不支持
            print(f"[warn] 未能注册 {sig!r} 处理器：{e!r}", flush=True)


def main() -> None:
    err = bind_guard_error(BIND_HOST, KEY, ALLOW_INSECURE)
    if err:
        print(f"[fatal] {err}", file=sys.stderr, flush=True)
        raise SystemExit(2)
    if ALLOW_INSECURE and not KEY:
        print("[warn] MINTER_ALLOW_INSECURE 已开启且未设 MINTER_KEY —— "
              "请自行确保该端口不可从公网到达", flush=True)
    elif not KEY:
        print("[warn] MINTER_KEY 未设置 —— 仅回环绑定，切勿暴露公网", flush=True)

    tz = tz_state()
    if not tz["env"]:
        print("[warn] TZ 未设置 —— 容器默认 UTC 会让 CF 判定「时区/会话不一致」并下发"
              "**交互式挑战** ⇒ 铸造恒失败（现象：interactive、0 token）。"
              "请显式设 TZ=（且与**出口 IP 地理一致**）。", flush=True)
    elif not tz["zoneinfo_present"]:
        print(f"[warn] TZ={tz['env']} 但镜像内没有 /usr/share/zoneinfo/{tz['name']} —— "
              "libc 会按 UTC 解释它（浏览器走自带 ICU 库、不受影响）。装上 tzdata 可消除这层"
              "「只对浏览器生效」的半吊子状态。", flush=True)

    registry = build_registry()
    sync_registry(registry)
    Handler.registry = registry

    # 按 base_pool() 判定而非 prewarm 标志：只配了 pool_target 的目标同样会被预热
    # （见 TargetState.base_pool）。按标志列会让横幅与实际行为不符。
    prewarm = [f"{s.target.name}×{s.base_pool()}" for s in _STATES.values() if s.base_pool() > 0]
    overridden = [
        s.target.name for s in _STATES.values()
        if s.target.pool_target is not None or s.target.ttl_s is not None
    ]
    threading.Thread(target=refill_loop, args=(registry,), daemon=True).start()
    threading.Thread(target=reload_loop, args=(registry,), daemon=True).start()
    _WAKE.set()

    srv = ThreadingHTTPServer((BIND_HOST, PORT), Handler)
    _install_signal_handlers(srv)
    print(
        f"minter-service 已启动 http://{BIND_HOST}:{PORT}\n"
        f"  目标 {len(_STATES)} 个：{registry.names()}"
        f"（默认 {registry.default().name if registry.default() else '无（调用方须指定）'}；"
        f"预热 {prewarm or '无'}）\n"
        f"  池 默认每目标 {POOL_TARGET} 个 / TTL {TTL_S}s"
        f"{'（已覆盖：' + ', '.join(overridden) + '）' if overridden else ''}；"
        f"缺省预约 {RESERVE_DEFAULT}；热页上限 {MAX_WARM_TARGETS}\n"
        f"  Chrome：CDP 端口 {M.PORT}、profile {M.PROFILE_DIR}、"
        f"{'headless' if M.HEADLESS else '有头（xvfb）'}\n"
        f"  鉴权 {'开' if KEY else '关（仅回环）'}；"
        f"TZ={tz['name']}{'（默认值！请显式设置）' if tz['assumed_default'] else ''}"
        f"（offset {tz['offset']}）",
        flush=True,
    )
    srv.serve_forever()


if __name__ == "__main__":
    # 容器 HEALTHCHECK 走这一支：端口与地址由本进程自己从 PORT / BIND_HOST 读，
    # 镜像里不再重复写一遍数字（写死会造出"假 unhealthy"）。
    if "--selfcheck" in sys.argv[1:]:
        raise SystemExit(_cli_selfcheck())
    main()
