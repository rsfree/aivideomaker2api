# minter-service —— 通用 Cloudflare Turnstile 铸造服务
#
# 🔴 本文件是**上游 minter 仓库的 vendored 副本**（源：~/WorkBuddy/minter，2026-11-20
#    同步；minter-service:0.1.0 已在生产跑满 4 天）。随本项目一起构建发布，
#    目的是让「一套 compose」同时起业务与过闸能力 —— 不再需要单独部署、单独搬镜像。
#    修改前先确认是否该回上游：这里不是分叉，是构建入口。
#
# 为什么**单独一个镜像**：它要装 Google Chrome + Xvfb（约 500MB），而各业务服务镜像
# 大多是 python-slim 的瘦身版 —— 混在一起会让每次改业务代码都重下 Chrome。
#
# 为什么必须 **Xvfb（有头）而不是 --headless**：上游实测同一台机器、同一 Chrome 版本、
# 同出口 IP，`--headless=new` **0/4 全部 46 秒超时**，`xvfb-run` 有头 **8/8、3.7 秒/个**。
#
# 安全：该服务能产出"绕过风控的凭证"。**不要映射端口到宿主**，只让同 compose 网络里的
# 调用方用。🔴 **MINTER_KEY 是必设项**：本镜像 BIND_HOST=0.0.0.0（非回环），未设 key 时
# 服务会**拒绝启动**（fail-closed）—— 容器表现为 restart 循环，日志写明原因与三条出路。
# 仅私有网络可用 MINTER_ALLOW_INSECURE=1 显式放行。
#
# ⚠️ 必须钉 **bookworm**：`python:3.12-slim` 现在基于 Debian 13(trixie)，而 Chrome 的 deb
# 依赖 `libasound2` —— 该包在 trixie 里改名为 `libasound2t64` ⇒ apt 直接 exit 100 失败
# （实测踩到：构建卡在装 Chrome 那一步，日志只给一句 "did not complete successfully"）。
FROM python:3.12-slim-bookworm

ENV DEBIAN_FRONTEND=noninteractive \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PORT=8899 \
    BIND_HOST=0.0.0.0 \
    CDP_PORT=9333 \
    CHROME_PROFILE=/data/chrome \
    POOL_TARGET=4 \
    TOKEN_TTL_S=120 \
    MAX_WARM_TARGETS=8 \
    TARGETS_FILE=/app/targets.yaml \
    TZ=Asia/Shanghai

# 🔴 `CDP_PORT` 必须与 `service.py` 的默认值（9333）一致，**不要改回 9222**：
#    9222 是 Chrome 调试端口的业界默认值，宿主/其他容器上极易已被占用；用 host 网络
#    部署时会连到**别人的 Chrome** 上去 —— 表现为铸造莫名失败或 profile 互相踩。
#    这里写死 9222 还会让 service 里的 `setdefault("CDP_PORT","9333")` 永远不生效
#    （ENV 优先），等于悄悄绕过那条防御。

# ★ `TZ` 是**铸造能否成功的前提，不是可选的美化项**：容器默认 UTC 时 CF 会判定
#   "时区/会话不一致"并下发**交互式挑战** ⇒ 铸造恒失败（现象是 `interactive` 分类、
#   约 12s 快速失败、0 token；冷/热 profile 都一样）。
#   🔴 它必须与**出口 IP 的地理**一致 —— 这一条只能在部署侧决定，镜像给不了。
#
# 额外装的几组（都有理由，不是"顺手多加"）：
#   * xauth      —— 没有它 `xvfb-run` 直接起不来；且补货线程会被 SystemExit 静默打死
#                   ⇒ ready=false / last_error=null，极难查（实测踩过）；
#   * 多字体      —— 只装 liberation 时字体家族只有 4 个，而真人浏览器有几十个，
#                   这是容器里最容易被 CF 当机器人的信号之一；
#   * libgl1/mesa —— CF 会查 WebGL 渲染器，缺库时报"无 WebGL" ⇒ 强负信号；
#   * tzdata     —— 让 `TZ` 在 **libc 层**也真的生效（缺 `/usr/share/zoneinfo` 时
#                   `TZ` 会被 libc 静默当成 UTC：变量看起来设了、`date` 却是 UTC）。
# ⚠️ 这些注释**不能写在 RUN 的续行里面**：Dockerfile 只在行首识别注释，续行里的 `#`
#    会被 shell 当注释，把后半行命令整段吃掉，而该层还会"成功"—— 典型的静默破坏。
#
# ⚠️ Chrome 运行库与 Chrome 的 deb **必须合成一层**：拆开时若前一层结尾清理了
#    `/var/lib/apt/lists/*`，后一层装 deb 就会因没有 apt 索引而报
#    `google-chrome-stable : Depends: libvulkan1 but it is not installable`（exit 100）。
ARG TARGETARCH
# 🔴 Chrome 版本**钉死**，不用 `google-chrome-stable_current_amd64.deb`（那个是浮动 latest）：
#    浮动版本会让同一个镜像 tag 在不同时间重建出不同内容（违反可复现），而且某天 Chrome
#    新版若让 CF 判定异常，构建照样成功、铸造全败，回滚镜像也救不了（重建还是新的）。
#    升级走这里：从
#      https://dl.google.com/linux/chrome/deb/dists/stable/main/binary-amd64/Packages
#    取 `Filename:` 里的版本号（形如 158.0.8127.36-1），改本行后重新发版。
#    注意必须带 `-1` 修订号：不带的话 pool 里没有对应文件（404）。
ARG CHROME_VERSION=158.0.8127.36-1
RUN sed -i 's|http://deb.debian.org|https://deb.debian.org|g' /etc/apt/sources.list.d/debian.sources \
    && printf 'Acquire::Retries "6";\nAcquire::http::Timeout "30";\nAcquire::https::Timeout "30";\n' \
        > /etc/apt/apt.conf.d/80minter-retries \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates curl xvfb xauth dbus dbus-x11 tzdata \
        fonts-liberation fonts-dejavu-core fonts-noto-core fonts-noto-color-emoji \
        libgl1 libglx-mesa0 libegl1 \
        libnss3 libxss1 libatk-bridge2.0-0 libatk1.0-0 libgtk-3-0 libgbm1 libasound2 \
        libx11-xcb1 libxcomposite1 libxdamage1 libxrandr2 libxkbcommon0 \
        libpango-1.0-0 libcairo2 \
    && if [ "$TARGETARCH" = "arm64" ]; then \
        apt-get install -y --no-install-recommends chromium; \
    else \
        url="https://dl.google.com/linux/chrome/deb/pool/main/g/google-chrome-stable/google-chrome-stable_${CHROME_VERSION}_amd64.deb"; \
        if ! curl -fsSL -o /tmp/chrome.deb "$url"; then \
            echo "Chrome ${CHROME_VERSION} 下载失败：$url" >&2; \
            echo "从 dl.google.com/linux/chrome/deb/dists/stable/main/binary-amd64/Packages 取 Filename 后更新 CHROME_VERSION" >&2; \
            exit 1; \
        fi; \
        apt-get install -y /tmp/chrome.deb \
        && rm -f /tmp/chrome.deb; \
    fi \
    && (google-chrome --version || chromium --version || true) \
    && rm -rf /var/lib/apt/lists/*
#
# 注：Google 不发布 Linux/arm64 的 Chrome ⇒ arm64 走 Debian 原生 chromium
#     （bookworm-security 的 152.x 与 Chrome 153 同代，不算旧）。二进制路径由
#     `engine.chrome_bin()` 自动发现（google-chrome → chromium）。
#     ⚠️ 不要用 `--platform linux/amd64` 让 Chrome 跑在 QEMU 模拟层里：CF 挑战的
#     JS 计时/行为信号会明显异常（等于把"我是模拟器"写在脸上）。
#
# CDP 客户端：服务用**原生 CDP**，不依赖 Playwright（少一层、也少一个浏览器下载）。
# PyYAML 只为让注册表能用 .yaml（不想装也能用 .json，两种都认）。
# ⚠️ 这里刻意用 pip 而不是 uv：本镜像是**已验证 4 天的生产制品**，换包管理器等于换
#    制品；用户级「Python 一律 uv」的约定针对开发机环境，不适用于镜像内的运行时依赖。
RUN pip install --no-cache-dir websocket-client==1.9.0 PyYAML==6.0.2

WORKDIR /app
COPY minter /app/minter
# 注册表**内置**：不依赖宿主挂载文件的存在性（缺文件会被 Docker 建成目录，容器读到
# 目录 ⇒ 注册表加载失败）。需要临时改目标时在 compose 里挂载覆盖即可。
COPY targets.yaml /app/targets.yaml

# Chrome 的 profile 落在卷里：重启后不用重新冷启动。
# ⚠️ 实测复盘：全新 profile 的**首铸**曾卡死而非"慢"；旧注释里的「首次 render ~46 秒」是
#    45s 超时签名的误读，已证伪。把 `/data` 卷"用长期实例的热 profile 播种"这条**已被实验
#    否掉**（播种后 3 连败全 interactive：CF 的信任不落在 profile 里）。播种的唯一价值是
#    省掉预热时间。别再据此把 /data 卷当解药。
VOLUME ["/data"]
# ⚠️ `EXPOSE` 只是**文档性**信息（host 网络下不产生端口映射）；实际监听端口由运行时
#    的 `PORT` 决定（默认与这里一致）。
EXPOSE 8899

# 🔴 **不要把地址/端口写死在健康检查里**：`PORT`/`BIND_HOST` 都是部署可覆盖的运行时旋钮，
#    写死会造出"服务好端端的、`/healthz` 一片正常，`docker ps` 却恒 unhealthy"的假警报。
#    假警报比"没有健康检查"更糟：它会训练人忽略健康状态，也会卡住
#    `depends_on: condition: service_healthy` 这类编排。
#    `--selfcheck` 与 `/healthz` **是同一处判据**（HTTP 200 且报文含 ready 键）；
#    `ready=false`（Chrome 还在预热）仍算健康 —— 冷启动那一两分钟它本来就该是 false。
HEALTHCHECK --interval=30s --timeout=5s --start-period=150s --retries=3 \
    CMD ["python3", "-m", "minter.service", "--selfcheck"]

CMD ["python3", "-m", "minter.service"]
