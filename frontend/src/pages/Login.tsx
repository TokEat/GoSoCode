import { useEffect, useState } from "react";
import { api, auth } from "../lib/api";

export default function Login({ onDone }: { onDone: () => void }) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [err, setErr] = useState(() => sessionStorage.getItem("gsc_oidc_error") || "");
  const [busy, setBusy] = useState(false);
  const [ssoEnabled, setSsoEnabled] = useState(false);

  useEffect(() => {
    sessionStorage.removeItem("gsc_oidc_error");
    api.oidcStatus()
      .then((r) => setSsoEnabled(r.enabled))
      .catch(() => {});
  }, []);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setErr(""); setBusy(true);
    try {
      const r = await api.login(username, password);
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
          {err && <div className="err-text" style={{ marginBottom: 12 }}>{err}</div>}
          <button className="btn" style={{ width: "100%" }} disabled={busy}>
            {busy ? "处理中…" : "登录"}
          </button>
          {ssoEnabled && (
            <>
              <div className="muted" style={{ textAlign: "center", margin: "12px 0 10px", fontSize: 12 }}>—— 或 ——</div>
              <button
                type="button"
                className="btn ghost"
                style={{ width: "100%" }}
                onClick={() => { window.location.href = "/api/auth/oidc/login"; }}
              >
                使用统一认证登录 (SSO)
              </button>
            </>
          )}
        </div>
      </form>
    </div>
  );
}
