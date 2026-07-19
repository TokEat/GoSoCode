import { useState } from "react";
import { useTransfers, TransferStatus } from "./transfer";
import { humanRate } from "./lib/format";

const LABEL: Record<TransferStatus, string> = {
  running: "传输中",
  paused: "已暂停",
  finalizing: "云端处理中",
  done: "完成",
  error: "失败",
  canceled: "已取消",
};

// Docked at the bottom of the main area so transfer progress stays visible
// while browsing other pages (e.g. checking Files for a finished upload).
// Renders nothing until the current user has transfer tasks; collapsible to a
// one-line summary strip.
export default function TransferDrawer({ hidden = false, onViewAll }: { hidden?: boolean; onViewAll: () => void }) {
  const { transfers } = useTransfers();
  const [open, setOpen] = useState(true);

  if (hidden || transfers.length === 0) return null;

  const active = transfers.filter((t) => t.status === "running" || t.status === "paused" || t.status === "finalizing");
  const speed = active.reduce((s, t) => s + (t.status === "running" ? t.speed : 0), 0);
  const summary = active.length > 0 ? `${active.length} 个进行中` : "已全部结束";

  return (
    <div className="transfer-drawer">
      <div className="transfer-drawer-head" onClick={() => setOpen(!open)}>
        <span className="transfer-drawer-title">
          ⇅ 传输任务 ({transfers.length}) · {summary}{speed > 0 ? ` · ${humanRate(speed)}` : ""}
        </span>
        <span className="row" style={{ gap: 8 }}>
          <button className="btn ghost sm" onClick={(e) => { e.stopPropagation(); onViewAll(); }}>查看全部</button>
          <button className="btn ghost sm" onClick={(e) => { e.stopPropagation(); setOpen(!open); }}>{open ? "收起 ▾" : "展开 ▴"}</button>
        </span>
      </div>
      {open && (
        <div className="transfer-drawer-body">
          {transfers.map((t) => {
            const pct = t.total > 0 ? Math.min(100, Math.round((t.transferred / t.total) * 100)) : (t.status === "done" ? 100 : 0);
            const color = t.status === "error" ? "#dc2626" : t.status === "done" ? "#16a34a" : t.status === "paused" ? "#d97706" : "#6366f1";
            return (
              <div key={t.id} className="transfer-drawer-row">
                <span className={`drawer-kind ${t.kind}`}>{t.kind === "upload" ? "↑" : "↓"}</span>
                <span className="drawer-name" title={t.path}>{t.name}</span>
                <div className="bar drawer-bar"><span style={{ width: `${pct}%`, background: color }} /></div>
                <span className="drawer-pct">{pct}%</span>
                <span className="drawer-info" title={t.error || t.note || undefined}>
                  {t.status === "error"
                    ? (t.error || LABEL.error)
                    : t.status === "running"
                      ? (t.speed > 0 ? humanRate(t.speed) : (t.note || LABEL.running))
                      : LABEL[t.status]}
                </span>
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}
