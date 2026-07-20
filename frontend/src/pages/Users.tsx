import { useEffect, useState } from "react";
import { api, UserInput, UserItem } from "../lib/api";
import { Card, statusBadge, Badge, useToast, useDialog } from "../ui";
import { humanSize, fmtTime } from "../lib/format";

const blankForm: UserInput = { username: "", password: "", role: "user", status: "active" };

export default function Users() {
  const [users, setUsers] = useState<UserItem[]>([]);
  const [formOpen, setFormOpen] = useState(false);
  const [editing, setEditing] = useState<UserItem | null>(null);
  const [form, setForm] = useState<UserInput>(blankForm);
  const [busy, setBusy] = useState(false);
  const toast = useToast();
  const dialog = useDialog();
  const refresh = () => api.listUsers().then(setUsers).catch((e) => toast("err", e.message));

  useEffect(() => { refresh(); }, []);

  const act = (fn: Promise<any>, ok: string) =>
    fn.then(() => { toast("ok", ok); refresh(); }).catch((e) => toast("err", e.message));

  function startCreate() {
    setEditing(null);
    setForm(blankForm);
    setFormOpen(true);
  }

  function startEdit(u: UserItem) {
    setEditing(u);
    setForm({ username: u.username, password: "", role: u.role, status: u.status });
    setFormOpen(true);
  }

  async function submit() {
    if (!form.username.trim()) { toast("info", "请填写用户名"); return; }
    if (!editing && !form.password?.trim()) { toast("info", "请填写初始密码"); return; }
    setBusy(true);
    try {
      const payload = { ...form, username: form.username.trim(), password: form.password?.trim() };
      if (editing) await api.updateUser(editing.id, payload);
      else await api.createUser(payload);
      toast("ok", editing ? "用户已更新" : "用户已创建");
      setFormOpen(false);
      refresh();
    } catch (e: any) { toast("err", e.message); }
    finally { setBusy(false); }
  }

  async function delUser(id: number, name: string) {
    if (await dialog.confirm({ title: "删除用户", message: `确定删除用户 ${name} 吗?`, danger: true, confirmLabel: "删除" })) {
      act(api.deleteUser(id), "已删除");
    }
  }

  return (
    <div className="grid" style={{ gap: 18 }}>
      {formOpen && (
        <Card title={editing ? `编辑用户 #${editing.id}` : "新增用户"} action={
          <button className="btn ghost" onClick={() => setFormOpen(false)}>取消</button>
        }>
          <div className="row" style={{ flexWrap: "wrap", gap: 12 }}>
            <div className="field" style={{ flex: 1, minWidth: 220, margin: 0 }}>
              <label>用户名</label>
              <input value={form.username} onChange={(e) => setForm({ ...form, username: e.target.value })} placeholder="3-64 个字符" />
            </div>
            <div className="field" style={{ flex: 1, minWidth: 220, margin: 0 }}>
              <label>{editing ? "新密码" : "初始密码"}</label>
              <input type="password" value={form.password || ""} onChange={(e) => setForm({ ...form, password: e.target.value })}
                placeholder={editing ? "留空则不修改" : "至少 6 个字符"} />
            </div>
            <div className="field" style={{ width: 150, margin: 0 }}>
              <label>角色</label>
              <select value={form.role} onChange={(e) => setForm({ ...form, role: e.target.value })}>
                <option value="user">普通用户</option>
                <option value="admin">管理员</option>
              </select>
            </div>
            <div className="field" style={{ width: 150, margin: 0 }}>
              <label>状态</label>
              <select value={form.status} onChange={(e) => setForm({ ...form, status: e.target.value })}>
                <option value="active">正常</option>
                <option value="disabled">已停用</option>
              </select>
            </div>
            <button className="btn" onClick={submit} disabled={busy} style={{ alignSelf: "end" }}>
              {busy ? "保存中…" : "保存"}
            </button>
          </div>
        </Card>
      )}

      <Card title={`系统用户 (${users.length})`} action={
        <div className="row">
          <button className="btn ghost" onClick={refresh}>刷新</button>
          <button className="btn" onClick={startCreate}>+ 新增用户</button>
        </div>
      }>
        <table>
          <thead><tr><th>编号</th><th>用户名</th><th>来源</th><th>角色</th><th>状态</th><th>累计上传/下载</th><th>注册时间</th><th>操作</th></tr></thead>
          <tbody>
            {users.map((u) => (
              <tr key={u.id}>
                <td>{u.id}</td>
                <td>{u.username}</td>
                <td><Badge kind={u.sso ? "amber" : "gray"}>{u.sso ? "SSO 用户" : "系统用户"}</Badge></td>
                <td><Badge kind={u.role === "admin" ? "blue" : "gray"}>{u.role === "admin" ? "管理员" : "普通用户"}</Badge></td>
                <td>{statusBadge(u.status)}</td>
                <td>{humanSize(u.bytes_up)} / {humanSize(u.bytes_down)}</td>
                <td>{fmtTime(u.created_at)}</td>
                <td>
                  <div className="row">
                    <button className="btn ghost sm" onClick={() => startEdit(u)}>编辑</button>
                    {u.status === "active"
                      ? <button className="btn ghost sm" onClick={() => act(api.setUserStatus(u.id, "disabled"), "已禁用")}>禁用</button>
                      : <button className="btn ghost sm" onClick={() => act(api.setUserStatus(u.id, "active"), "已启用")}>启用</button>}
                    {u.role === "user"
                      ? <button className="btn ghost sm" onClick={() => act(api.setUserRole(u.id, "admin"), "已设为管理员")}>设管理员</button>
                      : <button className="btn ghost sm" onClick={() => act(api.setUserRole(u.id, "user"), "已取消管理员")}>取消管理员</button>}
                    <button className="btn danger sm" onClick={() => delUser(u.id, u.username)}>删除</button>
                  </div>
                </td>
              </tr>
            ))}
            {users.length === 0 && <tr><td colSpan={8} className="empty">暂无用户</td></tr>}
          </tbody>
        </table>
      </Card>
    </div>
  );
}
