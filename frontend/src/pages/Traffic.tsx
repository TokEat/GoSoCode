import { useEffect, useState } from "react";
import { api, openProgressSocket, Traffic as T, TrafficPoint } from "../lib/api";
import { Card, Stat } from "../ui";
import { humanSize, humanRate } from "../lib/format";

const UP_COLOR = "#6366f1";
const DOWN_COLOR = "#0ea5e9";

function Legend() {
  return (
    <div className="row" style={{ gap: 14, fontSize: 12, color: "#7a8194" }}>
      <span><span style={{ display: "inline-block", width: 10, height: 10, background: UP_COLOR, borderRadius: 2, marginRight: 4 }} />上传</span>
      <span><span style={{ display: "inline-block", width: 10, height: 10, background: DOWN_COLOR, borderRadius: 2, marginRight: 4 }} />下载</span>
    </div>
  );
}

// RateChart draws the live per-second throughput as two area lines (SVG, no
// deps) with a labelled Y axis (rate units follow the current peak).
function RateChart({ points }: { points: TrafficPoint[] }) {
  const W = 600, H = 160, PADL = 54, PADR = 6, PADT = 8, PADB = 4;
  const max = Math.max(1, ...points.map((p) => Math.max(p.up, p.down)));
  const x = (i: number) => PADL + (i / Math.max(1, points.length - 1)) * (W - PADL - PADR);
  const y = (v: number) => H - PADB - (v / max) * (H - PADT - PADB);
  const line = (get: (p: TrafficPoint) => number) =>
    points.map((p, i) => `${x(i).toFixed(1)},${y(get(p)).toFixed(1)}`).join(" ");
  const area = (get: (p: TrafficPoint) => number) =>
    `${PADL},${H - PADB} ${line(get)} ${W - PADR},${H - PADB}`;
  const fmtT = (t: number) => new Date(t * 1000).toLocaleTimeString("zh-CN", { hour12: false });
  return (
    <div>
      <div className="row" style={{ justifyContent: "space-between", marginBottom: 6 }}>
        <Legend />
        <span style={{ fontSize: 12, color: "#7a8194" }}>峰值 {humanRate(max)}</span>
      </div>
      <svg viewBox={`0 0 ${W} ${H}`} style={{ width: "100%", height: 160, display: "block", background: "rgba(255,255,255,0.45)", borderRadius: 8 }}>
        {[0.25, 0.5, 0.75, 1].map((r) => {
          const gy = y(max * r);
          return (
            <g key={r}>
              <line x1={PADL} x2={W - PADR} y1={gy} y2={gy} stroke="#e2e8f0" strokeWidth="1" />
              <text x={PADL - 6} y={gy + 3} fontSize="9" fill="#94a3b8" textAnchor="end">{humanRate(max * r)}</text>
            </g>
          );
        })}
        <text x={PADL - 6} y={H - PADB} fontSize="9" fill="#94a3b8" textAnchor="end">0</text>
        <polygon points={area((p) => p.down)} fill={DOWN_COLOR} opacity="0.12" />
        <polyline points={line((p) => p.down)} fill="none" stroke={DOWN_COLOR} strokeWidth="1.5" />
        <polygon points={area((p) => p.up)} fill={UP_COLOR} opacity="0.12" />
        <polyline points={line((p) => p.up)} fill="none" stroke={UP_COLOR} strokeWidth="1.5" />
      </svg>
      {points.length > 0 && (
        <div className="row" style={{ justifyContent: "space-between", fontSize: 11, color: "#94a3b8", marginTop: 2 }}>
          <span style={{ marginLeft: `${(PADL / W) * 100}%` }}>{fmtT(points[0].t)}</span>
          <span>{fmtT(points[points.length - 1].t)}</span>
        </div>
      )}
    </div>
  );
}

// HourlyChart draws the last-24h per-hour totals as grouped bars with a
// labelled Y axis (per-hour volume). The data comes from the persisted
// transfer log, so it survives server restarts.
function HourlyChart({ points }: { points: TrafficPoint[] }) {
  const W = 600, H = 160, PADL = 54, PADR = 6, PADT = 8, PADB = 4, GAP = 2;
  const max = Math.max(1, ...points.map((p) => Math.max(p.up, p.down)));
  const baseY = H - PADB - 12; // bars sit above the hour-label band
  const plotH = baseY - PADT;
  const group = (W - PADL - PADR) / Math.max(1, points.length);
  const barW = Math.max(2, (group - GAP) / 2);
  const h = (v: number) => (v / max) * plotH;
  const hourOf = (t: number) => new Date(t * 1000).getHours();
  return (
    <div>
      <div className="row" style={{ justifyContent: "space-between", marginBottom: 6 }}>
        <Legend />
        <span style={{ fontSize: 12, color: "#7a8194" }}>最高 {humanSize(max)}/时</span>
      </div>
      <svg viewBox={`0 0 ${W} ${H}`} style={{ width: "100%", height: 160, display: "block", background: "rgba(255,255,255,0.45)", borderRadius: 8 }}>
        {[0.25, 0.5, 0.75, 1].map((r) => {
          const gy = baseY - plotH * r;
          return (
            <g key={r}>
              <line x1={PADL} x2={W - PADR} y1={gy} y2={gy} stroke="#e2e8f0" strokeWidth="1" />
              <text x={PADL - 6} y={gy + 3} fontSize="9" fill="#94a3b8" textAnchor="end">{humanSize(max * r)}</text>
            </g>
          );
        })}
        <text x={PADL - 6} y={baseY + 3} fontSize="9" fill="#94a3b8" textAnchor="end">0</text>
        {points.map((p, i) => {
          const gx = PADL + i * group;
          return (
            <g key={p.t}>
              <rect x={gx} y={baseY - h(p.up)} width={barW} height={h(p.up)} fill={UP_COLOR} rx="1" />
              <rect x={gx + barW + 1} y={baseY - h(p.down)} width={barW} height={h(p.down)} fill={DOWN_COLOR} rx="1" />
              {hourOf(p.t) % 4 === 0 && (
                <text x={gx + barW} y={H - 3} fontSize="9" fill="#94a3b8" textAnchor="middle">{hourOf(p.t)}时</text>
              )}
            </g>
          );
        })}
      </svg>
    </div>
  );
}

const INTERVALS = [
  { v: 30, label: "30 秒" },
  { v: 60, label: "1 分钟" },
  { v: 120, label: "2 分钟" },
  { v: 300, label: "5 分钟" },
];

export default function Traffic() {
  const [t, setT] = useState<T | null>(null);
  const [wsLive, setWsLive] = useState<T["live"] | null>(null);
  const [interval_, setInterval_] = useState(30); // chart sampling bucket (seconds)

  // Realtime stats arrive over WebSocket (server pushes every 2s); the heavier
  // chart/table data is refreshed by a slow poll — buckets are >=30s anyway.
  useEffect(() => {
    const ws = openProgressSocket((e) => {
      if (e?.type === "traffic" && e.live) setWsLive(e.live);
    });
    return () => ws.close();
  }, []);

  useEffect(() => {
    const load = () => api.traffic(interval_).then(setT).catch(() => {});
    load();
    const id = setInterval(load, 30000);
    return () => clearInterval(id);
  }, [interval_]);

  const live = wsLive || t?.live;
  const hourTotal = (t?.hourly || []).reduce((s, p) => ({ up: s.up + p.up, down: s.down + p.down }), { up: 0, down: 0 });
  return (
    <div className="grid" style={{ gap: 20 }}>
      <div className="grid cols-4">
        <Stat label="实时上传速度" value={humanRate(live?.up_bps ?? 0)} sub="近 10 秒平均" />
        <Stat label="实时下载速度" value={humanRate(live?.down_bps ?? 0)} sub="近 10 秒平均" />
        <Stat label="24 小时上传" value={humanSize(hourTotal.up)} sub="近 24 小时总量" />
        <Stat label="24 小时下载" value={humanSize(hourTotal.down)} sub="近 24 小时总量" />
      </div>

      <Card
        title={`实时流量（近 ${Math.round((interval_ * 60 > 3600 ? 3600 : interval_ * 60) / 60)} 分钟，每 ${INTERVALS.find((i) => i.v === interval_)?.label || `${interval_} 秒`}采样）`}
        action={
          <div className="row" style={{ gap: 6 }}>
            {INTERVALS.map((i) => (
              <button key={i.v} className={`btn sm ${interval_ === i.v ? "" : "ghost"}`}
                onClick={() => setInterval_(i.v)}>{i.label}</button>
            ))}
          </div>
        }
      >
        {t?.series?.length ? <RateChart points={t.series} /> : <div className="empty">暂无数据</div>}
      </Card>

      <Card title="近 24 小时流量统计（按小时）">
        {t?.hourly?.length ? <HourlyChart points={t.hourly} /> : <div className="empty">暂无数据</div>}
      </Card>

      <div className="grid cols-2" style={{ gridTemplateColumns: "1fr 1fr" }}>
        <Card title="按存储节点">
          <table>
            <thead><tr><th>节点</th><th>上传</th><th>下载</th></tr></thead>
            <tbody>
              {t?.nodes.map((r) => (
                <tr key={r.id}><td>#{r.id} {r.label}</td><td>{humanSize(r.bytes_up)}</td><td>{humanSize(r.bytes_down)}</td></tr>
              ))}
              {(!t || t.nodes.length === 0) && <tr><td colSpan={3} className="empty">暂无数据</td></tr>}
            </tbody>
          </table>
        </Card>
        <Card title="按用户">
          <table>
            <thead><tr><th>用户</th><th>上传</th><th>下载</th></tr></thead>
            <tbody>
              {t?.users.map((r) => (
                <tr key={r.id}><td>#{r.id} {r.label}</td><td>{humanSize(r.bytes_up)}</td><td>{humanSize(r.bytes_down)}</td></tr>
              ))}
              {(!t || t.users.length === 0) && <tr><td colSpan={3} className="empty">暂无数据</td></tr>}
            </tbody>
          </table>
        </Card>
      </div>
    </div>
  );
}
