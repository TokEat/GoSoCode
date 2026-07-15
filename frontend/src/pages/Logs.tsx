import { useEffect, useState } from "react";
import { api, LogItem } from "../lib/api";
import { Card, Badge, useToast } from "../ui";
import { fmtTime } from "../lib/format";

// Raw audit action codes -> user-facing labels.
const ACTIONS: Record<string, { kind: string; label: string }> = {
  login: { kind: "blue", label: "登录" },
  register: { kind: "green", label: "注册" },
  "file.delete": { kind: "amber", label: "删除文件" },
  "file.protect": { kind: "blue", label: "设置私密" },
  "file.unprotect": { kind: "gray", label: "取消私密" },
  "folder.create": { kind: "gray", label: "新建文件夹" },
  "folder.delete": { kind: "amber", label: "删除文件夹" },
  move: { kind: "gray", label: "移动/重命名" },
  "user.status": { kind: "amber", label: "启停用户" },
  "user.role": { kind: "amber", label: "变更角色" },
  "user.delete": { kind: "red", label: "删除用户" },
  "node.add": { kind: "green", label: "接入网盘" },
  "node.status": { kind: "amber", label: "启停节点" },
  "node.delete": { kind: "red", label: "删除节点" },
  "baidu.config": { kind: "gray", label: "修改应用配置" },
  "system.config": { kind: "blue", label: "系统设置" },
  "http.error": { kind: "red", label: "请求失败" },
};
const actionBadge = (a: string) => {
  const it = ACTIONS[a];
  return <Badge kind={it?.kind || "gray"}>{it?.label || a}</Badge>;
};

const PAGE_SIZE = 20;

export default function Logs() {
  const [logs, setLogs] = useState<LogItem[]>([]);
  const [page, setPage] = useState(1);
  const [total, setTotal] = useState(0);
  const [action, setAction] = useState("");
  const toast = useToast();

  const refresh = (p = page, a = action) =>
    api.logs(p, PAGE_SIZE, a)
      .then((r) => { setLogs(r.logs); setTotal(r.total); setPage(r.page); })
      .catch((e) => toast("err", e.message));
  useEffect(() => { refresh(page, action); }, [page, action]);

  const pages = Math.max(1, Math.ceil(total / PAGE_SIZE));

  return (
    <Card
      title={`系统日志 (共 ${total} 条)`}
      action={
        <div className="row">
          <select value={action} onChange={(e) => { setAction(e.target.value); setPage(1); }}>
            <option value="">全部类型</option>
            {Object.entries(ACTIONS).map(([k, v]) => <option key={k} value={k}>{v.label}</option>)}
          </select>
          <button className="btn ghost" onClick={() => refresh()}>刷新</button>
        </div>
      }
    >
      <table>
        <thead><tr><th>时间</th><th>用户</th><th>操作</th><th>详情</th><th>来源 IP</th></tr></thead>
        <tbody>
          {logs.map((l) => (
            <tr key={l.ID}>
              <td>{fmtTime(l.At)}</td>
              <td>{l.Username || "—"}</td>
              <td>{actionBadge(l.Action)}</td>
              <td style={{ maxWidth: 480, wordBreak: "break-all" }}>{l.Detail || "—"}</td>
              <td>{l.IP}</td>
            </tr>
          ))}
          {logs.length === 0 && <tr><td colSpan={5} className="empty">暂无日志</td></tr>}
        </tbody>
      </table>
      <div className="row" style={{ justifyContent: "flex-end", alignItems: "center", gap: 10, marginTop: 12 }}>
        <button className="btn ghost sm" disabled={page <= 1} onClick={() => setPage(page - 1)}>上一页</button>
        <span className="field-help">第 {page} / {pages} 页</span>
        <button className="btn ghost sm" disabled={page >= pages} onClick={() => setPage(page + 1)}>下一页</button>
      </div>
    </Card>
  );
}
