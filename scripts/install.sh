#!/usr/bin/env bash
#
# install.sh — deploy GoSoCode server from the local source tree.
#
# Run as root on a systemd-based Linux host.
#
# Quick start:
#   sudo ./scripts/install.sh
#
# Environment overrides (all optional):
#   GSC_USER        service user (default: gosocode)
#   GSC_HOME        install directory for binaries (default: /opt/gosocode)
#   GSC_DATA        runtime data dir / home dir (default: /var/lib/gosocode)
#   GSC_LOGS        log directory (default: /var/log/gosocode)
#   GSC_ETC         config directory (default: /etc/gosocode)
#   GSC_SERVICE     systemd service name (default: gosocode)
#   GSC_SERVICE_FILE path to systemd unit file (default: /etc/systemd/system/gosocode.service)
#   GSC_ADDR        listen address (default: :8080)
#   GSC_JWT_SECRET  JWT signing secret (auto-generated if unset)
#   GSC_REDIS       redis address for distributed coordination (default: empty)
#   GSC_APP_NAME    baidu app sandbox name (default: gosocode)
#   GSC_REDIRECT_URI oauth redirect uri (default: http://localhost:8080/api/oauth/callback)
#   GSC_START       whether to start & enable the service (default: true)
#
set -euo pipefail

GSC_USER=${GSC_USER:-gosocode}
GSC_HOME=${GSC_HOME:-/opt/gosocode}
GSC_DATA=${GSC_DATA:-/var/lib/gosocode}
GSC_LOGS=${GSC_LOGS:-/var/log/gosocode}
GSC_ETC=${GSC_ETC:-/etc/gosocode}
GSC_SERVICE=${GSC_SERVICE:-gosocode}
GSC_SERVICE_FILE=${GSC_SERVICE_FILE:-/etc/systemd/system/${GSC_SERVICE}.service}
GSC_ADDR=${GSC_ADDR:-:8080}
GSC_APP_NAME=${GSC_APP_NAME:-gosocode}
GSC_REDIS=${GSC_REDIS:-}
GSC_REDIRECT_URI=${GSC_REDIRECT_URI:-http://localhost:8080/api/oauth/callback}
GSC_START=${GSC_START:-true}

# Secrets are generated automatically when not provided.
if [[ -z "${GSC_JWT_SECRET:-}" ]]; then
  GSC_JWT_SECRET=$(openssl rand -hex 32)
fi
if [[ -z "${GSC_AGENT_CONTROL_KEY:-}" ]]; then
  GSC_AGENT_CONTROL_KEY=$(openssl rand -hex 32)
fi

GSC_DB="${GSC_DATA}/gosocode.db"
GSC_KEYS_DIR="${GSC_DATA}/gsc-keys"
GSC_CLIENT_DIR="${GSC_DATA}/client-bin"

log() { printf '\033[1;32m[install]\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m[install]\033[0m %s\n' "$*" >&2; exit 1; }

if [[ $EUID -ne 0 ]]; then
  die "请使用 root 权限运行本脚本 (sudo ./scripts/install.sh)"
fi

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
REPO_ROOT=$(cd "${SCRIPT_DIR}/.." && pwd)

[[ -f "${REPO_ROOT}/backend/go.mod" ]] || die "未找到 backend/go.mod，请在仓库根目录执行脚本"
[[ -f "${REPO_ROOT}/frontend/package.json" ]] || die "未找到 frontend/package.json"

# Ensure Go is available even if /etc/profile.d was not sourced.
if ! command -v go >/dev/null 2>&1 && [[ -x /usr/local/go/bin/go ]]; then
  export PATH="${PATH}:/usr/local/go/bin"
fi

for cmd in go node npm git; do
  command -v "$cmd" >/dev/null 2>&1 || die "缺少依赖: $cmd"
done

# Allow overriding the Go module proxy; goproxy.cn is a sensible default
# for networks with limited access to proxy.golang.org.
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"

GO_VERSION=$(go version | awk '{print $3}' | sed 's/^go//')
[[ "$(printf '%s\n' "$GO_VERSION" "1.25" | sort -V | head -n1)" = "1.25" ]] || die "Go 版本需 >= 1.25，当前: $GO_VERSION"

log "创建运行用户与目录..."
if ! id -u "$GSC_USER" >/dev/null 2>&1; then
  useradd -r -s /usr/sbin/nologin -d "$GSC_DATA" "$GSC_USER"
fi

mkdir -p "$GSC_HOME" "$GSC_DATA" "$GSC_LOGS" "$GSC_ETC" \
         "${GSC_DATA}/backups" "$GSC_CLIENT_DIR" "$GSC_KEYS_DIR"
chown -R "${GSC_USER}:${GSC_USER}" "$GSC_DATA" "$GSC_LOGS" "$GSC_CLIENT_DIR" "$GSC_KEYS_DIR"
chmod 750 "$GSC_DATA" "$GSC_LOGS"

log "构建前端..."
cd "${REPO_ROOT}/frontend"
npm ci
npm run build

mkdir -p "${REPO_ROOT}/backend/internal/webui/static"
rm -rf "${REPO_ROOT}/backend/internal/webui/static"/*
cp -r "${REPO_ROOT}/frontend/dist/." "${REPO_ROOT}/backend/internal/webui/static/"

log "构建后端..."
cd "${REPO_ROOT}/backend"
go build -trimpath -buildvcs=false -ldflags="-s -w" -o "${REPO_ROOT}/dist/gosocode-server" ./cmd/server
go build -trimpath -buildvcs=false -ldflags="-s -w" -o "${REPO_ROOT}/dist/gosocode-client" ./cmd/client

log "安装二进制..."
install -m 755 "${REPO_ROOT}/dist/gosocode-server" "${GSC_HOME}/gosocode-server"
install -m 755 "${REPO_ROOT}/dist/gosocode-client" "${GSC_HOME}/gosocode-client"
chown -R "${GSC_USER}:${GSC_USER}" "$GSC_HOME"

log "写入环境配置 ${GSC_ETC}/env ..."
cat > "${GSC_ETC}/env" <<EOF
# GoSoCode runtime configuration
# 修改后执行: systemctl restart ${GSC_SERVICE}

GSC_ENV=production
GSC_ADDR=${GSC_ADDR}
GSC_DB=${GSC_DB}
GSC_JWT_SECRET=${GSC_JWT_SECRET}
GSC_REDIS=${GSC_REDIS}
GSC_APP_NAME=${GSC_APP_NAME}
GSC_BAIDU_APPKEY=${GSC_BAIDU_APPKEY:-}
GSC_BAIDU_SECRET=${GSC_BAIDU_SECRET:-}
GSC_REDIRECT_URI=${GSC_REDIRECT_URI}
GSC_KEYS_DIR=${GSC_KEYS_DIR}
GSC_CLIENT_DIR=${GSC_CLIENT_DIR}
GSC_AGENT_ENABLED=1
GSC_AGENT_CONTROL_KEY=${GSC_AGENT_CONTROL_KEY}
GSC_STREAM_ENABLED=1
GSC_STREAM_MAX_CONCURRENT=8
GSC_STREAM_WINDOW=3
GSC_DAILY_RECONCILE=1
GSC_TIMEZONE=Asia/Shanghai
EOF
chmod 640 "${GSC_ETC}/env"
chown "${GSC_USER}:${GSC_USER}" "${GSC_ETC}/env"

log "写入 systemd service ${GSC_SERVICE_FILE} ..."
cat > "$GSC_SERVICE_FILE" <<EOF
[Unit]
Description=GoSoCode server
After=network.target

[Service]
Type=simple
User=${GSC_USER}
Group=${GSC_USER}
WorkingDirectory=${GSC_DATA}
EnvironmentFile=${GSC_ETC}/env
ExecStart=${GSC_HOME}/gosocode-server
ExecReload=/bin/kill -HUP \$MAINPID
Restart=on-failure
RestartSec=5
StandardOutput=append:${GSC_LOGS}/server.log
StandardError=append:${GSC_LOGS}/server.log

[Install]
WantedBy=multi-user.target
EOF
chmod 644 "$GSC_SERVICE_FILE"

if [[ "$GSC_START" == "true" ]] && [[ "$GSC_SERVICE_FILE" == /etc/systemd/system/* ]]; then
  log "加载 systemd 并启动服务..."
  systemctl daemon-reload
  systemctl enable "$GSC_SERVICE"
  systemctl restart "$GSC_SERVICE"
  sleep 2
  systemctl status "$GSC_SERVICE" --no-pager || true
else
  log "已跳过 systemd 启动。手动启动请运行:"
  echo "  systemctl daemon-reload"
  echo "  systemctl enable --now ${GSC_SERVICE}"
fi

cat <<EOF

========================================
GoSoCode 部署完成
========================================
安装目录:     ${GSC_HOME}
数据目录:     ${GSC_DATA}
日志目录:     ${GSC_LOGS}
配置文件:     ${GSC_ETC}/env
服务名称:     ${GSC_SERVICE}
监听地址:     ${GSC_ADDR}

运维命令:
  ./scripts/ops.sh status
  ./scripts/ops.sh logs -f
  ./scripts/ops.sh backup

首次使用:
1. 在 ${GSC_ETC}/env 中配置 GSC_REDIRECT_URI（需与百度应用一致）
2. 登录后进入「管理 → 系统设置」填写百度 AppKey / SecretKey
3. 进入「管理 → 账号池」添加百度网盘节点
4. 生产环境务必通过 nginx/HTTPS 反向代理，并修改 GSC_JWT_SECRET

客户端分发（可选）:
  ./scripts/ops.sh unpack-clients gosocode-client-binaries.tar.gz
========================================
EOF
