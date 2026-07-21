import { useEffect, useMemo, useState } from "react";
import { api, ShareFile, ShareResolveResult } from "./lib/api";
import { humanSize } from "./lib/format";

interface Props {
  open: boolean;
  targetDir: string; // current logical directory (import destination)
  onClose: () => void;
  onSubmitted: (jobId: string, name: string, total: number) => void;
}

export default function ShareImportDialog({ open, targetDir, onClose, onSubmitted }: Props) {
  const [link, setLink] = useState("");
  const [pwd, setPwd] = useState("");
  const [resolving, setResolving] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState("");
  const [share, setShare] = useState<ShareResolveResult | null>(null);
  const [picked, setPicked] = useState<Set<string>>(new Set());
  const [encrypt, setEncrypt] = useState(false);

  useEffect(() => {
    if (open) {
      setLink(""); setPwd(""); setShare(null); setPicked(new Set());
      setEncrypt(false); setError(""); setResolving(false); setSubmitting(false);
    }
  }, [open]);

  const totalSize = useMemo(() => {
    if (!share) return 0;
    return share.files.filter((f) => picked.has(f.fs_id)).reduce((n, f) => n + f.size, 0);
  }, [share, picked]);
  const hasDir = useMemo(
    () => !!share && share.files.some((f) => picked.has(f.fs_id) && f.is_dir),
    [share, picked],
  );

  if (!open) return null;

  async function resolve() {
    setResolving(true);
    setError("");
    try {
      const r = await api.shareResolve(link, pwd || undefined);
      setShare(r);
      setPicked(new Set(r.files.map((f) => f.fs_id)));
    } catch (e: any) {
      setError(e.message || "解析失败");
    } finally {
      setResolving(false);
    }
  }

  function toggle(f: ShareFile) {
    setPicked((prev) => {
      const next = new Set(prev);
      if (next.has(f.fs_id)) next.delete(f.fs_id);
      else next.add(f.fs_id);
      return next;
    });
  }

  async function submit() {
    if (!share || picked.size === 0) return;
    setSubmitting(true);
    setError("");
    try {
      const chosen = share.files.filter((f) => picked.has(f.fs_id));
      const r = await api.shareImport({
        surl: share.surl, sekey: share.sekey, share_id: share.share_id, uk: share.uk,
        fs_ids: chosen.map((f) => f.fs_id), total_size: totalSize,
        target_dir: targetDir, encrypt,
      });
      const name = chosen.length === 1 ? chosen[0].name : `分享转存（${chosen.length} 项）`;
      onSubmitted(r.job_id, name, totalSize);
    } catch (e: any) {
      setError(e.message || "提交转存任务失败");
      setSubmitting(false);
    }
  }

  return (
    <div className="modal-overlay" onClick={onClose}>
      <div className="modal" style={{ width: 560, maxWidth: "94vw" }} onClick={(e) => e.stopPropagation()}>
        <div className="modal-h">分享链接转存</div>
        <div className="muted" style={{ fontSize: 12, marginBottom: 12 }}>
          粘贴别人分享的百度网盘链接，文件将转存到：<b>{targetDir}</b>
        </div>

        <div className="field">
          <label>分享链接</label>
          <textarea
            rows={2}
            value={link}
            disabled={resolving || submitting}
            onChange={(e) => setLink(e.target.value)}
            placeholder={"支持整段分享文本，例如：\n链接: https://pan.baidu.com/s/1xxxx 提取码: abcd"}
            style={{ width: "100%", resize: "vertical" }}
          />
        </div>
        <div className="row" style={{ alignItems: "flex-end", gap: 8 }}>
          <div className="field" style={{ flex: 1 }}>
            <label>提取码（链接中已包含时可留空）</label>
            <input value={pwd} disabled={resolving || submitting} maxLength={4}
              onChange={(e) => setPwd(e.target.value)} placeholder="abcd" />
          </div>
          <button className="btn" disabled={resolving || submitting || !link.trim()} onClick={resolve}>
            {resolving ? "解析中…" : share ? "重新解析" : "解析链接"}
          </button>
        </div>

        {share && (
          <>
            <div className="field" style={{ marginTop: 8 }}>
              <label>选择要转存的内容（{picked.size}/{share.files.length}）</label>
              <div style={{ maxHeight: 220, overflowY: "auto", border: "1px solid var(--border, #e2e8f0)", borderRadius: 6 }}>
                {share.files.map((f) => (
                  <label key={f.fs_id} style={{ display: "flex", alignItems: "center", gap: 8, padding: "6px 10px", cursor: "pointer" }}>
                    <input type="checkbox" checked={picked.has(f.fs_id)} disabled={submitting}
                      onChange={() => toggle(f)} style={{ width: "auto" }} />
                    <span>{f.is_dir ? "📁" : "📄"}</span>
                    <span style={{ flex: 1, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{f.name}</span>
                    <span className="muted" style={{ fontSize: 12 }}>{f.is_dir ? "文件夹" : humanSize(f.size)}</span>
                  </label>
                ))}
                {share.files.length === 0 && <div className="empty" style={{ padding: 16 }}>分享内容为空</div>}
              </div>
              <small className="field-help">
                共 {humanSize(totalSize)}{hasDir ? "（文件夹整体转存，其内容大小以转存结果为准）" : ""}
              </small>
            </div>
            <div className="field">
              <label style={{ display: "flex", alignItems: "center", gap: 8, cursor: "pointer" }}>
                <input type="checkbox" checked={encrypt} disabled={submitting}
                  onChange={(e) => setEncrypt(e.target.checked)} style={{ width: "auto" }} />
                加密存储
              </label>
              <small className="field-help">
                默认在百度云端直接复制（秒级完成，明文原样保存）。勾选后文件会经服务器下载、AES-256 加密后重新上传，
                更安全但消耗服务器带宽且耗时明显更长。
              </small>
            </div>
          </>
        )}

        {error && <div className="err-text" style={{ marginTop: 8 }}>{error}</div>}

        <div className="row" style={{ justifyContent: "flex-end", gap: 8, marginTop: 16 }}>
          <button className="btn ghost" disabled={submitting} onClick={onClose}>取消</button>
          <button className="btn" disabled={!share || picked.size === 0 || submitting} onClick={submit}>
            {submitting ? "提交中…" : "开始转存"}
          </button>
        </div>
      </div>
    </div>
  );
}
