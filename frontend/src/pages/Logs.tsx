import { useEffect, useState } from "react";
import { api, LogItem } from "../lib/api";
import { Card, Badge, useToast } from "../ui";
import { fmtTime } from "../lib/format";

// Raw audit action codes -> user-facing labels.
const ACTIONS: Record<string, { kind: string; label: string }> = {
  login: { kind: "blue", label: "登录" },
  register: { kind: "green", label: "注册" },
  "file.delete": { kind: "amber", label: "删除文件" },
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
};
const actionBadge = (a: string) => {
  const it = ACTIONS[a];
  return <Badge kind={it?.kind || "gray"}>{it?.label || a}</Badge>;
};

export default function Logs() {
  const [logs, setLogs] = useState<LogItem[]>([]);
  const toast = useToast();
  const refresh = () => api.logs().then(setLogs).catch((e) => toast("err", e.message));
  useEffect(() => { refresh(); }, []);

  return (
    <Card title={`系统日志 (${logs.length})`} action={<button className="btn ghost" onClick={refresh}>刷新</button>}>
      <table>
        <thead><tr><th>时间</th><th>用户</th><th>操作</th><th>详情</th><th>来源 IP</th></tr></thead>
        <tbody>
          {logs.map((l) => (
            <tr key={l.ID}>
              <td>{fmtTime(l.At)}</td>
              <td>{l.Username || "—"}</td>
              <td>{actionBadge(l.Action)}</td>
              <td>{l.Detail || "—"}</td>
              <td>{l.IP}</td>
            </tr>
          ))}
          {logs.length === 0 && <tr><td colSpan={5} className="empty">暂无日志</td></tr>}
        </tbody>
      </table>
    </Card>
  );
}
