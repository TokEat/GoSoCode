import { useEffect, useMemo, useRef, useState } from "react";
import { api, ShareFile, ShareResolveResult } from "../lib/api";
import { Card, useToast, useDialog } from "../ui";
import { humanSize } from "../lib/format";
import { useTransfers } from "../transfer";

// Reads the optional ?dir= query from the hash route (#/share?dir=/node/sub)
// so the Files page can hand over the directory the user was browsing.
function dirFromHash(): string {
  const q = window.location.hash.split("?")[1] || "";
  const dir = new URLSearchParams(q).get("dir") || "";
  return dir.startsWith("/") ? dir : "";
}

function mountOf(p: string): string {
  return "/" + (p.split("/")[1] || "");
}

export default function ShareImport() {
  const [link, setLink] = useState("");
  const [pwd, setPwd] = useState("");
  const [resolving, setResolving] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState("");
  const [share, setShare] = useState<ShareResolveResult | null>(null);
  const [picked, setPicked] = useState<Set<string>>(new Set());
  const [encrypt, setEncrypt] = useState(false);
  const [randName, setRandName] = useState(false);
  const [mounts, setMounts] = useState<string[]>([]);
  const [targetDir, setTargetDir] = useState("");
  const [subDirs, setSubDirs] = useState<string[]>([]);
  const [dirLoading, setDirLoading] = useState(false);
  const toast = useToast();
  const dialog = useDialog();
  const transfers = useTransfers();

  // Session-memory access passwords for 私密 directories, keyed by path;
  // matches ancestors so one entry unlocks the whole subtree.
  const pwds = useRef(new Map<string, string>());
  function pwdFor(p: string): string | undefined {
    let cur = p.replace(/\/+$/, "");
    while (cur) {
      const v = pwds.current.get(cur);
      if (v) return v;
      cur = cur.slice(0, cur.lastIndexOf("/"));
    }
    return undefined;
  }

  async function askAccessPwd(owner?: string): Promise<string | null> {
    return dialog.prompt({
      title: "私密目录",
      message: owner ? `该目录由「${owner}」设置了访问密码` : "该目录设置了访问密码",
      placeholder: "请输入访问密码",
    });
  }

  // Mount points are the top-level directories of the shared filesystem.
  useEffect(() => {
    api.listFiles("/").then((files) => {
      const ms = files.filter((f) => f.is_dir && f.mount).map((f) => f.path);
      setMounts(ms);
      const want = dirFromHash();
      setTargetDir(want && ms.some((m) => want === m || want.startsWith(m + "/")) ? want : (ms[0] || ""));
    }).catch(() => {});
  }, []);

  // Load browsable subdirectories of the current target. A protected (私密)
  // directory prompts for its access password instead of erroring out; a wrong
  // or cancelled password steps back to the parent directory.
  useEffect(() => {
    if (!targetDir) { setSubDirs([]); return; }
    let stale = false;
    const back = () => setTargetDir(targetDir.includes("/", 1) ? targetDir.slice(0, targetDir.lastIndexOf("/")) : targetDir);
    (async () => {
      setDirLoading(true);
      try {
        const files = await api.listFiles(targetDir, pwdFor(targetDir));
        if (!stale) setSubDirs(files.filter((f) => f.is_dir && f.status !== "deleted").map((f) => f.path));
      } catch (e: any) {
        if (stale) return;
        setSubDirs([]);
        if (!e?.protected) return;
        const pwd = await askAccessPwd(e.owner);
        if (stale) return;
        if (!pwd || !pwd.trim()) { back(); return; }
        try {
          const files = await api.listFiles(targetDir, pwd.trim());
          if (stale) return;
          pwds.current.set(targetDir, pwd.trim());
          setSubDirs(files.filter((f) => f.is_dir && f.status !== "deleted").map((f) => f.path));
        } catch {
          if (!stale) { toast("err", "访问密码错误"); back(); }
        }
      } finally {
        if (!stale) setDirLoading(false);
      }
    })();
    return () => { stale = true; };
  }, [targetDir]);

  const mount = targetDir ? mountOf(targetDir) : "";

  const totalSize = useMemo(() => {
    if (!share) return 0;
    return share.files.filter((f) => picked.has(f.fs_id)).reduce((n, f) => n + f.size, 0);
  }, [share, picked]);
  const hasDir = useMemo(
    () => !!share && share.files.some((f) => picked.has(f.fs_id) && f.is_dir),
    [share, picked],
  );

  async function resolve() {
    setResolving(true);
    setError("");
    try {
      const r = await api.shareResolve(link, pwd || undefined);
      setShare(r);
      setPicked(new Set(r.files.map((f) => f.fs_id)));
    } catch (e: any) {
      setShare(null);
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
    if (!share || picked.size === 0 || !targetDir) return;
    setSubmitting(true);
    setError("");
    const chosen = share.files.filter((f) => picked.has(f.fs_id));
    const doImport = (accessPwd?: string) => api.shareImport({
      surl: share.surl, sekey: share.sekey, share_id: share.share_id, uk: share.uk,
      fs_ids: chosen.map((f) => f.fs_id), total_size: totalSize,
      target_dir: targetDir, encrypt, rand_name: randName,
    }, accessPwd);
    const onSubmitted = (r: { job_id: string }) => {
      const name = chosen.length === 1 ? chosen[0].name : `分享转存（${chosen.length} 项）`;
      if (!transfers.clientMode) {
        transfers.startShareImport(r.job_id, name, targetDir, totalSize);
        toast("ok", "转存任务已提交，可在「传输管理」查看进度");
      } else {
        toast("ok", "转存任务已提交，转存在服务器上进行，稍后刷新目录即可看到文件");
      }
      setShare(null); setPicked(new Set()); setLink(""); setPwd("");
    };
    try {
      onSubmitted(await doImport(pwdFor(targetDir)));
    } catch (e: any) {
      // Protected target: ask for the access password and retry instead of
      // failing outright; only a wrong password becomes an error.
      if (e?.protected) {
        const accessPwd = await askAccessPwd(e.owner);
        if (accessPwd && accessPwd.trim()) {
          try {
            const r = await doImport(accessPwd.trim());
            pwds.current.set(targetDir, accessPwd.trim());
            onSubmitted(r);
          } catch (e2: any) {
            setError(e2?.status === 403 ? "访问密码错误" : (e2.message || "提交转存任务失败"));
          }
        }
      } else {
        setError(e.message || "提交转存任务失败");
      }
    } finally {
      setSubmitting(false);
    }
  }

  const allPicked = !!share && share.files.length > 0 && picked.size === share.files.length;

  return (
    <div className="share-grid">
      {/* ---- left: link + resolved content ---- */}
      <div style={{ display: "grid", gap: 18 }}>
        <Card title="分享链接">
          <div className="muted" style={{ fontSize: 12, marginBottom: 12 }}>
            粘贴别人分享的百度网盘链接（支持整段分享文本），解析后选择要转存的内容
          </div>
          <div className="field share-link-box">
            <textarea
              value={link}
              disabled={resolving || submitting}
              onChange={(e) => setLink(e.target.value)}
              placeholder={"例如：\n通过网盘分享的文件：xxx.zip\n链接: https://pan.baidu.com/s/1xxxx?pwd=abcd 提取码: abcd"}
            />
          </div>
          <div className="row" style={{ alignItems: "flex-end", gap: 10 }}>
            <div className="field" style={{ flex: "0 0 220px", marginBottom: 0 }}>
              <label>提取码（链接中已包含时可留空）</label>
              <input value={pwd} disabled={resolving || submitting} maxLength={4}
                onChange={(e) => setPwd(e.target.value)} placeholder="abcd" />
            </div>
            <button className="btn" disabled={resolving || submitting || !link.trim()} onClick={resolve}>
              {resolving ? "解析中…" : share ? "重新解析" : "解析链接"}
            </button>
            <div style={{ flex: 1 }} />
            {error && <div className="err-text">{error}</div>}
          </div>
        </Card>

        <Card title="分享内容">
          {!share && (
            <div className="empty">解析分享链接后，这里会列出可转存的文件</div>
          )}
          {share && (
            <div className="share-file-list">
              <div className="share-list-head">
                <span>已选 {picked.size} / {share.files.length} 项 · 共 {humanSize(totalSize)}
                  {hasDir ? "（文件夹整体转存，其内容大小以转存结果为准）" : ""}</span>
                {share.files.length > 1 && (
                  <a style={{ cursor: "pointer" }}
                    onClick={() => setPicked(allPicked ? new Set() : new Set(share.files.map((f) => f.fs_id)))}>
                    {allPicked ? "全不选" : "全选"}
                  </a>
                )}
              </div>
              <div className="share-file-scroll">
                {share.files.map((f) => (
                  <label key={f.fs_id} className="share-file-row">
                    <input type="checkbox" checked={picked.has(f.fs_id)} disabled={submitting}
                      onChange={() => toggle(f)} />
                    <span>{f.is_dir ? "📁" : "📄"}</span>
                    <span style={{ flex: 1, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{f.name}</span>
                    <span className="muted" style={{ fontSize: 12 }}>{f.is_dir ? "文件夹" : humanSize(f.size)}</span>
                  </label>
                ))}
                {share.files.length === 0 && <div className="empty" style={{ padding: 16 }}>分享内容为空</div>}
              </div>
            </div>
          )}
        </Card>
      </div>

      {/* ---- right: destination + options + submit ---- */}
      <Card title="转存设置">
        <div className="field">
          <label>存储节点</label>
          <select value={mount} disabled={submitting || mounts.length === 0}
            onChange={(e) => setTargetDir(e.target.value)}>
            {mounts.length === 0 && <option value="">（暂无可用节点）</option>}
            {mounts.map((m) => <option key={m} value={m}>{m}</option>)}
          </select>
        </div>

        <div className="field">
          <label>目标目录</label>
          <div className="share-pathbar">
            <span>📁</span>
            <code title={targetDir}>{targetDir || "—"}</code>
            {targetDir && targetDir !== mount && (
              <a style={{ cursor: "pointer", whiteSpace: "nowrap" }}
                onClick={() => !submitting && setTargetDir(targetDir.slice(0, targetDir.lastIndexOf("/")) || mount)}>
                ↑ 上级
              </a>
            )}
          </div>
          <div className="share-dir-list">
            {dirLoading && <div className="muted" style={{ padding: "10px 12px", fontSize: 12 }}>加载中…</div>}
            {!dirLoading && subDirs.map((d) => (
              <div key={d} className="share-dir-row" onClick={() => !submitting && setTargetDir(d)}>
                <span>📁</span>
                <span style={{ flex: 1, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
                  {d.slice(d.lastIndexOf("/") + 1)}
                </span>
                <span className="muted">›</span>
              </div>
            ))}
            {!dirLoading && subDirs.length === 0 && (
              <div className="muted" style={{ padding: "10px 12px", fontSize: 12 }}>当前目录下没有子文件夹</div>
            )}
          </div>
          <small className="field-help">点击文件夹进入下一级，文件将转存到上方显示的目录</small>
        </div>

        <div className="field" style={{ gap: 10 }}>
          <label>转存选项</label>
          <label className="share-opt">
            <input type="checkbox" checked={randName} disabled={submitting}
              onChange={(e) => setRandName(e.target.checked)} />
            <span>
              <b>文件名随机化</b>
              <small>网盘上的文件以随机 UUID 存储（与系统 UUID 存储一致），本系统内仍显示原文件名，
                对应关系记录在云端清单文件中</small>
            </span>
          </label>
          <label className="share-opt">
            <input type="checkbox" checked={encrypt} disabled={submitting}
              onChange={(e) => setEncrypt(e.target.checked)} />
            <span>
              <b>加密存储</b>
              <small>默认在百度云端直接复制（秒级完成，明文原样保存）。勾选后文件会经服务器下载、AES-256
                加密后重新上传，更安全但消耗服务器带宽且耗时明显更长</small>
            </span>
          </label>
        </div>

        <button className="btn" style={{ width: "100%", marginTop: 4 }}
          disabled={!share || picked.size === 0 || !targetDir || submitting} onClick={submit}>
          {submitting ? "提交中…" : share && picked.size > 0
            ? `开始转存（${picked.size} 项 · ${humanSize(totalSize)}）`
            : "开始转存"}
        </button>
      </Card>
    </div>
  );
}
