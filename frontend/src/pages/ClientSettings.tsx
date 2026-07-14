import { useEffect, useState } from "react";
import { clientApi, ClientInfo } from "../lib/api";
import { Card, useToast } from "../ui";

interface Props {
  info: ClientInfo;
  firstRun?: boolean;
  onSaved: (info: ClientInfo, serverChanged: boolean) => void;
}

export default function ClientSettings({ info, firstRun = false, onSaved }: Props) {
  const [server, setServer] = useState(info.server || "");
  const [downloadDir, setDownloadDir] = useState(info.download_dir || "");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const toast = useToast();

  useEffect(() => {
    setServer(info.server || "");
    setDownloadDir(info.download_dir || "");
  }, [info.server, info.download_dir]);

  async function save() {
    const nextServer = server.trim().replace(/\/+$/, "");
    if (!nextServer) {
      setError("请填写 GoSoCode 服务端地址");
      return;
    }
    setSaving(true);
    setError("");
    try {
      const next = await clientApi.saveConfig({ server: nextServer, download_dir: downloadDir.trim() });
      const changed = !!info.server && next.server !== info.server;
      toast("ok", firstRun ? "客户端已连接服务端" : "客户端设置已保存");
      onSaved(next, changed);
    } catch (e: any) {
      setError(e.message || "连接服务端失败");
    } finally {
      setSaving(false);
    }
  }

  const form = (
    <>
      <div className="client-setting-intro">
        <div className="client-orbit"><span>G</span></div>
        <div>
          <h2>{firstRun ? "连接你的 GoSoCode 服务端" : "本地客户端设置"}</h2>
          <p>浏览器只负责选择任务与查看进度。文件由本机客户端直接读写并连接云端，不经过浏览器或平台服务器。</p>
        </div>
      </div>
      <div className="field">
        <label>服务端地址</label>
        <input
          autoFocus={firstRun}
          value={server}
          onChange={(e) => setServer(e.target.value)}
          onKeyDown={(e) => { if (e.key === "Enter") save(); }}
          placeholder="http://192.168.1.10:8080"
        />
        <small className="field-help">保存前会检查服务端的健康状态。</small>
      </div>
      <div className="field">
        <label>本地下载目录</label>
        <input
          value={downloadDir}
          onChange={(e) => setDownloadDir(e.target.value)}
          onKeyDown={(e) => { if (e.key === "Enter") save(); }}
          placeholder="例如 D:\\Downloads"
        />
        <small className="field-help">下载任务完成后直接保存在此目录；重名文件会自动编号。</small>
      </div>
      {error && <div className="client-config-error">{error}</div>}
      <div className="client-setting-actions">
        {!firstRun && <span className={`client-connection ${info.configured ? "online" : "offline"}`}>
          <i />{info.configured ? "服务端已连接" : "当前未连接"}
        </span>}
        <button className="btn" disabled={saving} onClick={save}>{saving ? "正在连接…" : firstRun ? "连接并继续" : "保存设置"}</button>
      </div>
    </>
  );

  if (firstRun) {
    return (
      <div className="client-onboarding">
        <div className="client-onboarding-glow" />
        <div className="client-onboarding-card">{form}</div>
        <div className="client-onboarding-foot">GoSoCode Local Client · 数据留在你的设备与云端之间</div>
      </div>
    );
  }

  return <Card title="客户端连接与本地存储">{form}</Card>;
}
