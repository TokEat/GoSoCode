#!/usr/bin/env bash
#
# ops.sh — daily operations for GoSoCode server.
#
# Usage:
#   sudo ./scripts/ops.sh start|stop|restart|reload|status
#   sudo ./scripts/ops.sh logs [-f]
#   sudo ./scripts/ops.sh health
#   sudo ./scripts/ops.sh backup [name]
#   sudo ./scripts/ops.sh restore <backup-name>
#   sudo ./scripts/ops.sh db
#   sudo ./scripts/ops.sh unpack-clients <tarball>
#   sudo ./scripts/ops.sh rotate-logs
#
set -euo pipefail

GSC_ETC=${GSC_ETC:-/etc/gosocode}
GSC_SERVICE=${GSC_SERVICE:-gosocode}
ENV_FILE="${GSC_ETC}/env"

if [[ -f "$ENV_FILE" ]]; then
  # shellcheck source=/dev/null
  set -a
  # shellcheck source=/dev/null
  source "$ENV_FILE"
  set +a
fi

if [[ $EUID -ne 0 ]]; then
  die "请使用 root 权限运行本脚本"
fi

GSC_DB=${GSC_DB:-/var/lib/gosocode/gosocode.db}
GSC_DATA=${GSC_DATA:-$(dirname "$GSC_DB")}
GSC_LOGS=${GSC_LOGS:-/var/log/gosocode}
GSC_ADDR=${GSC_ADDR:-:8080}
GSC_BACKUPS="${GSC_DATA}/backups"
GSC_CLIENT_DIR=${GSC_CLIENT_DIR:-${GSC_DATA}/client-bin}
GSC_KEYS_DIR=${GSC_KEYS_DIR:-${GSC_DATA}/gsc-keys}

log() { printf '\033[1;34m[ops]\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m[ops]\033[0m %s\n' "$*" >&2; exit 1; }

health_url() {
  local addr="$GSC_ADDR"
  if [[ "$addr" == :* ]]; then
    addr="127.0.0.1${addr}"
  fi
  echo "http://${addr}/api/health"
}

cmd_status() {
  systemctl status "$GSC_SERVICE" --no-pager
}

cmd_start() {
  systemctl start "$GSC_SERVICE"
  cmd_status
}

cmd_stop() {
  systemctl stop "$GSC_SERVICE"
}

cmd_restart() {
  systemctl restart "$GSC_SERVICE"
  sleep 2
  cmd_health
}

cmd_reload() {
  systemctl reload-or-restart "$GSC_SERVICE"
}

cmd_logs() {
  local logfile="${GSC_LOGS}/server.log"
  if [[ -f "$logfile" ]]; then
    tail "$@" "$logfile"
  else
    journalctl -u "$GSC_SERVICE" "$@"
  fi
}

cmd_health() {
  local url
  url=$(health_url)
  if curl -fsS "$url" >/dev/null 2>&1; then
    log "服务健康: $url"
    curl -fsS "$url" 2>/dev/null
    echo
  else
    die "服务健康检查失败: $url"
  fi
}

cmd_backup() {
  mkdir -p "$GSC_BACKUPS"
  local name="${1:-$(date +%Y%m%d-%H%M%S)}"
  local dest="${GSC_BACKUPS}/${name}"
  [[ ! -d "$dest" ]] || die "备份已存在: $dest"
  mkdir -p "$dest"
  log "备份到 $dest ..."

  if command -v sqlite3 >/dev/null 2>&1; then
    sqlite3 "$GSC_DB" ".backup '${dest}/gosocode.db'"
  else
    cp -a "$GSC_DB" "${dest}/gosocode.db"
    log "警告: 未安装 sqlite3，使用文件拷贝备份"
  fi

  if [[ -d "$GSC_KEYS_DIR" ]]; then
    cp -a "$GSC_KEYS_DIR" "${dest}/gsc-keys"
  fi

  cp -a "$ENV_FILE" "${dest}/env"

  # keep last 14 backups
  find "$GSC_BACKUPS" -maxdepth 1 -mindepth 1 -type d -printf '%T@ %p\n' | \
    sort -rn | tail -n +15 | awk '{print $2}' | xargs -r rm -rf

  log "备份完成: $dest"
}

cmd_restore() {
  local name="${1:-}"
  [[ -n "$name" ]] || die "用法: $0 restore <backup-name>"
  local src="${GSC_BACKUPS}/${name}"
  [[ -d "$src" ]] || die "未找到备份: $src"

  log "即将从 $src 恢复，先创建当前状态备份..."
  cmd_backup "pre-restore-$(date +%Y%m%d-%H%M%S)"

  cmd_stop || true
  cp -a "${src}/gosocode.db" "$GSC_DB"
  if [[ -d "${src}/gsc-keys" ]]; then
    rm -rf "$GSC_KEYS_DIR"
    cp -a "${src}/gsc-keys" "$GSC_KEYS_DIR"
  fi
  chown -R "${GSC_USER:-gosocode}:${GSC_USER:-gosocode}" "$GSC_DATA"
  cmd_start
}

cmd_db() {
  command -v sqlite3 >/dev/null 2>&1 || die "未安装 sqlite3"
  sqlite3 "$GSC_DB"
}

cmd_unpack_clients() {
  local tarball="${1:-}"
  [[ -f "$tarball" ]] || die "用法: $0 unpack-clients <tarball>"
  mkdir -p "$GSC_CLIENT_DIR"
  tar -xzf "$tarball" -C "$GSC_CLIENT_DIR"
  chown -R "${GSC_USER:-gosocode}:${GSC_USER:-gosocode}" "$GSC_CLIENT_DIR"
  log "客户端二进制已解压到 $GSC_CLIENT_DIR"
  find "$GSC_CLIENT_DIR" -type f | sort
}

cmd_rotate_logs() {
  local logfile="${GSC_LOGS}/server.log"
  if [[ -f "$logfile" ]]; then
    > "$logfile"
    log "已清空 $logfile"
  fi
  if command -v journalctl >/dev/null 2>&1; then
    journalctl --vacuum-time=7d --unit="$GSC_SERVICE" >/dev/null
    log "已清理 7 天前的 journal 日志"
  fi
}

cmd_help() {
  sed -n '2,15p' "$0"
}

main() {
  local cmd="${1:-help}"
  shift || true
  case "$cmd" in
    start)       cmd_start ;;
    stop)        cmd_stop ;;
    restart)     cmd_restart ;;
    reload)      cmd_reload ;;
    status)      cmd_status ;;
    logs)        cmd_logs "$@" ;;
    health)      cmd_health ;;
    backup)      cmd_backup "$@" ;;
    restore)     cmd_restore "$@" ;;
    db)          cmd_db ;;
    unpack-clients) cmd_unpack_clients "$@" ;;
    rotate-logs) cmd_rotate_logs ;;
    help|--help|-h) cmd_help ;;
    *) die "未知命令: $cmd" ;;
  esac
}

main "$@"
