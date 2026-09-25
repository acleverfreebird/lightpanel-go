#!/usr/bin/env bash
# LightPanel 一键安装/升级脚本（Linux amd64/arm64，systemd）。
# 用法与说明见仓库 README「一键部署」或: sudo bash install.sh --help
set -euo pipefail

REPO="${LP_REPO:-acleverfreebird/lightpanel-go}"
REF="${LP_REF:-main}"
INSTALL_DIR="/opt/lightpanel"
SERVICE_NAME="lightpanel"
HELPER_SERVICE_NAME="lightpanel-helper"
UNIT="/etc/systemd/system/${SERVICE_NAME}.service"
HELPER_UNIT="/etc/systemd/system/${HELPER_SERVICE_NAME}.service"
PANEL_USER="${LP_PANEL_USER:-lightpanel}"
GO_FALLBACK="go1.25.0"

HOST="127.0.0.1"
PORT="8888"
ADMIN_USER="admin"
ORIGIN=""
READ_ONLY="false"
PASS_HASH="${LP_PASS_HASH:-}"
RELEASE=""
FORCE_CONFIG=0
NO_START=0
LEGACY_ROOT=0

usage() {
  cat <<'USAGE'
LightPanel 一键安装/升级脚本（Linux amd64/arm64，需 root，建议 systemd）

用法: sudo bash install.sh [选项]

选项:
  --port N              监听端口（默认 8888，仅 127.0.0.1 回环监听）
  --admin NAME          管理员用户名（默认 admin）
  --origin URL          public_origin；反代/域名场景如 https://panel.example.com
  --read-only           只读模式（read_only=true）
  --password-hash HASH  预置 bcrypt 哈希（等价于环境变量 LP_PASS_HASH）
  --repo OWNER/NAME     源码仓库（默认 acleverfreebird/lightpanel-go）
  --ref REF             构建分支或标签（默认 main）
  --release TAG         使用 GitHub Release 预编译二进制（latest 或具体 tag）；
                        默认下载源码现场编译并自动安装缺失的 Go 工具链
  --legacy-root         旧模式：面板进程以 root 运行，不创建专用用户/不启用 helper
  --force-config        覆盖已有 /opt/lightpanel/config.toml（默认保留原配置）
  --no-start            仅安装文件，不启动/重启服务
  -h, --help            显示本帮助

示例:
  sudo bash install.sh
  sudo bash install.sh --port 9443 --read-only
  sudo bash install.sh --origin https://panel.example.com
  sudo LP_PASS_HASH='$2b$12$...' bash install.sh

生成密码哈希（任意装有面板二进制的机器上交互执行）:
  /opt/lightpanel/lightpanel -hash-password
USAGE
}

log()  { printf '[lightpanel] %s\n' "$*"; }
fail() { printf '[lightpanel] 错误: %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    --port)          PORT="${2:?--port 需要参数}"; shift 2;;
    --admin)         ADMIN_USER="${2:?--admin 需要参数}"; shift 2;;
    --origin)        ORIGIN="${2:?--origin 需要参数}"; shift 2;;
    --read-only)     READ_ONLY="true"; shift;;
    --password-hash) PASS_HASH="${2:?--password-hash 需要参数}"; shift 2;;
    --repo)          REPO="${2:?--repo 需要参数}"; shift 2;;
    --ref)           REF="${2:?--ref 需要参数}"; shift 2;;
    --release)       RELEASE="${2:?--release 需要参数}"; shift 2;;
    --legacy-root)   LEGACY_ROOT=1; shift;;
    --force-config)  FORCE_CONFIG=1; shift;;
    --no-start)      NO_START=1; shift;;
    -h|--help)       usage; exit 0;;
    *)               usage >&2; fail "未知参数: $1";;
  esac
done

[ "$(id -u)" -eq 0 ] || fail "请用 root 运行: sudo bash $0"
command -v uname >/dev/null 2>&1 || fail "仅支持 Linux"

case "$(uname -m)" in
  x86_64)          ARCH="amd64";;
  aarch64|arm64)   ARCH="arm64";;
  *)               fail "不支持的架构: $(uname -m)（仅 amd64/arm64）";;
esac

HAVE_SYSTEMD=0
[ -d /run/systemd/system ] && HAVE_SYSTEMD=1
[ "$HAVE_SYSTEMD" -eq 1 ] || log "警告: 未检测到 systemd，将只安装文件并输出手动启动命令"

case "$PORT" in ''|*[!0-9]*) fail "端口必须是数字: $PORT";; esac
[ "$PORT" -ge 1 ] && [ "$PORT" -le 65535 ] || fail "端口必须在 1..65535: $PORT"
case "$HOST" in
  127.*|::1) : ;;
  *) fail "host 必须是回环地址（非回监听需 TLS 证书，请改用反向代理并保持 127.0.0.1）: $HOST" ;;
esac
if [ -n "$ORIGIN" ]; then
  case "$ORIGIN" in
    http://*|https://*) : ;;
    *) fail "origin 必须以 http:// 或 https:// 开头: $ORIGIN" ;;
  esac
  case "$ORIGIN" in */) fail "origin 不能以 / 结尾: $ORIGIN";; esac
  case "$ORIGIN" in
    https://*) : ;;
    http://*)
      oh="${ORIGIN#http://}"; oh="${oh%%/*}"; oh="${oh%%:*}"
      case "$oh" in 127.*|localhost|::1) : ;; *) fail "http origin 必须是回环地址，远程访问请用 https:// + 反向代理";; esac ;;
  esac
fi
case "$PASS_HASH" in
  '') : ;;
  '$2a$'*|'$2b$'*|'$2y$'*) : ;;
  *) fail "password_hash 不是 bcrypt 格式（应以 \$2a\$ / \$2b\$ / \$2y\$ 开头）" ;;
esac

if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fSL --retry 2 --connect-timeout 15 -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -q --tries=2 --timeout=15 -O "$2" "$1"; }
else
  fail "需要 curl 或 wget，请先安装（如 apt/dnf install curl）"
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

find_go() {
  local g v
  for g in "$(command -v go 2>/dev/null || true)" /usr/local/go/bin/go; do
    [ -n "$g" ] && [ -x "$g" ] || continue
    v="$("$g" env GOVERSION 2>/dev/null || true)"
    case "$v" in
      go1.*) v="${v#go}";;
      *) continue;;
    esac
    if [ "$v" = "$(printf '%s\n%s\n' "$v" 1.21 | sort -V | head -n1)" ]; then
      GO="$g"; return 0
    fi
  done
  return 1
}

install_go() {
  local ver f="$WORK/go.tgz" base
  f_first_line() {
    local url tmp="$WORK/ver.txt"
    for url in "https://go.dev/VERSION?m=text" "https://golang.google.cn/VERSION?m=text"; do
      if fetch "$url" "$tmp" 2>/dev/null && [ -s "$tmp" ]; then
        ver="$(head -n1 "$tmp")"; return 0
      fi
    done
    return 1
  }
  ver="$GO_FALLBACK"
  if f_first_line; then case "$ver" in go1.*) : ;; *) ver="$GO_FALLBACK";; esac; fi
  log "安装 Go 工具链 $ver 到 /usr/local/go（原有安装将备份为 /usr/local/go.bak.*）"
  for base in "https://go.dev/dl" "https://golang.google.cn/dl" "https://mirrors.aliyun.com/golang"; do
    rm -f "$f"
    if fetch "$base/$ver.linux-$ARCH.tar.gz" "$f" 2>/dev/null && [ -s "$f" ]; then break; fi
  done
  [ -s "$f" ] || fail "下载 Go 工具链失败，请检查网络后重试"
  [ -d /usr/local/go ] && mv /usr/local/go "/usr/local/go.bak.$(date +%s)"
  tar -C /usr/local -xzf "$f"
  GO="/usr/local/go/bin/go"
}

build_from_source() {
  local tarball="$WORK/src.tar.gz" url ok=0 prefix="${LP_SOURCE_MIRROR:-}"
  log "下载源码: github.com/$REPO (ref: $REF)"
  for url in \
    "${prefix}https://codeload.github.com/$REPO/tar.gz/refs/heads/$REF" \
    "${prefix}https://codeload.github.com/$REPO/tar.gz/refs/tags/$REF" \
    "https://codeload.github.com/$REPO/tar.gz/refs/heads/$REF" \
    "https://codeload.github.com/$REPO/tar.gz/refs/tags/$REF"; do
    rm -f "$tarball"
    if fetch "$url" "$tarball" 2>/dev/null && [ -s "$tarball" ]; then ok=1; break; fi
  done
  [ "$ok" -eq 1 ] || fail "下载源码失败：检查网络与 --ref/$REPO 是否存在"
  mkdir -p "$WORK/src"
  tar -xzf "$tarball" -C "$WORK/src" --strip-components=1
  find_go || install_go
  log "编译中: CGO_ENABLED=0 GOOS=linux GOARCH=$ARCH"
  if ! (cd "$WORK/src" && CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" GOTOOLCHAIN=auto \
        "$GO" build -trimpath -ldflags='-s -w' -o "$WORK/lightpanel" .); then
    log "默认模块代理不可用，改用 goproxy.cn 重试..."
    (cd "$WORK/src" && CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" GOTOOLCHAIN=auto \
     GOPROXY="https://goproxy.cn,direct" \
     "$GO" build -trimpath -ldflags='-s -w' -o "$WORK/lightpanel" .) \
     || fail "编译失败"
  fi
}

download_release() {
  local url prefix="${LP_SOURCE_MIRROR:-}"
  if [ "$RELEASE" = "latest" ]; then
    url="https://github.com/$REPO/releases/latest/download/lightpanel-linux-$ARCH"
  else
    url="https://github.com/$REPO/releases/download/$RELEASE/lightpanel-linux-$ARCH"
  fi
  log "下载预编译二进制: $url"
  rm -f "$WORK/lightpanel"
  fetch "${prefix}$url" "$WORK/lightpanel" \
    || fetch "$url" "$WORK/lightpanel" \
    || fail "下载失败：$REPO 可能还没有发布 $RELEASE"
  [ "$(head -c 4 "$WORK/lightpanel" | od -An -tx1 | tr -d ' \n')" = "7f454c46" ] \
    || fail "下载内容不是有效的 Linux 二进制"
  chmod 0755 "$WORK/lightpanel"
  if fetch "$url.sha256" "$WORK/lightpanel.sha256" 2>/dev/null && [ -s "$WORK/lightpanel.sha256" ]; then
    (cd "$WORK" && sha256sum -c "lightpanel.sha256") || fail "sha256 校验失败"
  else
    log "警告: 未提供 .sha256 校验文件，跳过完整性校验"
  fi
}

write_config() {
  local dst="$INSTALL_DIR/config.toml" hash="$1"
  awk -v host="$HOST" -v port="$PORT" -v user="$ADMIN_USER" -v hash="$hash" \
      -v origin="${ORIGIN:-http://127.0.0.1:$PORT}" -v ro="$READ_ONLY" \
      -v panel_user="$PANEL_USER" '
    { gsub(/__HOST__/, host); gsub(/__PORT__/, port); gsub(/__USER__/, user);
      gsub(/__HASH__/, hash); gsub(/__ORIGIN__/, origin);
      gsub(/__RO__/, ro); gsub(/__PANEL_USER__/, panel_user); print }' <<'EOF' > "$dst"
host = "__HOST__"
port = __PORT__
admin_user = "__USER__"
password_hash = "__HASH__"
public_origin = "__ORIGIN__"
read_only = __RO__
tls_cert = ""
tls_key = ""
log_file = ""

# 最小特权 helper：root 级操作由独立的 lightpanel-helper 服务执行，
# 并按下列白名单授权。详见 README「最小特权 helper」。
[helper]
allowed_users = ["__PANEL_USER__"]
# 端口规则（ufw/firewalld，参数在 helper 端白名单重建）
allow_firewall = true
# 结束进程（SIGTERM/SIGKILL，进程身份经 pidfd 固定）
allow_kill = true
# 在线更新（helper 复核 SHA256 后安装并重启面板）
allow_update = true

# 按服务/动作细分授权：单元名 = 允许的 systemd 动作。
# 空列表 = 拒绝一切服务控制（最小特权默认）。按需添加，例如：
# [helper.services]
# "nginx.service" = ["start", "stop", "restart"]
# 或用 "*" = ["start", "stop", "restart"] 放开所有单元（旧版行为）。
[helper.services]
EOF
  chmod 0600 "$dst"
}

# 升级路径：旧配置没有 [helper] 段时追加默认段。为保持升级后服务控制
# 仍可用，先用 "*" 通配放开（等价旧版 root 行为），管理员可再收紧。
append_helper_config() {
  local dst="$INSTALL_DIR/config.toml"
  grep -q '^\[helper\]' "$dst" 2>/dev/null && return 0
  cat >> "$dst" <<EOF

[helper]
allowed_users = ["$PANEL_USER"]
allow_firewall = true
allow_kill = true
allow_update = true

# 升级默认：通配放开全部单元（旧版 root 行为）。建议改为按需授权，例如
# 删除 "*" 行并逐个列出单元。
[helper.services]
"*" = ["start", "stop", "restart"]
EOF
}

existing_hash() {
  awk -F'"' '/^password_hash[[:space:]]*=/{print $2; exit}' "$INSTALL_DIR/config.toml" 2>/dev/null || true
}

install_unit() {
  if [ "$LEGACY_ROOT" -eq 0 ]; then
    cat > "$UNIT" <<'EOF'
[Unit]
Description=LightPanel lightweight Linux management panel
After=network.target lightpanel-helper.service
Wants=lightpanel-helper.service

[Service]
Type=simple
# 最小特权模式：面板以专用非特权用户 lightpanel 运行，root 级操作
# （服务控制/防火墙/进程信号/在线更新）转发给 lightpanel-helper 服务，
# 并由 helper 按 [helper] 配置中的白名单（服务/动作）授权。
User=__PANEL_USER__
Group=__PANEL_USER__
__JOURNAL_GROUP__
WorkingDirectory=/opt/lightpanel
ExecStart=/opt/lightpanel/lightpanel -c /opt/lightpanel/config.toml
Restart=on-failure
RestartSec=3
TimeoutStopSec=15
UMask=0077
NoNewPrivileges=true
# 文件管理需要访问运行用户可读写的整个文件系统，因此不启用
# ProtectSystem/ProtectHome/PrivateTmp/ReadWritePaths 等文件系统隔离。
# 面板进程不需要任何 capability。
CapabilityBoundingSet=
AmbientCapabilities=
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
LimitNOFILE=1024
TasksMax=64
MemoryHigh=64M
MemoryMax=128M
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF
    if getent group systemd-journal >/dev/null 2>&1; then
      sed -i 's/^__JOURNAL_GROUP__$/SupplementaryGroups=systemd-journal/' "$UNIT"
    else
      sed -i '/^__JOURNAL_GROUP__$/d' "$UNIT"
    fi
    sed -i "s/__PANEL_USER__/$PANEL_USER/g" "$UNIT"
  else
    cat > "$UNIT" <<'EOF'
[Unit]
Description=LightPanel lightweight Linux management panel
After=network.target

[Service]
Type=simple
# Legacy root mode: the panel process itself runs as root and manages all
# system services/processes/firewall rules directly; [helper] is unused.
User=root
WorkingDirectory=/opt/lightpanel
ExecStart=/opt/lightpanel/lightpanel -c /opt/lightpanel/config.toml
Restart=on-failure
RestartSec=3
TimeoutStopSec=15
UMask=0077
NoNewPrivileges=true
# 文件管理需要访问整个文件系统（/home、/etc、/tmp 等），因此不启用
# ProtectSystem/ProtectHome/PrivateTmp/ReadWritePaths 等文件系统隔离。
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
LimitNOFILE=1024
TasksMax=64
MemoryHigh=64M
MemoryMax=128M
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF
  fi
  chmod 0644 "$UNIT"

  if [ "$LEGACY_ROOT" -eq 0 ]; then
    cat > "$HELPER_UNIT" <<'EOF'
[Unit]
Description=LightPanel privileged helper (least-privilege operations)
# 面板通过 /run/lightpanel/helper.sock 转发白名单内的特权请求。
After=network.target

[Service]
Type=simple
# helper 是唯一以 root 运行的组件：只暴露 [helper] 白名单内的操作
# （按服务/动作细分授权），并通过 SO_PEERCRED 校验连接者身份。
User=root
Group=root
RuntimeDirectory=lightpanel
RuntimeDirectoryMode=0755
ExecStart=/opt/lightpanel/lightpanel helper -c /opt/lightpanel/config.toml
Restart=on-failure
RestartSec=3
TimeoutStopSec=15
UMask=0077
NoNewPrivileges=true
# helper 需要写 /opt/lightpanel（在线更新换二进制）与 /etc/ufw（ufw 规则）；
# 其余文件系统一律只读。firewalld 走 D-Bus，不需要本地写权限。
ProtectSystem=strict
ReadWritePaths=/opt/lightpanel -/etc/ufw
ProtectHome=true
PrivateTmp=true
ProtectKernelTunables=true
ProtectControlGroups=true
RestrictSUIDSGID=true
LimitNOFILE=256
TasksMax=32
MemoryHigh=32M
MemoryMax=64M
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF
    chmod 0644 "$HELPER_UNIT"
  else
    rm -f "$HELPER_UNIT"
    systemctl disable "$HELPER_SERVICE_NAME" >/dev/null 2>&1 || true
  fi
  systemctl daemon-reload
  [ "$NO_START" -eq 1 ] && return 0
  systemctl enable "$SERVICE_NAME" >/dev/null 2>&1 || true
  if [ "$LEGACY_ROOT" -eq 0 ]; then
    systemctl enable "$HELPER_SERVICE_NAME" >/dev/null 2>&1 || true
    systemctl restart "$HELPER_SERVICE_NAME"
  fi
  systemctl restart "$SERVICE_NAME"
  sleep 1
  if systemctl is-active --quiet "$SERVICE_NAME"; then
    log "服务已启动并设置开机自启"
  else
    log "错误: 服务未在运行，最近日志如下（也可: journalctl -u $SERVICE_NAME -n 50 --no-pager）"
    journalctl -u "$SERVICE_NAME" -n 20 --no-pager || true
    fail "面板启动失败，请修正上述错误后重新启动"
  fi
  if [ "$LEGACY_ROOT" -eq 0 ]; then
    if systemctl is-active --quiet "$HELPER_SERVICE_NAME"; then
      log "helper 服务已启动（lightpanel-helper）"
    else
      journalctl -u "$HELPER_SERVICE_NAME" -n 20 --no-pager || true
      fail "helper 启动失败，服务控制等特权操作不可用"
    fi
  fi
}

create_panel_user() {
  if ! getent passwd "$PANEL_USER" >/dev/null 2>&1; then
    log "创建系统用户 $PANEL_USER（面板进程将以其身份运行）"
    useradd --system --home-dir "$INSTALL_DIR" --no-create-home \
      --shell /usr/sbin/nologin "$PANEL_USER" \
      || fail "创建系统用户 $PANEL_USER 失败"
  fi
}

# ---- 获取二进制 ----
if [ -n "$RELEASE" ]; then
  download_release
else
  build_from_source
fi

# ---- 安装 ----
if [ "$LEGACY_ROOT" -eq 0 ]; then
  create_panel_user
fi
log "安装目录 $INSTALL_DIR"
install -d -m 0750 "$INSTALL_DIR"
install -m 0755 "$WORK/lightpanel" "$INSTALL_DIR/lightpanel"

CONFIG_NEW=0
if [ -f "$INSTALL_DIR/config.toml" ] && [ "$FORCE_CONFIG" -eq 0 ]; then
  log "保留已有 $INSTALL_DIR/config.toml（重复执行即升级；重写配置请加 --force-config）"
  if [ -z "$(existing_hash)" ]; then
    log "已有配置缺少 password_hash，需要补设密码"
  fi
  if [ "$LEGACY_ROOT" -eq 0 ]; then
    append_helper_config
  fi
else
  CONFIG_NEW=1
fi

if [ "$CONFIG_NEW" -eq 1 ]; then
  if [ -z "$PASS_HASH" ]; then
    if [ -t 0 ]; then
      log "设置管理员密码（输入不会回显，12–72 字节）"
      PASS_HASH="$("$INSTALL_DIR/lightpanel" -hash-password)" || fail "密码哈希生成失败"
    else
      fail "非交互环境无法输入密码。请改用: sudo LP_PASS_HASH='<bcrypt 哈希>' bash $0（或 --password-hash）。" \
           "哈希可先在有终端的机器上用 $INSTALL_DIR/lightpanel -hash-password 生成。"
    fi
  fi
  write_config "$PASS_HASH"
  if [ "$LEGACY_ROOT" -eq 1 ]; then
    log "已写入 $INSTALL_DIR/config.toml（0600，仅 root 可读）"
  else
    log "已写入 $INSTALL_DIR/config.toml（0600；非 root 模式下见下方权限说明）"
  fi
fi

if [ "$LEGACY_ROOT" -eq 0 ]; then
  # 面板（lightpanel 用户）需要读取配置、执行二进制；二进制与配置本身仍归
  # root 所有，配置对运行用户只读。更新暂存目录归运行用户所有。
  chown root:"$PANEL_USER" "$INSTALL_DIR" 2>/dev/null || true
  chmod 0750 "$INSTALL_DIR"
  chown root:root "$INSTALL_DIR/lightpanel"
  chmod 0755 "$INSTALL_DIR/lightpanel"
  chown root:"$PANEL_USER" "$INSTALL_DIR/config.toml"
  chmod 0640 "$INSTALL_DIR/config.toml"
  install -d -o "$PANEL_USER" -g "$PANEL_USER" -m 0750 /var/lib/lightpanel/update
fi

# ---- systemd ----
if [ "$HAVE_SYSTEMD" -eq 1 ]; then
  install_unit
else
  log "手动启动命令: sudo -H $INSTALL_DIR/lightpanel -c $INSTALL_DIR/config.toml"
fi

# ---- 完成摘要 ----
FINAL_ORIGIN="${ORIGIN:-http://127.0.0.1:$PORT}"
if [ "$LEGACY_ROOT" -eq 0 ]; then
  MODE_NOTE="运行模式: 最小特权（面板用户 $PANEL_USER；root 操作由 lightpanel-helper 按白名单执行）
  服务控制授权: 编辑 $INSTALL_DIR/config.toml 的 [helper.services]（默认拒绝一切服务控制）"
else
  MODE_NOTE="运行模式: 旧版 root（--legacy-root；[helper] 不参与）"
fi
cat <<SUMMARY

[lightpanel] 安装完成
  二进制:   $INSTALL_DIR/lightpanel ($ARCH)
  配置:     $INSTALL_DIR/config.toml
  访问地址: $FINAL_ORIGIN
  $MODE_NOTE

远程访问（SSH 隧道，在本地机器执行）:
  ssh -N -L $PORT:127.0.0.1:$PORT <user>@<server>
  然后本地打开 $FINAL_ORIGIN

常用命令:
  systemctl status $SERVICE_NAME
  systemctl status $HELPER_SERVICE_NAME
  journalctl -u $SERVICE_NAME -f
  修改密码: $INSTALL_DIR/lightpanel -hash-password 后更新 config.toml 并 systemctl restart $SERVICE_NAME
SUMMARY
