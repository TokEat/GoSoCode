import { useEffect, useState } from "react";
import { api, downloadShare, ShareInfo } from "../lib/api";
import { Card, useToast } from "../ui";
import { humanSize } from "../lib/format";

export default function SharePage() {
  const toast = useToast();
  const [info, setInfo] = useState<ShareInfo | null>(null);
  const [error, setError] = useState<string>("");
  const [pwd, setPwd] = useState("");
  const [loading, setLoading] = useState(true);

  const token = window.location.pathname.split("/s/")[1]?.split("/")[0] || "";

  useEffect(() => {
    if (!token) {
      setError("分享链接无效");
      setLoading(false);
      return;
    }
    api.getShare(token)
      .then((data) => {
        setInfo(data);
        setLoading(false);
      })
      .catch((e: any) => {
        setError(e?.message || "无法加载分享信息");
        setLoading(false);
      });
  }, [token]);

  function doDownload() {
    if (!token) return;
    if (info?.protected && !pwd.trim()) {
      toast("err", "请输入访问密码");
      return;
    }
    downloadShare(token, info?.name || "download", pwd.trim() || undefined).catch((e: any) => {
      toast("err", e?.message || "下载失败");
    });
  }

  if (loading) {
    return (
      <div className="app-loading">
        <span>G</span>
        <p>正在加载分享信息…</p>
      </div>
    );
  }

  if (error) {
    return (
      <div className="share-page">
        <Card title="分享链接">
          <div className="share-error">{error}</div>
        </Card>
      </div>
    );
  }

  if (!info) return null;

  const expires = info.expires_at
    ? new Date(info.expires_at).toLocaleString("zh-CN")
    : "永久有效";

  return (
    <div className="share-page">
      <Card title="分享内容">
        <div className="share-meta">
          <div className="share-name">{info.is_dir ? "📁" : "📄"} {info.name}</div>
          <div className="share-detail">大小：{humanSize(info.size)}</div>
          <div className="share-detail">类型：{info.is_dir ? "文件夹" : "文件"}</div>
          <div className="share-detail">有效期至：{expires}</div>
          {info.protected && (
            <div className="share-protected">🔒 该内容已设置访问密码</div>
          )}
        </div>

        {info.protected && (
          <div className="share-pwd">
            <input
              type="password"
              value={pwd}
              onChange={(e) => setPwd(e.target.value)}
              placeholder="请输入访问密码"
              onKeyDown={(e) => { if (e.key === "Enter") doDownload(); }}
            />
          </div>
        )}

        <div className="share-actions">
          <button className="btn" onClick={doDownload}>
            {info.is_dir ? "下载目录（zip）" : "立即下载"}
          </button>
        </div>
      </Card>
    </div>
  );
}
