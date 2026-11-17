import { useEffect, useRef, useState } from "react";
import { api, FileItem } from "../lib/api";
import { baseName, parentDir } from "../lib/searchLocate";

function highlightName(text: string, q: string) {
  const ql = q.trim();
  if (!ql) return text;
  const i = text.toLowerCase().indexOf(ql.toLowerCase());
  if (i < 0) return text;
  return <>{text.slice(0, i)}<mark className="search-mark">{text.slice(i, i + ql.length)}</mark>{text.slice(i + ql.length)}</>;
}

function FolderIcon() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" style={{ flexShrink: 0 }} aria-hidden="true">
      <path fill="#f5b849" d="M10 4H4a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2V8a2 2 0 0 0-2-2h-8l-2-2z" />
    </svg>
  );
}

function FileIcon() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" style={{ flexShrink: 0 }} aria-hidden="true">
      <path fill="#94a3b8" d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z" />
      <path fill="#cbd5e1" d="M14 2v6h6z" />
    </svg>
  );
}

export default function FileSearch({
  onLocate,
  search = (q: string) => api.searchFiles(q),
  debounceMs = 250,
}: {
  onLocate: (item: FileItem) => void;
  search?: (q: string) => Promise<FileItem[]>;
  debounceMs?: number;
}) {
  const [searchQ, setSearchQ] = useState("");
  const [searchHits, setSearchHits] = useState<FileItem[]>([]);
  const [searching, setSearching] = useState(false);
  const [searchOpen, setSearchOpen] = useState(false);
  const [searchIdx, setSearchIdx] = useState(0);
  const searchRef = useRef<HTMLInputElement>(null);
  const searchBoxRef = useRef<HTMLDivElement>(null);
  const searchFn = useRef(search);
  searchFn.current = search;
  const [error, setError] = useState("");

  useEffect(() => {
    const q = searchQ.trim();
    if (!q) {
      setSearchHits([]);
      setSearching(false);
      setError("");
      return;
    }
    setSearching(true);
    let cancelled = false;
    const t = setTimeout(() => {
      searchFn.current(q)
        .then((rows) => {
          if (cancelled) return;
          setSearchHits(rows);
          setSearchIdx(0);
          setSearchOpen(true);
          setError("");
        })
        .catch((e: any) => {
          if (cancelled) return;
          setError(e?.message || "搜索失败");
          setSearchHits([]);
        })
        .finally(() => {
          if (!cancelled) setSearching(false);
        });
    }, debounceMs);
    return () => {
      cancelled = true;
      clearTimeout(t);
    };
  }, [searchQ, debounceMs]);

  useEffect(() => {
    const onDown = (e: MouseEvent) => {
      if (searchBoxRef.current && !searchBoxRef.current.contains(e.target as Node)) {
        setSearchOpen(false);
      }
    };
    document.addEventListener("mousedown", onDown);
    return () => document.removeEventListener("mousedown", onDown);
  }, []);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const tag = (e.target as HTMLElement)?.tagName;
      const typing = tag === "INPUT" || tag === "TEXTAREA";
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        searchRef.current?.focus();
        setSearchOpen(!!searchQ.trim());
        return;
      }
      if (!typing && e.key === "/" && !e.metaKey && !e.ctrlKey && !e.altKey) {
        e.preventDefault();
        searchRef.current?.focus();
        setSearchOpen(!!searchQ.trim());
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [searchQ]);

  function locate(item: FileItem) {
    setSearchOpen(false);
    setSearchQ("");
    setSearchHits([]);
    onLocate(item);
  }

  return (
    <div className="file-search" ref={searchBoxRef}>
      <input
        ref={searchRef}
        data-testid="file-search-input"
        value={searchQ}
        onChange={(e) => { setSearchQ(e.target.value); setSearchOpen(true); }}
        onFocus={() => { if (searchQ.trim()) setSearchOpen(true); }}
        onKeyDown={(e) => {
          if (e.key === "Escape") {
            setSearchOpen(false);
            searchRef.current?.blur();
            return;
          }
          if (!searchOpen || searchHits.length === 0) return;
          if (e.key === "ArrowDown") {
            e.preventDefault();
            setSearchIdx((i) => (i + 1) % searchHits.length);
          } else if (e.key === "ArrowUp") {
            e.preventDefault();
            setSearchIdx((i) => (i - 1 + searchHits.length) % searchHits.length);
          } else if (e.key === "Enter") {
            e.preventDefault();
            const hit = searchHits[searchIdx];
            if (hit) locate(hit);
          }
        }}
        placeholder="搜索文件和目录"
        aria-label="全局搜索"
      />
      {searchOpen && searchQ.trim() && (
        <div className="file-search-menu" data-testid="file-search-menu" role="listbox">
          {searching && searchHits.length === 0 && <div className="file-search-empty">搜索中…</div>}
          {!searching && error && <div className="file-search-empty">{error}</div>}
          {!searching && !error && searchHits.length === 0 && <div className="file-search-empty">未找到匹配的文件或目录</div>}
          {searchHits.map((h, i) => {
            const name = baseName(h.path);
            const dir = parentDir(h.path);
            return (
              <div
                key={(h.is_dir ? "d:" : "f:") + h.path + ":" + h.id}
                data-testid="file-search-item"
                data-path={h.path}
                role="option"
                aria-selected={i === searchIdx}
                className={`file-search-item${i === searchIdx ? " active" : ""}`}
                onMouseEnter={() => setSearchIdx(i)}
                onMouseDown={(e) => { e.preventDefault(); locate(h); }}
              >
                {h.is_dir ? <FolderIcon /> : <FileIcon />}
                <div className="file-search-meta">
                  <div className="file-search-name">
                    {highlightName(name, searchQ)}
                    {h.protected && <span>🔒</span>}
                    {h.encrypted && <span title="加密存储">🛡️</span>}
                  </div>
                  <div className="file-search-path">{dir === "/" ? "根目录" : dir}</div>
                </div>
                <span className="file-search-kind">{h.is_dir ? (h.mount ? "挂载" : "文件夹") : "文件"}</span>
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}
