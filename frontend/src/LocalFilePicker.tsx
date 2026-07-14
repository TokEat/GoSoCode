import { useEffect, useState } from "react";
import { clientApi, LocalEntry } from "./lib/api";
import { humanSize } from "./lib/format";

interface Props {
  open: boolean;
  onClose: () => void;
  onSelect: (entry: LocalEntry) => void;
}

export default function LocalFilePicker({ open, onClose, onSelect }: Props) {
  const [path, setPath] = useState("");
  const [input, setInput] = useState("");
  const [parent, setParent] = useState("");
  const [entries, setEntries] = useState<LocalEntry[]>([]);
  const [drives, setDrives] = useState<LocalEntry[]>([]);
  const [selected, setSelected] = useState<LocalEntry | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");

  async function load(next = "") {
    setLoading(true);
    setError("");
    setSelected(null);
    try {
      const data = await clientApi.localFiles(next);
      setPath(data.path);
      setInput(data.path === "::drives" ? "此电脑" : data.path);
      setParent(data.parent);
      setEntries(data.entries);
      setDrives(data.drives || []);
    } catch (e: any) {
      setError(e.message || "无法读取此目录");
    } finally {
      setLoading(false);
    }
  }

  useEffect(() => { if (open) load(path); }, [open]);
  if (!open) return null;

  const goInput = () => load(input.trim() === "此电脑" ? "::drives" : input.trim());
  const activeDrive = (drive: LocalEntry) => {
    if (path === "::drives") return false;
    return path.toLowerCase().startsWith(drive.path.toLowerCase());
  };

  return (
    <div className="modal-overlay" onClick={onClose}>
      <div className="modal local-picker" onClick={(e) => e.stopPropagation()}>
        <div className="local-picker-head">
          <div>
            <div className="modal-h">选择本机文件</div>
            <div className="muted">文件内容由客户端读取，浏览器不会接收文件数据。</div>
          </div>
          <button className="picker-close" onClick={onClose}>×</button>
        </div>
        <div className="local-pathbar">
          <button className="btn ghost sm" disabled={!parent || loading} onClick={() => load(parent)}>← 上级</button>
          <input value={input} onChange={(e) => setInput(e.target.value)} onKeyDown={(e) => { if (e.key === "Enter") goInput(); }} />
          <button className="btn ghost sm" disabled={loading} onClick={goInput}>前往</button>
        </div>
        <div className="local-picker-workspace">
          <aside className="local-drive-rail">
            <div className="drive-rail-label">位置</div>
            <button className={`drive-shortcut${path === "::drives" ? " active" : ""}`} onClick={() => load("::drives")}>
              <span className="drive-glyph computer">▦</span><span>此电脑</span>
            </button>
            <div className="drive-rail-label disks">磁盘</div>
            {drives.map((drive) => (
              <button key={drive.path} className={`drive-shortcut${activeDrive(drive) ? " active" : ""}`} onClick={() => load(drive.path)}>
                <span className="drive-glyph">▰</span>
                <span className="drive-shortcut-name">{drive.name}</span>
              </button>
            ))}
          </aside>
          <div className="local-file-list">
            {entries.map((entry) => (
              <button
                key={entry.path}
                className={`local-file-row${selected?.path === entry.path ? " selected" : ""}`}
                onClick={() => entry.dir ? setSelected(null) : setSelected(entry)}
                onDoubleClick={() => entry.dir ? load(entry.path) : onSelect(entry)}
              >
                <span className="local-file-icon">{path === "::drives" ? "▰" : entry.dir ? "▰" : "▤"}</span>
                <span className="local-file-name">{entry.name}</span>
                <span className="local-file-size">{path === "::drives" ? "磁盘" : entry.dir ? "文件夹" : humanSize(entry.size)}</span>
              </button>
            ))}
            {loading && <div className="picker-state">正在读取目录…</div>}
            {!loading && error && <div className="picker-state error">{error}</div>}
            {!loading && !error && entries.length === 0 && <div className="picker-state">此目录为空</div>}
          </div>
        </div>
        <div className="local-picker-foot">
          <span className="muted">{selected ? `已选择：${selected.name}` : `当前位置：${path === "::drives" ? "此电脑" : path}`}</span>
          <div className="row">
            <button className="btn ghost" onClick={onClose}>取消</button>
            <button className="btn" disabled={!selected} onClick={() => selected && onSelect(selected)}>添加上传任务</button>
          </div>
        </div>
      </div>
    </div>
  );
}
