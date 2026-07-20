import { useEffect, useState } from "react";
import { api, auth, clientApi, ClientInfo } from "../lib/api";

export default function Login({ onDone, runtime }: { onDone: () => void; runtime?: ClientInfo | null }) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [err, setErr] = useState(() => sessionStorage.getItem("gsc_oidc_error") || "");
  const [busy, setBusy] = useState(false);
  const [ssoEnabled, setSsoEnabled] = useState(false);
  const clientMode = runtime?.mode === "client";
  const [editServer, setEditServer] = useState(false);
  const [serverAddr, setServerAddr] = useState(runtime?.server || "");
  const [serverBusy, setServerBusy] = useState(false);
  const [serverErr, setServerErr] = useState("");

  useEffect(() => {
    sessionStorage.removeItem("gsc_oidc_error");
    api.oidcStatus()
      .then((r) => setSsoEnabled(r.enabled))
      .catch(() => {});
  }, []);

  async function saveServer() {
    const addr = serverAddr.trim();
    if (!addr) { setServerErr("请输入服务端地址"); return; }
    setServerBusy(true); setServerErr("");
    try {
      await clientApi.saveConfig({ server: addr });
      // New server = new accounts/JWT secret; drop any stale token and
      // re-probe everything (mode, SSO availability) from scratch.
      auth.clear();
      window.location.reload();
    } catch (e: any) {
      setServerErr(e.message || "无法连接服务端");
      setServerBusy(false);
    }
  }

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
          {clientMode && (
            <div style={{ marginTop: 16, paddingTop: 12, borderTop: "1px solid var(--border, #e2e8f0)" }}>
              {!editServer ? (
                <div className="muted" style={{ fontSize: 12, display: "flex", justifyContent: "space-between", alignItems: "center", gap: 8 }}>
                  <span style={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }} title={runtime?.server}>
                    服务端: {runtime?.server || "未设置"}
                  </span>
                  <button type="button" className="btn ghost sm" onClick={() => { setServerAddr(runtime?.server || ""); setServerErr(""); setEditServer(true); }}>
                    切换
                  </button>
                </div>
              ) : (
                <>
                  <div className="field" style={{ marginBottom: 8 }}>
                    <label>服务端地址</label>
                    <input
                      value={serverAddr}
                      onChange={(e) => setServerAddr(e.target.value)}
                      placeholder="http://192.168.1.10:8080"
                      disabled={serverBusy}
                    />
                  </div>
                  {serverErr && <div className="err-text" style={{ marginBottom: 8 }}>{serverErr}</div>}
                  <div className="row" style={{ gap: 8 }}>
                    <button type="button" className="btn sm" disabled={serverBusy} onClick={saveServer}>
                      {serverBusy ? "连接中…" : "保存并切换"}
                    </button>
                    <button type="button" className="btn ghost sm" disabled={serverBusy} onClick={() => setEditServer(false)}>取消</button>
                  </div>
                </>
              )}
            </div>
          )}
        </div>
      </form>
    </div>
  );
}
