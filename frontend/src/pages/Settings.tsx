import { useEffect, useState } from "react";
import { api, SystemConfig } from "../lib/api";
import { Card, useToast } from "../ui";

export default function Settings() {
  const [cfg, setCfg] = useState<SystemConfig | null>(null);
  const [logMax, setLogMax] = useState("500");
  const [saving, setSaving] = useState(false);
  const [oidc, setOidc] = useState({ enabled: false, issuer: "", clientId: "", secret: "", redirect: "" });
  const toast = useToast();

  const syncOidc = (c: SystemConfig) =>
    setOidc({ enabled: c.oidc_enabled, issuer: c.oidc_issuer || "", clientId: c.oidc_client_id || "", secret: "", redirect: c.oidc_redirect_url || "" });

  useEffect(() => {
    api.getSystemConfig()
      .then((c) => { setCfg(c); setLogMax(String(c.log_max_entries)); syncOidc(c); })
      .catch((e) => toast("err", e.message));
  }, []);

  async function save(next: Partial<SystemConfig>) {
    setSaving(true);
    try {
      const saved = await api.saveSystemConfig(next);
      setCfg(saved);
      setLogMax(String(saved.log_max_entries));
      syncOidc(saved);
      toast("ok", "系统设置已保存");
    } catch (e: any) {
      toast("err", e.message);
    } finally {
      setSaving(false);
    }
  }

  if (!cfg) return <Card title="系统设置"><p className="field-help">正在加载…</p></Card>;

  return (
    <>
      <Card title="加密存储">
        <div className="field">
          <label style={{ display: "flex", alignItems: "center", gap: 8, cursor: "pointer" }}>
            <input
              type="checkbox"
              checked={cfg.encrypt_storage}
              disabled={saving}
              onChange={(e) => save({ encrypt_storage: e.target.checked })}
              style={{ width: "auto" }}
            />
            启用加密存储模式
          </label>
          <small className="field-help">
            开启后，新上传的文件在云端保存为 AES-256 加密的密文，下载时由服务端自动解密；
            加密密钥仅保存在服务端。注意：加密上传将自动回退到存储转发路径（先传到服务器再推送云端），
            流式直传与秒传在加密模式下不可用；已上传的历史文件不受开关影响。
          </small>
        </div>
      </Card>

      <Card title="统一认证 (Keycloak / OIDC)">
        <div className="field">
          <label style={{ display: "flex", alignItems: "center", gap: 8, cursor: "pointer" }}>
            <input
              type="checkbox"
              checked={oidc.enabled}
              disabled={saving}
              onChange={(e) => setOidc({ ...oidc, enabled: e.target.checked })}
              style={{ width: "auto" }}
            />
            启用统一认证登录
          </label>
          <small className="field-help">
            开启后，登录页显示「统一认证登录」入口，Keycloak 用户首次登录自动创建账号（角色为普通用户）；
            系统中已有的本地用户仍可使用用户名密码登录，两种方式并存。
          </small>
        </div>
        <div className="field">
          <label>Issuer 地址</label>
          <input
            value={oidc.issuer}
            disabled={saving}
            onChange={(e) => setOidc({ ...oidc, issuer: e.target.value })}
            placeholder="https://kc.example.com/realms/myrealm"
          />
          <small className="field-help">Keycloak realm 的 Issuer，需能访问 &lt;Issuer&gt;/.well-known/openid-configuration。</small>
        </div>
        <div className="field">
          <label>Client ID</label>
          <input value={oidc.clientId} disabled={saving} onChange={(e) => setOidc({ ...oidc, clientId: e.target.value })} placeholder="gosocode" />
        </div>
        <div className="field">
          <label>Client Secret</label>
          <input
            type="password"
            value={oidc.secret}
            disabled={saving}
            onChange={(e) => setOidc({ ...oidc, secret: e.target.value })}
            placeholder={cfg.oidc_has_secret ? "已保存（留空表示不修改）" : "机密客户端的 Secret"}
          />
        </div>
        <div className="field">
          <label>回调地址（可选）</label>
          <input
            value={oidc.redirect}
            disabled={saving}
            onChange={(e) => setOidc({ ...oidc, redirect: e.target.value })}
            placeholder="https://pan.example.com/api/auth/oidc/callback（留空则按访问地址自动生成）"
          />
          <small className="field-help">需与 Keycloak 客户端里配置的 Valid Redirect URI 完全一致。</small>
        </div>
        <button
          className="btn"
          disabled={saving}
          onClick={() => save({
            oidc_enabled: oidc.enabled,
            oidc_issuer: oidc.issuer,
            oidc_client_id: oidc.clientId,
            oidc_client_secret: oidc.secret,
            oidc_redirect_url: oidc.redirect,
          })}
        >
          保存统一认证配置
        </button>
      </Card>

      <Card title="系统日志">
        <div className="field">
          <label>日志保留条数上限</label>
          <input
            type="number"
            min={50}
            max={100000}
            value={logMax}
            onChange={(e) => setLogMax(e.target.value)}
            style={{ maxWidth: 200 }}
          />
          <small className="field-help">日志超过该数量时，系统会定时清理最旧的记录（默认 500 条）。</small>
        </div>
        <button
          className="btn"
          disabled={saving}
          onClick={() => {
            const n = parseInt(logMax, 10);
            if (!Number.isFinite(n) || n < 50 || n > 100000) { toast("err", "日志保留条数需在 50-100000 之间"); return; }
            save({ log_max_entries: n });
          }}
        >
          保存
        </button>
      </Card>
    </>
  );
}
