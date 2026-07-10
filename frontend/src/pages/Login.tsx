import { useState } from "react";
import { api, auth } from "../lib/api";

export default function Login({ onDone }: { onDone: () => void }) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [mode, setMode] = useState<"login" | "register">("login");
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setErr(""); setBusy(true);
    try {
      const r = mode === "login" ? await api.login(username, password) : await api.register(username, password);
      auth.set(r.token, r.role || "user");
      onDone();
    } catch (e: any) {
      setErr(e.message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="auth">
      <form className="card" onSubmit={submit}>
        <div className="card-b">
          <h2>GoSoCode</h2>
          <div className="muted">多人共享云盘 · 存储空间由百度网盘提供</div>
          <div className="field">
            <label>用户名</label>
            <input value={username} onChange={(e) => setUsername(e.target.value)} placeholder="3-64 位用户名" />
          </div>
          <div className="field">
            <label>密码</label>
            <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} placeholder="至少 6 位" />
          </div>
          {mode === "login" && <div className="muted" style={{ marginBottom: 10, fontSize: 12 }}>默认管理员：admin / admin@qwe123</div>}
          {err && <div className="err-text" style={{ marginBottom: 12 }}>{err}</div>}
          <button className="btn" style={{ width: "100%" }} disabled={busy}>
            {busy ? "处理中…" : mode === "login" ? "登录" : "注册"}
          </button>
          <div style={{ marginTop: 14, textAlign: "center" }}>
            <span className="link" onClick={() => setMode(mode === "login" ? "register" : "login")}>
              {mode === "login" ? "没有账号？去注册（首个账号为管理员）" : "已有账号？去登录"}
            </span>
          </div>
        </div>
      </form>
    </div>
  );
}
