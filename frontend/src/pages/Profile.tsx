import { useEffect, useState } from "react";
import { api, Me } from "../lib/api";
import { Card, useToast } from "../ui";
import { humanSize, fmtTime } from "../lib/format";

export default function Profile() {
  const [me, setMe] = useState<Me | null>(null);
  const [oldp, setOldp] = useState("");
  const [newp, setNewp] = useState("");
  const [busy, setBusy] = useState(false);
  const toast = useToast();

  useEffect(() => { api.me().then(setMe).catch(() => {}); }, []);

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

  return (
    <div className="grid cols-3" style={{ alignItems: "start" }}>
      <div style={{ gridColumn: "span 2" }}>
        <Card title="账户信息">
          <table>
            <tbody>
              <tr><td style={{ color: "#7a8194" }}>用户名</td><td>{me?.username}</td></tr>
              <tr><td style={{ color: "#7a8194" }}>角色</td><td>{me?.role === "admin" ? "管理员" : "普通用户"}</td></tr>
              <tr><td style={{ color: "#7a8194" }}>文件数</td><td>{me?.file_count}</td></tr>
              <tr><td style={{ color: "#7a8194" }}>累计上传</td><td>{humanSize(me?.bytes_up ?? 0)}</td></tr>
              <tr><td style={{ color: "#7a8194" }}>累计下载</td><td>{humanSize(me?.bytes_down ?? 0)}</td></tr>
              <tr><td style={{ color: "#7a8194" }}>注册时间</td><td>{me ? fmtTime(me.created_at) : ""}</td></tr>
            </tbody>
          </table>
        </Card>
      </div>
      <Card title="修改密码">
        <form onSubmit={changePw}>
          <div className="field"><label>当前密码</label>
            <input type="password" value={oldp} onChange={(e) => setOldp(e.target.value)} /></div>
          <div className="field"><label>新密码 (≥6位)</label>
            <input type="password" value={newp} onChange={(e) => setNewp(e.target.value)} /></div>
          <button className="btn" disabled={busy}>{busy ? "提交中…" : "更新密码"}</button>
        </form>
      </Card>
    </div>
  );
}
