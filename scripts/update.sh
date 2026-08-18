#!/usr/bin/env bash
#
# update.sh — update GoSoCode server with rollback support.
#
# Usage:
#   sudo ./scripts/update.sh                  # rebuild from current git checkout
#   sudo ./scripts/update.sh --release v1.2.3 # download release binary
#   sudo ./scripts/update.sh --release latest # use GitHub latest rolling build
#   sudo ./scripts/update.sh --rollback       # revert to pre-update backup
#
set -euo pipefail

GSC_ETC=${GSC_ETC:-/etc/gosocode}
GSC_SERVICE=${GSC_SERVICE:-gosocode}
GSC_HOME=${GSC_HOME:-/opt/gosocode}
ENV_FILE="${GSC_ETC}/env"

if [[ -f "$ENV_FILE" ]]; then
  set -a
  # shellcheck source=/dev/null
  source "$ENV_FILE"
  set +a
fi

GSC_DB=${GSC_DB:-/var/lib/gosocode/gosocode.db}
GSC_DATA=${GSC_DATA:-$(dirname "$GSC_DB")}
GSC_BACKUPS="${GSC_DATA}/backups"
GSC_ADDR=${GSC_ADDR:-:8080}

REPO_URL=${REPO_URL:-https://github.com/Yyjccc/GoSoCode}
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
REPO_ROOT=$(cd "${SCRIPT_DIR}/.." && pwd)

log() { printf '\033[1;36m[update]\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m[update]\033[0m %s\n' "$*" >&2; exit 1; }

health_url() {
  local addr="$GSC_ADDR"
  if [[ "$addr" == :* ]]; then addr="127.0.0.1${addr}"; fi
  echo "http://${addr}/api/health"
}

wait_healthy() {
  local url max=30 i
  url=$(health_url)
  for i in $(seq 1 "$max"); do
    if curl -fsS "$url" >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done
  return 1
}

mode="source"
target=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --release)
      mode="release"
      target="${2:-latest}"
      shift 2
      ;;
    --rollback)
      mode="rollback"
      shift
      ;;
    --source)
      mode="source"
      shift
      ;;
    --help|-h)
      sed -n '2,12p' "$0"
      exit 0
      ;;
    *)
      die "未知参数: $1"
      ;;
  esac
done

BIN="${GSC_HOME}/gosocode-server"
BIN_PREV="${GSC_HOME}/gosocode-server.prev"

backup_state() {
  local tag="pre-update-$(date +%Y%m%d-%H%M%S)"
  "${SCRIPT_DIR}/ops.sh" backup "$tag"
  cp -a "$BIN" "$BIN_PREV" 2>/dev/null || true
  echo "$tag"
}

rollback() {
  log "开始回滚..."
  local tag=""
  # find the most recent pre-update backup
  tag=$(find "$GSC_BACKUPS" -maxdepth 1 -mindepth 1 -type d -name 'pre-update-*' -printf '%T@ %p\n' | sort -rn | head -n1 | awk '{print $2}')
  [[ -n "$tag" ]] || die "找不到可回滚的备份"
  log "使用备份: $tag"
  systemctl stop "$GSC_SERVICE" || true
  cp -a "${tag}/gosocode.db" "$GSC_DB"
  if [[ -f "$BIN_PREV" ]]; then
    cp -a "$BIN_PREV" "$BIN"
  fi
  chown -R "${GSC_USER:-gosocode}:${GSC_USER:-gosocode}" "$GSC_DATA"
  systemctl start "$GSC_SERVICE"
  if wait_healthy; then
    log "回滚成功，服务已恢复"
  else
    die "回滚后服务仍不健康，请人工检查"
  fi
}

update_source() {
  # Running as root against a user-owned worktree requires marking it safe.
  git config --global --add safe.directory "$REPO_ROOT" >/dev/null 2>&1 || true
  cd "$REPO_ROOT"
  if [[ -n "$(git status --porcelain 2>/dev/null)" ]]; then
    die "工作区存在未提交修改，请先提交或清理后再更新"
  fi

  local backup_tag
  backup_tag=$(backup_state)
  log "当前备份: $backup_tag"

  log "拉取最新代码..."
  git pull origin "$(git rev-parse --abbrev-ref HEAD)"

  log "重新构建..."
  cd "${REPO_ROOT}/frontend"
  npm ci
  npm run build
  rm -rf "${REPO_ROOT}/backend/internal/webui/static"/*
  cp -r "${REPO_ROOT}/frontend/dist/." "${REPO_ROOT}/backend/internal/webui/static/"

  cd "${REPO_ROOT}/backend"
  go build -trimpath -buildvcs=false -ldflags="-s -w" -o "${REPO_ROOT}/dist/gosocode-server" ./cmd/server

  log "替换二进制并重启..."
  systemctl stop "$GSC_SERVICE" || true
  install -m 755 "${REPO_ROOT}/dist/gosocode-server" "$BIN"
  chown "${GSC_USER:-gosocode}:${GSC_USER:-gosocode}" "$BIN"
  systemctl start "$GSC_SERVICE"

  if wait_healthy; then
    log "源码更新完成，服务健康"
  else
    die "更新后服务不健康，正在自动回滚..." ; rollback
  fi
}

update_release() {
  local tag="$target"
  [[ "$tag" == latest ]] || tag="${target}"

  local arch os
  arch=$(uname -m)
  case "$arch" in
    x86_64) arch="amd64" ;;
    aarch64|arm64) arch="arm64" ;;
    *) die "不支持的架构: $arch" ;;
  esac
  os=$(uname -s | tr '[:upper:]' '[:lower:]')
  [[ "$os" == linux ]] || die "release 更新仅支持 Linux"

  local dl_base="${REPO_URL}/releases/download/${tag}"
  local server_asset="gosocode-server-${os}-${arch}"
  local tmpdir
  tmpdir=$(mktemp -d)
  trap 'rm -rf "$tmpdir"' EXIT

  local backup_tag
  backup_tag=$(backup_state)
  log "当前备份: $backup_tag"

  log "下载 release 二进制: ${dl_base}/${server_asset}"
  curl -fsSL "${dl_base}/${server_asset}" -o "${tmpdir}/gosocode-server" || die "下载失败"
  chmod +x "${tmpdir}/gosocode-server"

  log "替换二进制并重启..."
  systemctl stop "$GSC_SERVICE" || true
  install -m 755 "${tmpdir}/gosocode-server" "$BIN"
  chown "${GSC_USER:-gosocode}:${GSC_USER:-gosocode}" "$BIN"
  systemctl start "$GSC_SERVICE"

  if wait_healthy; then
    log "Release 更新完成，服务健康"
  else
    die "更新后服务不健康，正在自动回滚..." ; rollback
  fi
}

if [[ $EUID -ne 0 ]]; then
  die "请使用 root 权限运行"
fi

if ! command -v go >/dev/null 2>&1 && [[ -x /usr/local/go/bin/go ]]; then
  export PATH="${PATH}:/usr/local/go/bin"
fi

# Allow overriding the Go module proxy.
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"

case "$mode" in
  source)  update_source ;;
  release) update_release ;;
  rollback) rollback ;;
esac
