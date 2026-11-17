import { useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { api, auth, FileItem, Storage, downloadZipUrl } from "../lib/api";
import { Card, statusBadge, useToast, useDialog } from "../ui";
import { humanSize, fmtTime } from "../lib/format";
import { useTransfers } from "../transfer";
import { JobEvent } from "../App";
import LocalFilePicker from "../LocalFilePicker";
import { LocalEntry } from "../lib/api";
import FileSearch from "../components/FileSearch";
import { PathNotePanel, NoteBadge, NotePreview } from "../components/PathNotePanel";
import { normDir, fullPath, parentDir, baseName, searchLocateTarget } from "../lib/searchLocate";

// ---- path helpers ----------------------------------------------------------

function extOf(name: string): string {
  const i = name.lastIndexOf(".");
  return i > 0 ? name.slice(i + 1).toUpperCase() + " 文件" : "文件";
}

interface FolderRow { name: string; count: number; size: number; modified: string; protected?: boolean; protectedOwn?: boolean; protectedBy?: string; }

// Derive folders + files directly under `cwd`. Explicit empty dirs (is_dir) are
// merged in so they show even with no children.
function buildListing(files: FileItem[], cwd: string) {
  const dir = normDir(cwd);
  const folders = new Map<string, FolderRow>();
  const here: FileItem[] = [];
  const touch = (name: string): FolderRow => {
    let r = folders.get(name);
    if (!r) { r = { name, count: 0, size: 0, modified: "" }; folders.set(name, r); }
    return r;
  };
  for (const f of files) {
    const p = fullPath(f.path);
    if (!p.startsWith(dir)) continue;
    const rest = p.slice(dir.length);
    if (rest === "") continue;
    const slash = rest.indexOf("/");
    if (slash === -1) {
      if (f.is_dir) {
        const r = touch(rest);
        if (f.modified > r.modified) r.modified = f.modified;
        if (f.protected) { r.protected = true; r.protectedOwn = f.protected_own; r.protectedBy = f.protected_by; }
      } else here.push(f);
    } else {
      const r = touch(rest.slice(0, slash));
      if (!f.is_dir) { r.count++; r.size += f.size; }
      if (f.modified > r.modified) r.modified = f.modified;
    }
  }
  const folderRows = [...folders.values()].sort((a, b) => a.name.localeCompare(b.name));
  here.sort((a, b) => a.path.localeCompare(b.path));
  return { folderRows, here };
}

// ---- icons -----------------------------------------------------------------

function FolderIcon() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" style={{ flexShrink: 0 }}>
      <path fill="#f5b849" d="M10 4H4a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2V8a2 2 0 0 0-2-2h-8l-2-2z" />
    </svg>
  );
}
function FileIcon() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" style={{ flexShrink: 0 }}>
      <path fill="#94a3b8" d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z" />
      <path fill="#cbd5e1" d="M14 2v6h6z" />
    </svg>
  );
}

type Target =
  | { kind: "blank" }
  | { kind: "folder"; name: string; path: string }
  | { kind: "file"; item: FileItem };
interface Menu { x: number; y: number; target: Target; }
interface Clip { path: string; isDir: boolean; }
interface MenuItem { label: string; onClick?: () => void; danger?: boolean; disabled?: boolean; }

export default function Files({ jobs }: { jobs: Record<string, JobEvent> }) {
  const [files, setFiles] = useState<FileItem[]>([]);
  const [storage, setStorage] = useState<Storage | null>(null);
  const [cwd, setCwd] = useState("/");
  const [menu, setMenu] = useState<Menu | null>(null);
  const [clip, setClip] = useState<Clip | null>(null);
  const [localPicker, setLocalPicker] = useState(false);
  const [locatePath, setLocatePath] = useState("");
  const [noteTarget, setNoteTarget] = useState<{ path: string; isDir: boolean; name: string } | null>(null);
  const [noteFlags, setNoteFlags] = useState<Set<string>>(new Set());
  const [notePreview, setNotePreview] = useState<{ path: string; isDir: boolean; name: string; x: number; y: number } | null>(null);

  const openNotePreview = (e: React.MouseEvent, t: { path: string; isDir: boolean; name: string }) => {
    setNotePreview({ ...t, x: e.clientX, y: e.clientY });
  };
  const fileRef = useRef<HTMLInputElement>(null);
  const toast = useToast();
  const dialog = useDialog();
  const transfers = useTransfers();

  const askDelete = async (label: string, p: string, fn: (pwd?: string) => Promise<any>, ok: string) => {
    if (await dialog.confirm({ title: "删除确认", message: `确定删除 ${label} 吗?此操作不可撤销。`, danger: true, confirmLabel: "删除" })) {
      runProtected(p, fn, ok);
    }
  };

  // Session-memory access passwords, keyed by logical path (no trailing slash).
  // pwdFor also matches ancestors, so entering a protected dir once unlocks
  // downloads/operations inside it.
  const pwds = useRef(new Map<string, string>());
  function pwdFor(p: string): string | undefined {
    let cur = fullPath(p).replace(/\/+$/, "");
    while (cur) {
      const v = pwds.current.get(cur);
      if (v) return v;
      const i = cur.lastIndexOf("/");
      if (i <= 0) break;
      cur = cur.slice(0, i);
    }
    return undefined;
  }

  const refreshNotes = async (dir = cwd) => {
    const key = fullPath(dir).replace(/\/+$/, "") || "/";
    try {
      const paths = await api.noteIndex(key, pwdFor(dir));
      setNoteFlags(new Set(paths));
    } catch {
      setNoteFlags(new Set());
    }
  };

  const refresh = async () => {
    try {
      setFiles(await api.listFiles(cwd, pwdFor(cwd)));
      void refreshNotes(cwd);
    } catch (e: any) {
      if (!e?.protected) return;
      const pwd = await dialog.prompt({
        title: "私密目录",
        message: e.owner ? `该目录由「${e.owner}」设置了访问密码` : undefined,
        placeholder: "请输入访问密码",
      });
      if (pwd && pwd.trim()) {
        const key = normDir(cwd).replace(/\/+$/, "") || "/";
        try {
          const list = await api.listFiles(cwd, pwd.trim());
          pwds.current.set(key, pwd.trim());
          setFiles(list);
          void refreshNotes(cwd);
          return;
        } catch (e2: any) {
          toast("err", e2.message || "访问密码错误");
        }
      }
      setCwd(parentDir(cwd));
    }
  };
  const refreshStorage = () => api.storage().then(setStorage).catch(() => {});
  useEffect(() => { refresh(); refreshStorage(); }, [cwd]);

  const doneCount = Object.values(jobs).filter((j) => j.status === "done").length;
  useEffect(() => { refresh(); refreshStorage(); }, [doneCount]);

  useEffect(() => {
    if (!locatePath) return;
    const el = document.querySelector("tr.locate") as HTMLElement | null;
    el?.scrollIntoView({ block: "center", behavior: "smooth" });
    const t = setTimeout(() => setLocatePath(""), 4000);
    return () => clearTimeout(t);
  }, [locatePath, files]);

  function locate(item: FileItem) {
    const t = searchLocateTarget(item);
    setCwd(t.cwd);
    setLocatePath(t.highlightPath);
  }

  const { folderRows, here } = useMemo(() => buildListing(files, cwd), [files, cwd]);
  const segments = cwd.split("/").filter(Boolean);

  function goTo(depth: number) { setCwd(depth <= 0 ? "/" : "/" + segments.slice(0, depth).join("/") + "/"); }
  function up() { goTo(segments.length - 1); }
  function enter(name: string) { setCwd(normDir(cwd) + name + "/"); }
  const closeMenu = () => setMenu(null);

  function openMenu(e: React.MouseEvent, target: Target) {
    e.preventDefault();
    setMenu({ x: e.clientX, y: e.clientY, target });
  }

  // Keep the context menu inside the viewport (it opens at the cursor, which
  // may be near the right/bottom edge).
  const menuRef = useRef<HTMLDivElement>(null);
  useLayoutEffect(() => {
    if (!menu || !menuRef.current) return;
    const r = menuRef.current.getBoundingClientRect();
    let nx = menu.x, ny = menu.y;
    if (r.right > window.innerWidth - 8) nx = Math.max(8, window.innerWidth - 8 - r.width);
    if (r.bottom > window.innerHeight - 8) ny = Math.max(8, window.innerHeight - 8 - r.height);
    if (nx !== menu.x || ny !== menu.y) setMenu({ ...menu, x: nx, y: ny });
  }, [menu]);

  async function runProtected(p: string, action: (pwd?: string) => Promise<any>, ok: string) {
    const cached = pwdFor(p);
    try {
      await action(cached);
      toast("ok", ok);
      refresh(); refreshStorage();
      return;
    } catch (e: any) {
      if (!e?.protected) {
        toast("err", e.message || "操作失败");
        return;
      }
      const pwd = await dialog.prompt({
        title: "私密内容",
        message: e.owner ? `该内容（或父目录）由「${e.owner}」设置了访问密码` : undefined,
        placeholder: "请输入访问密码",
      });
      if (!pwd || !pwd.trim()) return;
      try {
        await action(pwd.trim());
        pwds.current.set(fullPath(p).replace(/\/+$/, "") || "/", pwd.trim());
        toast("ok", ok);
        refresh(); refreshStorage();
      } catch (e2: any) {
        toast("err", e2?.status === 403 ? "访问密码错误" : (e2.message || "操作失败"));
      }
    }
  }

  // ---- actions -------------------------------------------------------------

  function pickFile() {
    if (transfers.clientMode) setLocalPicker(true);
    else fileRef.current?.click();
  }

  async function onLocalFilePicked(entry: LocalEntry) {
    if (storage && storage.total > 0 && entry.size > storage.free) {
      toast("err", `文件 ${humanSize(entry.size)} 超过系统可用容量 ${humanSize(storage.free)}，上传已取消`);
      return;
    }
    try {
      await transfers.startClientUpload(entry.path, cwd);
      setLocalPicker(false);
      toast("ok", "本机上传任务已创建，可在「传输管理」查看进度");
    } catch (e: any) {
      toast("err", e.message || "创建上传任务失败");
    }
  }

  function onFilePicked() {
    if (cwd === "/") { toast("info", "请先进入一个挂载节点"); return; }
    const f = fileRef.current?.files?.[0];
    if (!f) return;
    if (storage && storage.total > 0 && f.size > storage.free) {
      toast("err", `文件 ${humanSize(f.size)} 超过系统可用容量 ${humanSize(storage.free)},上传已取消`);
      if (fileRef.current) fileRef.current.value = "";
      return;
    }
    transfers.startUpload(f, normDir(cwd) + f.name);
    toast("ok", "已加入上传队列,可在「传输管理」查看进度");
    if (fileRef.current) fileRef.current.value = "";
  }

  function gotoShareImport() {
    window.location.hash = "/share?dir=" + encodeURIComponent(normDir(cwd).replace(/\/$/, ""));
  }

  async function newFolder() {
    if (cwd === "/") { toast("info", "请先进入一个挂载节点"); return; }
    const name = await dialog.prompt({ title: "新建文件夹", placeholder: "文件夹名称" });
    if (!name || !name.trim()) return;
    runProtected(cwd, (pwd) => api.createFolder(normDir(cwd) + name.trim(), pwd), "文件夹已创建");
  }

  async function doCreateShare(item: { id?: number; path: string; is_dir: boolean }) {
    const choices: { label: string; value: string }[] = [
      { label: "1 小时", value: "1h" },
      { label: "1 天", value: "1d" },
      { label: "7 天", value: "7d" },
      { label: "永久有效", value: "never" },
    ];
    const expiresIn = await dialog.prompt({
      title: "创建分享链接",
      message: "请选择链接有效期",
      defaultValue: "1d",
      placeholder: "1h / 1d / 7d / never",
    });
    if (!expiresIn) return;
    const valid = choices.some((c) => c.value === expiresIn.trim());
    if (!valid) {
      toast("err", "有效期必须是 1h、1d、7d 或 never");
      return;
    }
    try {
      const res = await api.createShare({ id: item.id, path: item.path, expires_in: expiresIn.trim() });
      await navigator.clipboard.writeText(res.url);
      toast("ok", `分享链接已复制到剪贴板（有效期：${choices.find((c) => c.value === expiresIn.trim())?.label}）`);
    } catch (e: any) {
      toast("err", e?.message || "创建分享链接失败");
    }
  }

  async function doDownloadZip(dirPath: string) {
    let pwd = pwdFor(dirPath);
    if (!pwd) {
      // Try to see if this directory or an ancestor is protected.
      try {
        await api.checkAccess(undefined, dirPath, "");
      } catch (e: any) {
        if (e?.status === 403 && e?.protected) {
          const entered = await dialog.prompt({
            title: "私密目录",
            message: e?.owner ? `该目录由「${e.owner}」设置了访问密码` : undefined,
            placeholder: "请输入访问密码",
          });
          if (!entered || !entered.trim()) return;
          pwd = entered.trim();
          try {
            await api.checkAccess(undefined, dirPath, pwd);
            pwds.current.set(fullPath(dirPath), pwd);
          } catch (err: any) {
            toast("err", "访问密码错误");
            return;
          }
        }
      }
    }
    window.open(downloadZipUrl(dirPath, pwd), "_blank");
  }

  async function doDownload(item: FileItem) {
    let pwd = pwdFor(item.path);
    if (item.protected && !item.protected_own && !pwd) {
      const entered = await dialog.prompt({
        title: "私密文件",
        message: item.protected_by ? `该文件由「${item.protected_by}」设置了访问密码` : undefined,
        placeholder: "请输入访问密码",
      });
      if (!entered || !entered.trim()) return;
      pwd = entered.trim();
    }
    // Verify the password before creating a transfer so a wrong password is
    // reported immediately instead of appearing as a network error.
    if (pwd) {
      try {
        // Remote-download items use the Baidu fs_id as id and must be checked by path.
        const checkId = item.download_url ? undefined : item.id;
        await api.checkAccess(checkId, item.path, pwd);
        if (item.protected && !item.protected_own) pwds.current.set(fullPath(item.path), pwd);
      } catch (e: any) {
        if (e?.status === 403) toast("err", "访问密码错误");
        else toast("err", e?.message || "访问校验失败");
        return;
      }
    }
    try {
      await transfers.startDownload({ ...item, pwd });
      toast("info", transfers.clientMode
        ? "本机下载任务已创建，可在「传输管理」查看进度"
        : "已加入下载队列，可在「传输管理」查看进度");
    } catch (e: any) {
      toast("err", e.message || "创建下载任务失败");
    }
  }

  async function doRename(p: string) {
    const cur = baseName(p);
    const name = await dialog.prompt({ title: "重命名", defaultValue: cur, placeholder: "新名称" });
    if (!name || !name.trim() || name.trim() === cur) return;
    runProtected(p, (pwd) => api.move(p, parentDir(p) + name.trim(), pwd), "已重命名");
  }

  function paste(targetDir: string) {
    if (!clip) return;
    const dst = normDir(targetDir) + baseName(clip.path);
    runProtected(clip.path, (pwd) => api.move(clip.path, dst, pwd), "已粘贴").then(() => setClip(null));
  }

  // ---- password protection (私密) -------------------------------------------

  async function setPassword(p: string, isDir: boolean) {
    const newPwd = await dialog.prompt({ title: "设为私密", placeholder: "访问密码（4-64 个字符）" });
    if (!newPwd || !newPwd.trim()) return;
    runProtected(p, (pwd) => api.setProtect(p, isDir, newPwd.trim(), pwd), "已设置访问密码，其他用户需输入密码才能访问");
  }

  async function removeProtect(p: string) {
    if (await dialog.confirm({ title: "取消私密", message: `确定移除 ${baseName(p)} 的访问密码吗？`, confirmLabel: "移除" })) {
      runProtected(p, (pwd) => api.unprotect(p, pwd), "已取消私密");
    }
  }

  async function viewPassword(p: string, isDir: boolean) {
    try {
      const info = await api.getProtect(p);
      if (!info.protected) { toast("info", "该项未设置密码"); refresh(); return; }
      if (info.password === undefined) { toast("err", "只有设置者本人或管理员可以查看密码"); return; }
      const next = await dialog.prompt({
        title: `访问密码（设置者：${info.owner || "未知"}）`,
        defaultValue: info.password,
        placeholder: "修改后确认即可更新密码",
      });
      if (next && next.trim() && next.trim() !== info.password) {
        runProtected(p, (pwd) => api.setProtect(p, isDir, next.trim(), pwd), "访问密码已更新");
      }
    } catch (e: any) {
      toast("err", e.message);
    }
  }

  // ---- context menu items --------------------------------------------------

  function menuItems(t: Target): (MenuItem | "div")[] {
    const wrap = (fn: () => void) => () => { closeMenu(); fn(); };
    // owner accesses passwordless; admin still needs the password to access
    // but can always view/manage it, so both get the manage menu items.
    const canManage = (own?: boolean) => own || auth.role === "admin";
    if (t.kind === "file") {
      const it = t.item;
      const fp = fullPath(it.path);
      const protect: (MenuItem | "div")[] = it.protected
        ? canManage(it.protected_own)
          ? [{ label: "查看/修改密码", onClick: wrap(() => viewPassword(fp, false)) },
             { label: "取消私密", onClick: wrap(() => removeProtect(fp)) }]
          : []
        : [{ label: "设为私密…", onClick: wrap(() => setPassword(fp, false)) }];
      return [
        { label: "下载", onClick: wrap(() => doDownload(it)), disabled: it.status !== "ready" },
        { label: "创建分享…", onClick: wrap(() => doCreateShare({ id: it.id, path: fp, is_dir: false })) },
        { label: "剪切", onClick: wrap(() => setClip({ path: fp, isDir: false })) },
        { label: "重命名", onClick: wrap(() => doRename(fp)) },
        { label: "备注", onClick: wrap(() => setNoteTarget({ path: fp, isDir: false, name: baseName(it.path) })) },
        ...protect,
        "div",
        { label: "删除", danger: true, onClick: wrap(() => askDelete(baseName(it.path), it.path, (pwd) => api.deletePath(it.path, pwd), "已删除")) },
      ];
    }
    if (t.kind === "folder") {
      const atRoot = cwd === "/";
      const row = folderRows.find((d) => d.name === t.name);
      const protect: (MenuItem | "div")[] = atRoot
        ? []
        : row?.protected
          ? canManage(row.protectedOwn)
            ? [{ label: "查看/修改密码", onClick: wrap(() => viewPassword(t.path, true)) },
               { label: "取消私密", onClick: wrap(() => removeProtect(t.path)) }]
            : []
          : [{ label: "设为私密…", onClick: wrap(() => setPassword(t.path, true)) }];
      return [
        { label: "打开", onClick: wrap(() => enter(t.name)) },
        { label: "下载目录", onClick: wrap(() => doDownloadZip(t.path)), disabled: atRoot },
        { label: "创建分享…", onClick: wrap(() => doCreateShare({ path: t.path, is_dir: true })), disabled: atRoot },
        { label: "剪切", onClick: wrap(() => setClip({ path: t.path, isDir: true })), disabled: atRoot },
        { label: "粘贴到此", onClick: wrap(() => paste(t.path)), disabled: !clip || atRoot },
        { label: "重命名", onClick: wrap(() => doRename(t.path)), disabled: atRoot },
        { label: "备注", onClick: wrap(() => setNoteTarget({ path: t.path, isDir: true, name: t.name })), disabled: atRoot },
        ...protect,
        "div",
        { label: "删除", danger: true, disabled: atRoot, onClick: wrap(() => askDelete(`文件夹 ${t.name} 及其内容`, t.path, (pwd) => api.deleteFolder(t.path, pwd), "已删除")) },
      ];
    }
    return [
      { label: "新建文件夹", onClick: wrap(newFolder), disabled: cwd === "/" },
      { label: "上传文件", onClick: wrap(pickFile), disabled: cwd === "/" },
      { label: "分享转存…", onClick: wrap(gotoShareImport), disabled: cwd === "/" },
      { label: clip ? `粘贴 “${baseName(clip.path)}”` : "粘贴", onClick: wrap(() => paste(cwd)), disabled: !clip || cwd === "/" },
      { label: "当前目录备注", onClick: wrap(() => setNoteTarget({ path: fullPath(cwd).replace(/\/+$/, "") || "/", isDir: true, name: baseName(cwd) || cwd })), disabled: cwd === "/" },
      "div",
      { label: "刷新", onClick: wrap(() => { refresh(); refreshStorage(); }) },
    ];
  }

  const empty = folderRows.length === 0 && here.length === 0;
  const usedPct = storage && storage.total > 0 ? Math.round((storage.used / storage.total) * 100) : 0;
  const cutPath = clip?.path;
  const cwdKey = fullPath(cwd).replace(/\/+$/, "") || "/";

  return (
    <Card
      title="文件资源管理器"
      action={
        <div className="row">
          <button className="btn ghost sm" onClick={newFolder} disabled={cwd === "/"}>新建文件夹</button>
          <button className="btn ghost sm" onClick={gotoShareImport} disabled={cwd === "/"} title="转存别人分享的百度网盘链接">分享转存</button>
          <button className="btn" onClick={pickFile} disabled={cwd === "/"}>{transfers.clientMode ? "选择本机文件" : "上传文件"}</button>
        </div>
      }
    >
      <input ref={fileRef} type="file" style={{ display: "none" }} onChange={onFilePicked} />
      <LocalFilePicker open={localPicker} onClose={() => setLocalPicker(false)} onSelect={onLocalFilePicked} />

      {/* capacity */}
      <div className="cap-wrap">
        <div className="cap-bar"><span style={{ width: `${usedPct}%`, background: usedPct > 85 ? "#dc2626" : "#6366f1" }} /></div>
        <div className="cap-text">
          {storage && storage.total > 0
            ? <>总 {humanSize(storage.total)} · 已用 {humanSize(storage.used)} ({usedPct}%) · 可用 <b>{humanSize(storage.free)}</b></>
            : "容量未知（请联系管理员接入网盘账号）"}
        </div>
      </div>

      {/* breadcrumb + toolbar */}
      <div className="explorer-bar">
        <button className="btn ghost sm" onClick={up} disabled={segments.length === 0}>← 上级</button>
        <div className="crumbs">
          <span className={`crumb${segments.length === 0 ? " active" : ""}`} onClick={() => goTo(0)}>根目录</span>
          {segments.map((s, i) => (
            <span key={i}>
              <span className="crumb-sep">/</span>
              <span className={`crumb${i === segments.length - 1 ? " active" : ""}`} onClick={() => goTo(i + 1)}>{s}</span>
            </span>
          ))}
        </div>
        <FileSearch onLocate={locate} />
        {cwd !== "/" && (
          noteFlags.has(cwdKey) ? (
            <NoteBadge title="查看当前目录备注"
              onClick={(e) => openNotePreview(e, { path: cwdKey, isDir: true, name: baseName(cwd) || cwd })} />
          ) : (
            <button className="btn ghost sm" title="当前目录备注"
              onClick={() => setNoteTarget({ path: cwdKey, isDir: true, name: baseName(cwd) || cwd })}>
              备注
            </button>
          )
        )}
        <div className="crumb-count">
          {clip && <span className="clip-hint">已剪切: {baseName(clip.path)} · <span className="link" onClick={() => paste(cwd)}>粘贴到此</span></span>}
          <span>{folderRows.length} 个文件夹 · {here.length} 个文件</span>
        </div>
      </div>

      <div className="explorer-body" onContextMenu={(e) => openMenu(e, { kind: "blank" })}>
        <table className="explorer">
          <thead>
            <tr><th>名称</th><th style={{ width: 120 }}>大小</th><th style={{ width: 180 }}>修改日期</th><th style={{ width: 140 }}>类型</th><th style={{ width: 200 }}></th></tr>
          </thead>
          <tbody>
            {folderRows.map((d) => {
              const dp = normDir(cwd) + d.name;
              return (
                <tr key={"d:" + d.name} className={"row-folder" + (noteFlags.has(dp) ? " has-note" : "")} style={cutPath === dp ? { opacity: 0.5 } : undefined}
                  onDoubleClick={() => enter(d.name)}
                  onContextMenu={(e) => { e.stopPropagation(); openMenu(e, { kind: "folder", name: d.name, path: dp }); }}>
                  <td><div className="name-cell" onClick={() => enter(d.name)}><FolderIcon /> <span className="name-link">{d.name}</span>{d.protected && <span title={d.protectedOwn ? "私密目录（你可管理）" : `私密目录${d.protectedBy ? `（由「${d.protectedBy}」设置）` : ""}`}>🔒</span>}{noteFlags.has(dp) && <NoteBadge onClick={(e) => openNotePreview(e, { path: dp, isDir: true, name: d.name })} />}</div></td>
                  {/* the listing only covers direct children, so a folder's
                      own size is unknown — show a dash instead of "0 B" */}
                  <td className="muted">{d.count > 0 ? humanSize(d.size) : "—"}</td>
                  <td className="muted">{d.modified ? fmtTime(d.modified) : "—"}</td>
                  <td className="muted">{d.count > 0 ? `文件夹 (${d.count})` : "文件夹"}</td>
                  <td></td>
                </tr>
              );
            })}
            {here.map((f) => {
              const name = baseName(f.path);
              return (
                <tr key={"f:" + f.id}
                  className={[locatePath === fullPath(f.path) ? "locate" : "", noteFlags.has(fullPath(f.path)) ? "has-note" : ""].filter(Boolean).join(" ") || undefined}
                  style={cutPath === fullPath(f.path) ? { opacity: 0.5 } : undefined}
                  onContextMenu={(e) => { e.stopPropagation(); openMenu(e, { kind: "file", item: f }); }}>
                  <td><div className="name-cell"><FileIcon /> <span>{name}</span>{f.protected && <span title={f.protected_own ? "私密文件（你可管理）" : `私密文件${f.protected_by ? `（由「${f.protected_by}」设置）` : ""}`}>🔒</span>}{f.encrypted && <span title="加密存储：云端仅保存密文，下载时自动解密">🛡️</span>}{noteFlags.has(fullPath(f.path)) && <NoteBadge onClick={(e) => openNotePreview(e, { path: fullPath(f.path), isDir: false, name })} />} {f.status !== "ready" && statusBadge(f.status)}</div></td>
                  <td className="muted">{humanSize(f.size)}</td>
                  <td className="muted">{fmtTime(f.modified || f.created_at)}</td>
                  <td className="muted">{extOf(name)}</td>
                  <td>
                    <div className="row">
                      {f.status === "ready" && <button className="btn ghost sm" onClick={() => doDownload(f)}>下载</button>}
                      <button className="btn ghost sm" onClick={() => doRename(fullPath(f.path))}>重命名</button>
                      <button className="btn danger sm" onClick={() => askDelete(name, f.path, (pwd) => api.deletePath(f.path, pwd), "已删除")}>删除</button>
                    </div>
                  </td>
                </tr>
              );
            })}
            {empty && <tr><td colSpan={5} className="empty">此目录为空 · 右键可新建文件夹或上传</td></tr>}
          </tbody>
        </table>
      </div>

      {/* Portal to <body>: the card's backdrop-filter (and hover transform)
          makes it the containing block for position:fixed, which would shift
          the menu away from the cursor and shrink the overlay to the card. */}
      {menu && createPortal(
        <>
          <div className="ctx-overlay" onClick={closeMenu} onContextMenu={(e) => { e.preventDefault(); closeMenu(); }} />
          <div className="ctx-menu" ref={menuRef} style={{ left: menu.x, top: menu.y }}>
            {menuItems(menu.target).map((it, i) =>
              it === "div"
                ? <div key={i} className="ctx-div" />
                : <div key={i} className={`ctx-item${it.danger ? " danger" : ""}${it.disabled ? " disabled" : ""}`}
                    onClick={() => !it.disabled && it.onClick?.()}>{it.label}</div>
            )}
          </div>
        </>,
        document.body,
      )}
      {notePreview && createPortal(
        <NotePreview
          target={notePreview}
          pwd={pwdFor(notePreview.path)}
          x={notePreview.x}
          y={notePreview.y}
          onClose={() => setNotePreview(null)}
          onEdit={() => {
            const { path, isDir, name } = notePreview;
            setNotePreview(null);
            setNoteTarget({ path, isDir, name });
          }}
        />,
        document.body,
      )}
      {noteTarget && createPortal(
        <PathNotePanel
          target={noteTarget}
          pwd={pwdFor(noteTarget.path)}
          onClose={() => setNoteTarget(null)}
          onChanged={() => { void refreshNotes(cwd); }}
        />,
        document.body,
      )}
    </Card>
  );
}
