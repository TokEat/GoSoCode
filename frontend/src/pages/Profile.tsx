import { useEffect, useState } from "react";
import { api, Me } from "../lib/api";
import { Card, Badge, useToast, useDialog } from "../ui";
import { humanSize, fmtTime } from "../lib/format";

export default function Profile() {
  const [me, setMe] = useState<Me | null>(null);
  const [oldp, setOldp] = useState("");
  const [newp, setNewp] = useState("");
  const [busy, setBusy] = useState(false);
  const toast = useToast();
  const dialog = useDialog();

  const refresh = () => api.me().then(setMe).catch(() => {});
  useEffect(() => { refresh(); }, []);

  async function changePw(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    try {
      await api.changePassword(oldp, newp);
      toast("ok", "密码已修改");
      setOldp(""); setNewp("");
    } catch (e: any) { toast("err", e.message); }
    finally { setBusy(false); }
  }

  async function removeProtect(path: string) {
    if (!(await dialog.confirm({ title: "取消私密", message: `确定移除 ${path} 的访问密码吗？`, confirmLabel: "移除" }))) return;
    try {
      await api.unprotect(path);
      toast("ok", "已取消私密");
      refresh();
    } catch (e: any) { toast("err", e.message || "操作失败"); }
  }

  const protectedItems = me?.protected || [];

  return (
    <div className="grid cols-3" style={{ alignItems: "start" }}>
      <div style={{ gridColumn: "span 2", display: "grid", gap: 18 }}>
        <Card title="账户信息">
          <table>
            <tbody>
              <tr><td style={{ color: "#7a8194" }}>用户名</td><td>{me?.username}</td></tr>
              <tr><td style={{ color: "#7a8194" }}>账号来源</td><td>
                {me && <Badge kind={me.sso ? "amber" : "gray"}>{me.sso ? "SSO 统一认证" : "系统账号"}</Badge>}
              </td></tr>
              <tr><td style={{ color: "#7a8194" }}>角色</td><td>{me?.role === "admin" ? "管理员" : "普通用户"}</td></tr>
              <tr><td style={{ color: "#7a8194" }}>状态</td><td>{me?.status === "active" ? "正常" : "已停用"}</td></tr>
              <tr><td style={{ color: "#7a8194" }}>文件数</td><td>{me?.file_count}</td></tr>
              <tr><td style={{ color: "#7a8194" }}>累计上传</td><td>{humanSize(me?.bytes_up ?? 0)}</td></tr>
              <tr><td style={{ color: "#7a8194" }}>累计下载</td><td>{humanSize(me?.bytes_down ?? 0)}</td></tr>
              <tr><td style={{ color: "#7a8194" }}>注册时间</td><td>{me ? fmtTime(me.created_at) : ""}</td></tr>
            </tbody>
          </table>
        </Card>

        <Card title={`我的私密内容 (${protectedItems.length})`}>
          <table>
            <thead><tr><th>路径</th><th style={{ width: 100 }}>类型</th><th style={{ width: 180 }}>设置时间</th><th style={{ width: 110 }}></th></tr></thead>
            <tbody>
              {protectedItems.map((p) => (
                <tr key={p.path}>
                  <td title={p.path} style={{ maxWidth: 360, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
                    🔒 {p.path}
                  </td>
                  <td className="muted">{p.is_dir ? "目录" : "文件"}</td>
                  <td className="muted">{fmtTime(p.created_at)}</td>
                  <td><button className="btn ghost sm" onClick={() => removeProtect(p.path)}>取消私密</button></td>
                </tr>
              ))}
              {protectedItems.length === 0 && <tr><td colSpan={4} className="empty">暂无私密内容 · 可在文件管理中右键「设为私密」</td></tr>}
            </tbody>
          </table>
        </Card>
      </div>

      <Card title="修改密码">
        {me?.sso ? (
          <p className="field-help">SSO 统一认证账号的密码由身份服务（Keycloak）管理，请前往统一认证平台修改。</p>
        ) : (
          <form onSubmit={changePw}>
            <div className="field"><label>当前密码</label>
              <input type="password" value={oldp} onChange={(e) => setOldp(e.target.value)} /></div>
            <div className="field"><label>新密码 (≥6位)</label>
              <input type="password" value={newp} onChange={(e) => setNewp(e.target.value)} /></div>
            <button className="btn" disabled={busy}>{busy ? "提交中…" : "更新密码"}</button>
          </form>
        )}
      </Card>
    </div>
  );
}
