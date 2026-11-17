import { useCallback, useEffect, useLayoutEffect, useRef, useState, type DragEvent, type ClipboardEvent, type MouseEvent as ReactMouseEvent } from "react";
import { api } from "../lib/api";
import { isSimpleNoteText, noteImageId, renderNote, renderSimpleNote } from "../lib/noteMarkdown";

export type NoteTarget = { path: string; isDir: boolean; name: string };

function AuthImage({ src, alt, pwd, onOpen }: { src: string; alt: string; pwd?: string; onOpen?: (url: string, alt: string) => void }) {
  const [url, setUrl] = useState("");
  const [err, setErr] = useState(false);
  useEffect(() => {
    let gone = false;
    let blobUrl = "";
    const id = noteImageId(src);
    if (id == null) {
      if (/^https?:\/\//i.test(src)) setUrl(src);
      return;
    }
    api.noteImageBlob(id, pwd).then((b) => {
      if (gone) return;
      blobUrl = URL.createObjectURL(b);
      setUrl(blobUrl);
    }).catch(() => { if (!gone) setErr(true); });
    return () => {
      gone = true;
      if (blobUrl) URL.revokeObjectURL(blobUrl);
    };
  }, [src, pwd]);
  if (err) return <span className="note-img-missing">图片无法加载</span>;
  if (!url) return <div className="note-img-ph" aria-hidden="true" />;
  return (
    <button type="button" className="note-img-btn" onClick={() => onOpen?.(url, alt)} title={alt || "查看大图"}>
      <img src={url} alt={alt} className="note-img" />
    </button>
  );
}

function NoteIcon({ size = 18 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" fill="none" aria-hidden="true">
      <path d="M7 3.5h7.2L19.5 9v11.5a1 1 0 0 1-1 1H7a1 1 0 0 1-1-1v-16a1 1 0 0 1 1-1Z" stroke="currentColor" strokeWidth="1.6" />
      <path d="M14 3.5V9h5.5" stroke="currentColor" strokeWidth="1.6" />
      <path d="M9 13h6M9 16.5h4" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" />
    </svg>
  );
}

export function PathNotePanel({
  target, pwd, onClose, onChanged,
}: {
  target: NoteTarget;
  pwd?: string;
  onClose: () => void;
  onChanged: () => void;
}) {
  const [loading, setLoading] = useState(true);
  const [exists, setExists] = useState(false);
  const [saved, setSaved] = useState("");
  const [draft, setDraft] = useState("");
  const [editing, setEditing] = useState(false);
  const [busy, setBusy] = useState(false);
  const [status, setStatus] = useState("");
  const [error, setError] = useState("");
  const [dragOver, setDragOver] = useState(false);
  const [lightbox, setLightbox] = useState<{ url: string; alt: string } | null>(null);
  const area = useRef<HTMLTextAreaElement>(null);
  const fileRef = useRef<HTMLInputElement>(null);
  const savedRef = useRef(saved);
  savedRef.current = saved;

  const load = useCallback(async () => {
    setLoading(true);
    setError("");
    try {
      const n = await api.getNote(target.path, pwd);
      setExists(!!n.exists);
      setSaved(n.body || "");
      setDraft(n.body || "");
      setEditing(false);
    } catch (e: any) {
      setError(e?.message || "无法打开备注");
    } finally {
      setLoading(false);
    }
  }, [target.path, pwd]);

  useEffect(() => { void load(); }, [load]);

  async function persist(body: string, closeEdit = false) {
    setBusy(true);
    setError("");
    try {
      if (!body.trim()) {
        if (exists) await api.deleteNote(target.path, pwd);
        setExists(false);
        setSaved("");
        setDraft("");
        setStatus("已清空");
        onChanged();
        if (closeEdit) setEditing(false);
        return;
      }
      const n = await api.putNote(target.path, target.isDir, body, pwd);
      setExists(!!n.exists);
      setSaved(n.body);
      setDraft(n.body);
      setStatus("已保存");
      onChanged();
      if (closeEdit) setEditing(false);
    } catch (e: any) {
      setError(e?.message || "保存失败");
    } finally {
      setBusy(false);
    }
  }

  useEffect(() => {
    if (!editing || loading) return;
    if (draft === savedRef.current) return;
    if (!draft.trim()) {
      setStatus("清空后点完成才会删除备注");
      return;
    }
    setStatus("正在保存…");
    const t = setTimeout(() => { void persist(draft); }, 700);
    return () => clearTimeout(t);
    // persist is stable enough via target/pwd/exists; we intentionally close over latest exists
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [draft, editing, loading]);

  function insertAtCursor(text: string) {
    const el = area.current;
    if (!el) {
      setDraft((d) => d + text);
      return;
    }
    const start = el.selectionStart;
    const end = el.selectionEnd;
    const next = draft.slice(0, start) + text + draft.slice(end);
    setDraft(next);
    requestAnimationFrame(() => {
      el.focus();
      const pos = start + text.length;
      el.setSelectionRange(pos, pos);
    });
  }

  async function addFiles(files: File[]) {
    const images = files.filter((f) => /^image\/(jpeg|png|gif|webp)$/i.test(f.type) || /\.(jpe?g|png|gif|webp)$/i.test(f.name));
    if (images.length === 0) {
      setError("仅支持 JPEG、PNG、GIF、WebP 图片");
      return;
    }
    setError("");
    for (const f of images) {
      try {
        const up = await api.uploadNoteImage(target.path, f, pwd);
        const alt = f.name.replace(/\.[^.]+$/, "");
        insertAtCursor(`\n![${alt}](${up.url})\n`);
        setStatus("图片已插入");
      } catch (e: any) {
        setError(e?.message || "图片上传失败");
      }
    }
  }

  function onDrop(e: DragEvent) {
    e.preventDefault();
    setDragOver(false);
    void addFiles(Array.from(e.dataTransfer.files || []));
  }

  function onPaste(e: ClipboardEvent<HTMLTextAreaElement>) {
    const items = Array.from(e.clipboardData?.items || []);
    const files = items.filter((it) => it.kind === "file").map((it) => it.getAsFile()).filter((f): f is File => !!f);
    if (files.length) {
      e.preventDefault();
      void addFiles(files);
    }
  }

  const dirty = draft !== saved;

  return (
    <>
      <div className="note-overlay" onClick={onClose} />
      <aside className={"note-drawer" + (dragOver ? " dragging" : "")} role="dialog" aria-label="备注"
        onDragOver={(e) => { e.preventDefault(); setDragOver(true); }}
        onDragLeave={() => setDragOver(false)}
        onDrop={onDrop}>
        <header className="note-head">
          <div className="note-head-icon"><NoteIcon /></div>
          <div className="note-head-meta">
            <div className="note-title">{target.name || "备注"}</div>
            <div className="note-path">{target.isDir ? "文件夹" : "文件"} · {target.path}</div>
          </div>
          <button className="btn ghost sm note-close" onClick={onClose} aria-label="关闭">关闭</button>
        </header>

        {loading ? (
          <div className="note-empty">正在打开备注…</div>
        ) : error && !editing && !exists ? (
          <div className="note-empty">{error}</div>
        ) : editing ? (
          <div className="note-edit">
            <div className="note-toolbar">
              <button className="btn ghost sm" type="button" onClick={() => fileRef.current?.click()}>插入图片</button>
              <span className="note-hint">可粘贴或拖入图片，支持 Markdown</span>
              <input ref={fileRef} type="file" accept="image/jpeg,image/png,image/gif,image/webp" multiple hidden
                onChange={(e) => { void addFiles(Array.from(e.target.files || [])); e.target.value = ""; }} />
            </div>
            <textarea
              ref={area}
              className="note-editor"
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              onPaste={onPaste}
              placeholder="写下备注，比如这份资料的来源、用法，或贴上示意图…"
              spellCheck={false}
            />
            {dragOver && <div className="note-drop">放开即可加入图片</div>}
          </div>
        ) : !exists || !saved.trim() ? (
          <div className="note-empty">
            <NoteIcon size={32} />
            <p>还没有备注</p>
            <span>可以写很长的说明，也可以配上图片。</span>
            <button className="btn sm" type="button" onClick={() => setEditing(true)}>开始写</button>
          </div>
        ) : (
          <article className="note-body">
            {renderNote(saved, (src, alt) => (
              <AuthImage src={src} alt={alt} pwd={pwd} onOpen={(url, a) => setLightbox({ url, alt: a })} />
            ))}
          </article>
        )}

        {error && <div className="note-error">{error}</div>}

        <footer className="note-foot">
          <span className="note-status">
            {draft.length > 0 && <em>{draft.length} 字</em>}
            {status && <span>{status}</span>}
          </span>
          <div className="row">
            {editing ? (
              <>
                {exists && (
                  <button className="btn ghost sm" type="button" disabled={busy}
                    onClick={() => { if (confirm("清空这条备注？配图也会一并删除。")) void persist("", true); }}>
                    清空
                  </button>
                )}
                <button className="btn ghost sm" type="button" disabled={busy}
                  onClick={() => { setDraft(saved); setEditing(!!exists && !!saved); setError(""); setStatus(""); }}>
                  取消
                </button>
                <button className="btn sm" type="button" disabled={busy || (!dirty && !!saved.trim())}
                  onClick={() => void persist(draft, true)}>
                  完成
                </button>
              </>
            ) : (
              exists && <button className="btn sm" type="button" onClick={() => setEditing(true)}>编辑</button>
            )}
          </div>
        </footer>
      </aside>

      {lightbox && (
        <div className="note-lightbox" onClick={() => setLightbox(null)} role="presentation">
          <img src={lightbox.url} alt={lightbox.alt} />
        </div>
      )}
    </>
  );
}

export function NoteBadge({ onClick, title = "查看备注" }: { onClick: (e: ReactMouseEvent) => void; title?: string }) {
  return (
    <button type="button" className="note-badge" title={title} aria-label={title}
      onClick={(e) => { e.preventDefault(); e.stopPropagation(); onClick(e); }}>
      <NoteIcon size={13} />
      备注
    </button>
  );
}

/** Inline simple-text note, or the “备注” badge for richer / longer markdown. */
export function PathNoteHint({
  body, onClick, title = "查看备注",
}: {
  body: string;
  onClick: (e: ReactMouseEvent) => void;
  title?: string;
}) {
  if (isSimpleNoteText(body)) {
    return (
      <button type="button" className="note-inline" title={title} aria-label={title}
        onClick={(e) => { e.preventDefault(); e.stopPropagation(); onClick(e); }}>
        {renderSimpleNote(body)}
      </button>
    );
  }
  return <NoteBadge onClick={onClick} title={title} />;
}

/** @deprecated use NoteBadge */
export const NoteFlag = NoteBadge;

export function NotePreview({
  target, pwd, x, y, onClose, onEdit,
}: {
  target: NoteTarget;
  pwd?: string;
  x: number;
  y: number;
  onClose: () => void;
  onEdit: () => void;
}) {
  const box = useRef<HTMLDivElement>(null);
  const [pos, setPos] = useState({ x, y });
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [body, setBody] = useState("");
  const [lightbox, setLightbox] = useState<{ url: string; alt: string } | null>(null);

  useEffect(() => {
    let gone = false;
    setLoading(true);
    setError("");
    api.getNote(target.path, pwd).then((n) => {
      if (gone) return;
      setBody(n.body || "");
      setLoading(false);
    }).catch((e: any) => {
      if (gone) return;
      setError(e?.message || "无法打开备注");
      setLoading(false);
    });
    return () => { gone = true; };
  }, [target.path, pwd]);

  useLayoutEffect(() => {
    const el = box.current;
    if (!el) return;
    const r = el.getBoundingClientRect();
    let nx = x, ny = y + 8;
    if (nx + r.width > window.innerWidth - 10) nx = Math.max(10, window.innerWidth - 10 - r.width);
    if (ny + r.height > window.innerHeight - 10) ny = Math.max(10, y - 8 - r.height);
    if (nx !== pos.x || ny !== pos.y) setPos({ x: nx, y: ny });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [x, y, loading, body, error]);

  return (
    <>
      <div className="note-preview-overlay" onClick={onClose} />
      <div ref={box} className="note-preview" style={{ left: pos.x, top: pos.y }} role="dialog" aria-label="备注预览">
        <header className="note-preview-head">
          <div className="note-preview-title">{target.name}</div>
          <span className="note-preview-kind">{target.isDir ? "文件夹备注" : "文件备注"}</span>
        </header>
        {loading ? (
          <div className="note-preview-empty">正在载入…</div>
        ) : error ? (
          <div className="note-preview-empty">{error}</div>
        ) : !body.trim() ? (
          <div className="note-preview-empty">这条备注是空的</div>
        ) : (
          <article className="note-preview-body note-body">
            {renderNote(body, (src, alt) => (
              <AuthImage src={src} alt={alt} pwd={pwd} onOpen={(url, a) => setLightbox({ url, alt: a })} />
            ))}
          </article>
        )}
        <footer className="note-preview-foot">
          <button className="btn ghost sm" type="button" onClick={onClose}>关闭</button>
          <button className="btn sm" type="button" onClick={onEdit}>编辑</button>
        </footer>
      </div>
      {lightbox && (
        <div className="note-lightbox" onClick={() => setLightbox(null)} role="presentation">
          <img src={lightbox.url} alt={lightbox.alt} />
        </div>
      )}
    </>
  );
}
