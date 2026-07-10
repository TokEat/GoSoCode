export function humanSize(n: number): string {
  if (!n) return "0 B";
  const u = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return `${n.toFixed(i === 0 ? 0 : 1)} ${u[i]}`;
}

export function humanRate(bps: number): string {
  return `${humanSize(bps)}/s`;
}

export function humanDuration(sec: number): string {
  if (!isFinite(sec) || sec < 0) return "—";
  if (sec < 10) return "数秒";
  if (sec < 60) return `${Math.round(sec)} 秒`;
  if (sec < 3600) {
    const m = Math.floor(sec / 60), s = Math.round(sec % 60);
    return s > 0 ? `${m} 分 ${s} 秒` : `${m} 分钟`;
  }
  const h = Math.floor(sec / 3600), m = Math.round((sec % 3600) / 60);
  return m > 0 ? `${h} 小时 ${m} 分` : `${h} 小时`;
}

export function fmtTime(s: string): string {
  const d = new Date(s);
  if (isNaN(d.getTime())) return s;
  return d.toLocaleString();
}
