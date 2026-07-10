import { Fragment, useEffect, useState } from "react";
import { api, NodeItem, BaiduConfig, NodeDirectory } from "../lib/api";
import { Card, statusBadge, Badge, useToast, useDialog } from "../ui";
import { humanSize } from "../lib/format";

export default function Nodes() {
  const [nodes, setNodes] = useState<NodeItem[]>([]);
  const [connecting, setConnecting] = useState(false);
  const [cfg, setCfg] = useState<BaiduConfig | null>(null);
  const [form, setForm] = useState({ app_key: "", secret_key: "", sign_key: "", redirect_uri: "" });
  const [loginMode, setLoginMode] = useState<"local_callback" | "local_oob">("local_callback");
  const [savingCfg, setSavingCfg] = useState(false);
  // oob (out-of-band) flow: paste the authorization code Baidu shows.
  const [oobOpen, setOobOpen] = useState(false);
  const [oobCode, setOobCode] = useState("");
  const [oobNickname, setOobNickname] = useState("");
  const [oobBusy, setOobBusy] = useState(false);
  const [editingNode, setEditingNode] = useState<number | null>(null);
  const [nodeForm, setNodeForm] = useState({ mount_name: "", mount_path: "/", remark: "" });
  const [savingNode, setSavingNode] = useState(false);
  const [pickerNode, setPickerNode] = useState<number | null>(null);
  const [pickerPath, setPickerPath] = useState("/");
  const [pickerParent, setPickerParent] = useState("/");
  const [pickerDirectories, setPickerDirectories] = useState<NodeDirectory[]>([]);
  const [pickerBusy, setPickerBusy] = useState(false);
  const toast = useToast();
  const dialog = useDialog();
  const refresh = () => api.listNodes().then(setNodes).catch((e) => toast("err", e.message));

  async function delNode(id: number) {
    if (await dialog.confirm({ title: "删除节点", message: `确定删除存储节点 #${id} 吗?`, danger: true, confirmLabel: "删除" })) {
      act(api.deleteNode(id), "已删除");
    }
  }

  const loadCfg = () => api.getBaiduConfig().then((c) => {
    setCfg(c);
    setLoginMode(c.redirect_uri === "oob" ? "local_oob" : "local_callback");
    setForm({
      app_key: c.app_key,
      secret_key: "",
      sign_key: c.sign_key,
      redirect_uri: c.redirect_uri || `${location.origin}/api/oauth/callback`,
    });
  }).catch((e) => toast("err", e.message));

  useEffect(() => { refresh(); loadCfg(); }, []);

  const act = (fn: Promise<any>, ok: string) =>
    fn.then(() => { toast("ok", ok); refresh(); }).catch((e) => toast("err", e.message));

  async function persistCfg(next: typeof form, okMsg: string) {
    if (!next.app_key.trim()) { toast("info", "请填写 AppKey"); return; }
    if (!cfg?.has_secret && !next.secret_key.trim()) { toast("info", "请填写 SecretKey"); return; }
    setSavingCfg(true);
    try {
      await api.saveBaiduConfig(next);
      toast("ok", okMsg);
      loadCfg();
    } catch (e: any) { toast("err", e.message); }
    finally { setSavingCfg(false); }
  }

  const saveCfg = () => persistCfg(form, "百度应用配置已保存");

  // One click: switch to oob and persist immediately, so connect() sees it.
  function useOobMode() {
    const next = { ...form, redirect_uri: "oob" };
    setForm(next);
    persistCfg(next, "已切换为 oob 模式并保存");
  }

  function connect() {
    if (!cfg?.configured) { toast("info", "请先在下方「百度应用配置」填写 AppKey / SecretKey 并保存"); return; }
    if (loginMode === "local_oob") connectOob();
    else connectPopup();
  }

  // Popup flow (used when a real callback URL is registered): open the Baidu
  // authorization page and wait for the callback page to postMessage the result.
  function connectPopup() {
    const popup = window.open("", "gsc-oauth", "width=760,height=780");
    setConnecting(true);

    const handler = (e: MessageEvent) => {
      if (!e.data || e.data.source !== "gsc-oauth") return;
      window.removeEventListener("message", handler);
      setConnecting(false);
      try { popup?.close(); } catch { /* ignore */ }
      if (e.data.ok) {
        toast(e.data.error ? "info" : "ok", e.data.error || `百度网盘已接入 (节点 #${e.data.node_id})`);
        refresh();
      }
      else { toast("err", "授权失败: " + (e.data.error || "未知错误")); }
    };
    window.addEventListener("message", handler);

    const ensureCallback = cfg?.redirect_uri === "oob"
      ? api.saveBaiduConfig({ ...form, redirect_uri: form.redirect_uri && form.redirect_uri !== "oob" ? form.redirect_uri : `${location.origin}/api/oauth/callback` })
      : Promise.resolve();

    ensureCallback.then(() => api.oauthUrl())
      .then((r) => { if (popup) popup.location.href = r.url; else window.location.href = r.url; })
      .catch((err) => {
        window.removeEventListener("message", handler);
        setConnecting(false);
        try { popup?.close(); } catch { /* ignore */ }
        toast("err", err.message);
      });
  }

  // oob flow (used when Baidu can't call back a local address): open the auth
  // page in a new tab; Baidu shows a one-time code the admin pastes back here.
  async function connectOob() {
    const tab = window.open("", "_blank");
    try {
      if (cfg?.redirect_uri !== "oob") {
        await api.saveBaiduConfig({ ...form, redirect_uri: "oob" });
        setCfg({ ...cfg!, redirect_uri: "oob" });
      }
      const r = await api.oauthUrl();
      if (tab) tab.location.href = r.url; else window.open(r.url, "_blank");
      setOobOpen(true);
      toast("info", "已打开百度授权页,授权后复制页面显示的授权码粘贴到下方");
    } catch (err: any) {
      try { tab?.close(); } catch { /* ignore */ }
      toast("err", err.message);
    }
  }

  async function submitOob() {
    if (!oobCode.trim()) { toast("info", "请粘贴百度显示的授权码"); return; }
    setOobBusy(true);
    try {
      const r = await api.oauthExchange(oobCode.trim(), oobNickname.trim());
      toast(r.requires_rename ? "info" : "ok", r.requires_rename
        ? `节点 #${r.node_id} 名称重复，已保持停用，请重命名后启用`
        : `百度网盘已接入 (节点 #${r.node_id})`);
      setOobOpen(false); setOobCode(""); setOobNickname("");
      refresh();
    } catch (e: any) { toast("err", e.message); }
    finally { setOobBusy(false); }
  }

  function editNode(n: NodeItem) {
    setEditingNode(n.id);
    setPickerNode(null);
    setNodeForm({ mount_name: n.mount_name, mount_path: n.mount_path || "/", remark: n.remark || "" });
  }

  async function browseDirectories(id: number, target: string) {
    setPickerNode(id);
    setPickerBusy(true);
    try {
      const result = await api.listNodeDirectories(id, target);
      setPickerPath(result.current);
      setPickerParent(result.parent);
      setPickerDirectories(result.directories);
    } catch (e: any) { toast("err", e.message); }
    finally { setPickerBusy(false); }
  }

  async function saveNode(id: number) {
    setSavingNode(true);
    try {
      await api.updateNode(id, {
        mount_name: nodeForm.mount_name.trim(),
        mount_path: nodeForm.mount_path.trim() || "/",
        remark: nodeForm.remark.trim(),
      });
      toast("ok", "挂载设置已保存");
      setEditingNode(null);
      setPickerNode(null);
      refresh();
    } catch (e: any) { toast("err", e.message); }
    finally { setSavingNode(false); }
  }

  function usagePct(n: NodeItem) {
    if (!n.quota_total) return 0;
    return Math.round((n.quota_used / n.quota_total) * 100);
  }

  return (
    <div className="grid" style={{ gap: 20 }}>
      <Card title="百度应用配置" action={
        cfg ? <Badge kind={cfg.configured ? "green" : "amber"}>{cfg.configured ? "已配置" : "未配置"}</Badge> : null
      }>
        <div className="muted" style={{ marginBottom: 12, fontSize: 12 }}>
          在百度网盘开放平台创建应用后,把 AppKey / SecretKey 填在这里(SignKey 可选)。回调地址需与开放平台登记的一致。
        </div>
        <div className="grid" style={{ gap: 10 }}>
          <div className="row" style={{ flexWrap: "wrap", gap: 10 }}>
            <div className="field" style={{ flex: 1, minWidth: 260, margin: 0 }}>
              <label>接入方式</label>
              <select value={loginMode} onChange={(e) => setLoginMode(e.target.value as typeof loginMode)}>
                <option value="local_callback">本站应用 · 回调登录</option>
                <option value="local_oob">本站应用 · 手动授权码</option>
              </select>
            </div>
          </div>
          <div className="row" style={{ flexWrap: "wrap", gap: 10 }}>
            <div className="field" style={{ flex: 1, minWidth: 240, margin: 0 }}>
              <label>AppKey</label>
              <input value={form.app_key} onChange={(e) => setForm({ ...form, app_key: e.target.value })} placeholder="应用 AppKey" />
            </div>
            <div className="field" style={{ flex: 1, minWidth: 240, margin: 0 }}>
              <label>SecretKey</label>
              <input type="password" value={form.secret_key} onChange={(e) => setForm({ ...form, secret_key: e.target.value })}
                placeholder={cfg?.has_secret ? "已保存,留空则不修改" : "应用 SecretKey"} />
            </div>
          </div>
          <div className="row" style={{ flexWrap: "wrap", gap: 10 }}>
            <div className="field" style={{ flex: 1, minWidth: 240, margin: 0 }}>
              <label>SignKey (可选)</label>
              <input value={form.sign_key} onChange={(e) => setForm({ ...form, sign_key: e.target.value })} placeholder="应用 SignKey" />
            </div>
            <div className="field" style={{ flex: 1, minWidth: 240, margin: 0 }}>
              <label>授权回调地址</label>
              <input value={form.redirect_uri} onChange={(e) => setForm({ ...form, redirect_uri: e.target.value })}
                placeholder="https://your-host/api/oauth/callback" />
            </div>
          </div>
          <div className="row">
            <button className="btn" onClick={saveCfg} disabled={savingCfg}>{savingCfg ? "保存中…" : "保存配置"}</button>
            <button className="btn ghost" onClick={useOobMode} disabled={savingCfg}>无法接收回调？一键切换免回调模式</button>
          </div>
          <div className="muted" style={{ fontSize: 12 }}>
            当前本站应用授权方式：<b>{cfg?.redirect_uri === "oob" ? "免回调（授权后粘贴授权码）" : (cfg?.redirect_uri || "未设置")}</b>。
            本机或内网部署收不到百度回调时，点上面「一键切换免回调模式」（立即生效）；之后接入账号时，授权页会显示一串授权码，复制粘贴回来即可。
          </div>
        </div>
      </Card>

      <Card title={`存储节点 · 百度网盘账号 (${nodes.length})`} action={
      <div className="row">
        <button className="btn ghost" onClick={refresh}>刷新列表</button>
        <button className="btn" onClick={connect} disabled={connecting}>
          {connecting ? "等待授权…" : "+ 接入百度网盘"}
        </button>
      </div>
    }>
      <div className="muted" style={{ marginBottom: 12, fontSize: 12 }}>
        {(loginMode === "local_oob" || cfg?.redirect_uri === "oob")
          ? "免回调模式：点「接入百度网盘」打开授权页，授权后复制页面显示的授权码，粘贴到下方提交。"
          : "点击「接入百度网盘」将跳转百度官方登录授权，完成后账号自动加入，无需手动填写任何密钥。"}
      </div>

      {oobOpen && (
        <div className="grid" style={{ gap: 10, marginBottom: 16, padding: 16, background: "rgba(255,255,255,0.45)", borderRadius: 8 }}>
          <div className="row" style={{ flexWrap: "wrap", gap: 10 }}>
            <input placeholder="授权账号别名 (可选)" value={oobNickname}
              onChange={(e) => setOobNickname(e.target.value)} style={{ maxWidth: 200 }} />
            <input placeholder="粘贴百度显示的授权码" value={oobCode}
              onChange={(e) => setOobCode(e.target.value)} style={{ flex: 1, minWidth: 260 }} />
            <button className="btn" onClick={submitOob} disabled={oobBusy}>{oobBusy ? "提交中…" : "提交授权码"}</button>
            <button className="btn ghost" onClick={() => setOobOpen(false)}>取消</button>
          </div>
        </div>
      )}

      <table>
        <thead><tr><th>编号</th><th>挂载节点</th><th>挂载目录</th><th>备注</th><th>状态 / 等级</th><th>容量使用</th><th>进行中任务</th><th>累计上传/下载</th><th>操作</th></tr></thead>
        <tbody>
          {nodes.map((n) => (
            <Fragment key={n.id}>
              <tr>
                <td>{n.id}</td>
                <td style={{ minWidth: 150 }}>
                  <div style={{ fontWeight: 650 }}>{n.mount_segment}</div>
                  <div className="muted" style={{ fontSize: 11 }}>账号：{n.nickname || "未获取"}</div>
                </td>
                <td><code>{n.mount_path || "/"}</code></td>
                <td style={{ maxWidth: 180, whiteSpace: "normal" }}>{n.remark || <span className="muted">无</span>}</td>
                <td>
                  <div style={{ marginBottom: 5 }}>{statusBadge(n.status)}</div>
                  <Badge kind={n.vip_type >= 2 ? "amber" : n.vip_type === 1 ? "blue" : "gray"}>
                    {n.vip_type >= 2 ? "SVIP" : n.vip_type === 1 ? "VIP" : "普通"}
                  </Badge>
                  <div style={{ fontSize: 11, color: "#7a8194" }}>分片 {humanSize(n.slice_size ?? 4 * 1024 * 1024)}</div>
                </td>
                <td style={{ minWidth: 180 }}>
                  {n.quota_total > 0 ? <>
                    <div className="bar"><span style={{ width: `${usagePct(n)}%`, background: usagePct(n) > 85 ? "#dc2626" : "#6366f1" }} /></div>
                    <span style={{ fontSize: 12, color: "#7a8194" }}>
                      {humanSize(n.quota_used)} / {humanSize(n.quota_total)} · 剩余 {humanSize(n.quota_free)}
                    </span>
                  </> : <span style={{ color: "#7a8194" }}>未知（点「刷新容量」获取）</span>}
                </td>
                <td>{n.current_load}</td>
                <td>{humanSize(n.bytes_up ?? 0)} / {humanSize(n.bytes_down ?? 0)}</td>
                <td>
                  <div className="row" style={{ flexWrap: "wrap" }}>
                    <button className="btn ghost sm" onClick={() => editNode(n)}>设置</button>
                    <button className="btn ghost sm" onClick={() => act(api.refreshNode(n.id), "容量信息已更新")}>刷新容量</button>
                    {n.status === "active"
                      ? <button className="btn ghost sm" onClick={() => act(api.setNodeStatus(n.id, "disabled"), "已停用")}>停用</button>
                      : <button className="btn ghost sm" onClick={() => act(api.setNodeStatus(n.id, "active"), "已启用")}>启用</button>}
                    <button className="btn danger sm" onClick={() => delNode(n.id)}>删除</button>
                  </div>
                </td>
              </tr>
              {editingNode === n.id && (
                <tr>
                  <td colSpan={9} style={{ padding: 16, background: "rgba(99, 102, 241, 0.045)" }}>
                    <div className="row" style={{ alignItems: "flex-end", flexWrap: "wrap", gap: 12 }}>
                      <div className="field" style={{ flex: "1 1 180px", margin: 0 }}>
                        <label>挂载名称</label>
                        <input value={nodeForm.mount_name} onChange={(e) => setNodeForm({ ...nodeForm, mount_name: e.target.value })}
                          placeholder={n.nickname || "百度网盘"} maxLength={128} />
                      </div>
                      <div className="field" style={{ flex: "1 1 260px", margin: 0 }}>
                        <label>挂载目录</label>
                        <div className="row" style={{ gap: 8 }}>
                          <input value={nodeForm.mount_path} readOnly aria-label="已选择的挂载目录" />
                          <button className="btn ghost" onClick={() => browseDirectories(n.id, nodeForm.mount_path || "/")}
                            disabled={pickerBusy}>选择目录</button>
                        </div>
                      </div>
                      <div className="field" style={{ flex: "2 1 300px", margin: 0 }}>
                        <label>账号备注</label>
                        <input value={nodeForm.remark} onChange={(e) => setNodeForm({ ...nodeForm, remark: e.target.value })}
                          placeholder="用途、负责人或容量说明" maxLength={512} />
                      </div>
                      <button className="btn" onClick={() => saveNode(n.id)} disabled={savingNode}>{savingNode ? "验证中…" : "保存"}</button>
                      <button className="btn ghost" onClick={() => { setEditingNode(null); setPickerNode(null); }} disabled={savingNode}>取消</button>
                    </div>
                    {pickerNode === n.id && (
                      <div style={{ marginTop: 14, border: "1px solid var(--border, #dfe3eb)", borderRadius: 6, background: "rgba(255,255,255,0.7)" }}>
                        <div className="row" style={{ padding: "10px 12px", borderBottom: "1px solid var(--border, #dfe3eb)", flexWrap: "wrap", gap: 8 }}>
                          <code style={{ flex: 1, minWidth: 180 }}>{pickerPath}</code>
                          <button className="btn ghost sm" onClick={() => browseDirectories(n.id, pickerParent)} disabled={pickerBusy || pickerPath === "/"}>上一级</button>
                          <button className="btn sm" onClick={() => { setNodeForm({ ...nodeForm, mount_path: pickerPath }); setPickerNode(null); }} disabled={pickerBusy}>选择当前目录</button>
                          <button className="btn ghost sm" onClick={() => setPickerNode(null)}>关闭</button>
                        </div>
                        <div style={{ padding: 10, display: "grid", gridTemplateColumns: "repeat(auto-fill, minmax(180px, 1fr))", gap: 8, minHeight: 54 }}>
                          {pickerDirectories.map((dir) => (
                            <button key={dir.path} className="btn ghost" style={{ justifyContent: "flex-start", overflow: "hidden", textOverflow: "ellipsis" }}
                              title={dir.path} onClick={() => browseDirectories(n.id, dir.path)} disabled={pickerBusy}>
                              {dir.name}
                            </button>
                          ))}
                          {!pickerBusy && pickerDirectories.length === 0 && <span className="muted" style={{ padding: 8 }}>当前目录没有子文件夹</span>}
                          {pickerBusy && <span className="muted" style={{ padding: 8 }}>正在读取目录…</span>}
                        </div>
                      </div>
                    )}
                    <div className="muted" style={{ marginTop: 8, fontSize: 12 }}>
                      文件管理中的根目录名称与挂载名称完全一致；挂载目录只能从该账号可访问的文件夹中选择。
                    </div>
                  </td>
                </tr>
              )}
            </Fragment>
          ))}
          {nodes.length === 0 && <tr><td colSpan={9} className="empty">暂无存储节点，点击右上角接入百度网盘</td></tr>}
        </tbody>
      </table>
      </Card>
    </div>
  );
}
