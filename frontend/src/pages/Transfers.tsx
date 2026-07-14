import { useEffect, useState } from "react";
import { api, TransferLogItem } from "../lib/api";
import { Card, Badge, useToast } from "../ui";
import { humanSize, humanRate, humanDuration, fmtTime } from "../lib/format";
import { useTransfers, TransferStatus } from "../transfer";

const STATUS: Record<TransferStatus, { label: string; kind: string }> = {
  running: { label: "传输中", kind: "blue" },
  paused: { label: "已暂停", kind: "amber" },
  finalizing: { label: "云端处理中", kind: "amber" },
  done: { label: "完成", kind: "green" },
  error: { label: "失败", kind: "red" },
  canceled: { label: "已取消", kind: "gray" },
};

export default function Transfers() {
  const { clientMode, transfers, pause, resume, cancel, remove, clearFinished } = useTransfers();
  const toast = useToast();
  const [history, setHistory] = useState<TransferLogItem[]>([]);

  const loadHistory = () => {
    if (clientMode) return Promise.resolve();
    return api.transferHistory().then(setHistory).catch(() => {});
  };
  useEffect(() => { if (!clientMode) loadHistory(); }, [clientMode]);
  // Refresh history whenever a transfer completes.
  const doneCount = transfers.filter((t) => t.status === "done").length;
  useEffect(() => { if (!clientMode) loadHistory(); }, [clientMode, doneCount]);

  return (
    <div className="grid" style={{ gap: 20 }}>
    {clientMode && (
      <div className="client-transfer-note">
        <span className="client-transfer-pulse" />
        <div><b>浏览器仅显示进度</b><small>上传从本地磁盘直达云端，下载由客户端直接写入设置的本地目录。</small></div>
      </div>
    )}
    <Card
      title={`${clientMode ? "本机传输任务" : "传输任务"} (${transfers.length})`}
      action={<button className="btn ghost" onClick={() => { clearFinished(); toast("ok", "已清除完成的任务"); }}>清除已完成</button>}
    >
      <table>
        <thead>
          <tr><th>类型</th><th>名称</th><th style={{ width: 200 }}>进度</th><th style={{ width: 120 }}>速度</th><th style={{ width: 170 }}>大小</th><th style={{ width: 120 }}>状态</th><th style={{ width: 170 }}></th></tr>
        </thead>
        <tbody>
          {transfers.map((t) => {
            const pct = t.total > 0 ? Math.min(100, Math.round((t.transferred / t.total) * 100)) : (t.status === "done" ? 100 : 0);
            const barColor = t.status === "error" ? "#dc2626" : t.status === "paused" ? "#d97706" : t.status === "done" ? "#16a34a" : "#6366f1";
            const s = STATUS[t.status];
            // streaming uploads carry a second progress: bytes confirmed at the cloud
            const isStream = t.kind === "upload" && t.synced !== undefined;
            const syncPct = isStream && t.total > 0 ? Math.min(100, Math.round(((t.synced || 0) / t.total) * 100)) : 0;
            return (
              <tr key={t.id}>
                <td><Badge kind={t.kind === "upload" ? "blue" : "green"}>{t.kind === "upload" ? "上传" : "下载"}</Badge></td>
                <td title={t.path} style={{ maxWidth: 260, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{t.name}</td>
                <td>
                  <div className="bar"><span style={{ width: `${pct}%`, background: barColor }} /></div>
                  {isStream && (
                    <div className="bar" style={{ marginTop: 3, height: 4 }}>
                      <span style={{ width: `${syncPct}%`, background: t.status === "error" ? "#dc2626" : "#0ea5e9" }} />
                    </div>
                  )}
                  <span className="muted" style={{ fontSize: 12 }}>
                    {isStream ? `已上传 ${pct}% · 云端已保存 ${syncPct}%` : `${pct}%`}
                  </span>
                </td>
                <td className="muted">
                  {t.status === "running" ? (
                    <div style={{ fontSize: 12, lineHeight: 1.6 }}>
                      {isStream ? (
                        <>
                          <div>↑ {humanRate(t.speed)}</div>
                          <div style={{ color: "#0ea5e9" }}>☁ {humanRate(t.syncSpeed || 0)}</div>
                        </>
                      ) : (
                        <div>{humanRate(t.speed)}</div>
                      )}
                      {t.eta !== undefined && (
                        <div style={{ color: "#94a3b8", fontSize: 11 }}>剩余 {humanDuration(t.eta)}</div>
                      )}
                    </div>
                  ) : "—"}
                </td>
                <td className="muted">{humanSize(t.transferred)} / {humanSize(t.total)}</td>
                <td>
                  <Badge kind={s.kind}>{s.label}</Badge>
                  {t.note && <div style={{ fontSize: 11, color: "#d97706", marginTop: 2 }}>{t.note}</div>}
                  {t.error && <div className="err-text" style={{ fontSize: 11, marginTop: 2 }}>{t.error}</div>}
                </td>
                <td>
                  <div className="row">
                    {!clientMode && t.status === "running" && <button className="btn ghost sm" onClick={() => pause(t.id)}>暂停</button>}
                    {!clientMode && t.status === "paused" && <button className="btn ghost sm" onClick={() => resume(t.id)}>恢复</button>}
                    {(t.status === "running" || t.status === "paused" || t.status === "finalizing") &&
                      <button className="btn danger sm" onClick={() => cancel(t.id)}>取消</button>}
                    {(t.status === "done" || t.status === "error" || t.status === "canceled") &&
                      <button className="btn ghost sm" onClick={() => remove(t.id)}>清除</button>}
                  </div>
                </td>
              </tr>
            );
          })}
          {transfers.length === 0 && <tr><td colSpan={7} className="empty">暂无传输任务</td></tr>}
        </tbody>
      </table>
    </Card>

    {!clientMode && <Card
      title={`历史传输 (${history.length})`}
      action={<button className="btn ghost" onClick={loadHistory}>刷新</button>}
    >
      <table>
        <thead>
          <tr><th style={{ width: 90 }}>类型</th><th>名称</th><th>路径</th><th style={{ width: 120 }}>大小</th><th style={{ width: 180 }}>完成时间</th></tr>
        </thead>
        <tbody>
          {history.map((h) => (
            <tr key={h.ID}>
              <td><Badge kind={h.Kind === "upload" ? "blue" : "green"}>{h.Kind === "upload" ? "上传" : "下载"}</Badge></td>
              <td>{h.Name}</td>
              <td className="muted" title={h.Path}>{h.Path}</td>
              <td className="muted">{humanSize(h.Size)}</td>
              <td className="muted">{fmtTime(h.At)}</td>
            </tr>
          ))}
          {history.length === 0 && <tr><td colSpan={5} className="empty">暂无历史记录</td></tr>}
        </tbody>
      </table>
    </Card>}
    </div>
  );
}
