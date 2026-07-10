import { createContext, useCallback, useContext, useState, ReactNode } from "react";

// ---- Modal dialogs (modern confirm / prompt) -------------------------------

type ConfirmOpts = { title: string; message?: string; confirmLabel?: string; danger?: boolean };
type PromptOpts = { title: string; message?: string; defaultValue?: string; placeholder?: string; confirmLabel?: string };
interface DialogApi {
  confirm: (o: ConfirmOpts) => Promise<boolean>;
  prompt: (o: PromptOpts) => Promise<string | null>;
}
const DialogCtx = createContext<DialogApi>({ confirm: async () => false, prompt: async () => null });
export const useDialog = () => useContext(DialogCtx);

type DialogState =
  | (ConfirmOpts & { kind: "confirm"; resolve: (v: boolean) => void })
  | (PromptOpts & { kind: "prompt"; resolve: (v: string | null) => void });

export function DialogProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<DialogState | null>(null);
  const [val, setVal] = useState("");

  const confirm = useCallback((o: ConfirmOpts) =>
    new Promise<boolean>((resolve) => setState({ kind: "confirm", ...o, resolve })), []);
  const prompt = useCallback((o: PromptOpts) =>
    new Promise<string | null>((resolve) => { setVal(o.defaultValue ?? ""); setState({ kind: "prompt", ...o, resolve }); }), []);

  function finish(result: boolean | string | null) {
    if (!state) return;
    if (state.kind === "confirm") state.resolve(result as boolean);
    else state.resolve(result as string | null);
    setState(null);
  }
  const cancel = () => finish(state?.kind === "confirm" ? false : null);
  const accept = () => finish(state?.kind === "confirm" ? true : val);

  return (
    <DialogCtx.Provider value={{ confirm, prompt }}>
      {children}
      {state && (
        <div className="modal-overlay" onClick={cancel}>
          <div className="modal" onClick={(e) => e.stopPropagation()}>
            <div className="modal-h">{state.title}</div>
            {state.message && <div className="modal-msg">{state.message}</div>}
            {state.kind === "prompt" && (
              <input
                autoFocus
                value={val}
                placeholder={state.placeholder}
                onChange={(e) => setVal(e.target.value)}
                onKeyDown={(e) => { if (e.key === "Enter") accept(); if (e.key === "Escape") cancel(); }}
              />
            )}
            <div className="modal-actions">
              <button className="btn ghost" onClick={cancel}>取消</button>
              <button className={`btn${(state.kind === "confirm" && state.danger) ? " danger" : ""}`} onClick={accept}>
                {state.confirmLabel || "确定"}
              </button>
            </div>
          </div>
        </div>
      )}
    </DialogCtx.Provider>
  );
}

// ---- Toasts ----------------------------------------------------------------

type Toast = { id: number; kind: "ok" | "err" | "info"; msg: string };
const ToastCtx = createContext<(kind: Toast["kind"], msg: string) => void>(() => {});
export const useToast = () => useContext(ToastCtx);

export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<Toast[]>([]);
  const push = useCallback((kind: Toast["kind"], msg: string) => {
    const id = Date.now() + Math.random();
    setItems((p) => [...p, { id, kind, msg }]);
    setTimeout(() => setItems((p) => p.filter((t) => t.id !== id)), 3500);
  }, []);
  return (
    <ToastCtx.Provider value={push}>
      {children}
      <div className="toasts">
        {items.map((t) => <div key={t.id} className={`toast ${t.kind}`}>{t.msg}</div>)}
      </div>
    </ToastCtx.Provider>
  );
}

// ---- small presentational helpers -----------------------------------------

export function Badge({ kind, children }: { kind: string; children: ReactNode }) {
  return <span className={`badge ${kind}`}>{children}</span>;
}

// statusBadge translates raw backend status strings into user-facing labels,
// so the UI never leaks developer vocabulary like "uploading"/"banned".
export function statusBadge(status: string) {
  const map: Record<string, { kind: string; label: string }> = {
    active: { kind: "green", label: "正常" },
    ready: { kind: "green", label: "就绪" },
    done: { kind: "green", label: "完成" },
    disabled: { kind: "gray", label: "已停用" },
    failed: { kind: "red", label: "失败" },
    banned: { kind: "red", label: "已封禁" },
    uploading: { kind: "amber", label: "上传中" },
    running: { kind: "amber", label: "进行中" },
    pending: { kind: "blue", label: "等待中" },
    deleted: { kind: "gray", label: "已删除" },
  };
  const s = map[status];
  return <Badge kind={s?.kind || "gray"}>{s?.label || status}</Badge>;
}

export function Card({ title, action, children }: { title?: string; action?: ReactNode; children: ReactNode }) {
  return (
    <div className="card">
      {title && <div className="card-h"><h3>{title}</h3>{action}</div>}
      <div className="card-b">{children}</div>
    </div>
  );
}

export function Stat({ label, value, sub }: { label: string; value: ReactNode; sub?: string }) {
  return (
    <div className="card stat">
      <div className="label">{label}</div>
      <div className="value">{value}</div>
      {sub && <div className="sub">{sub}</div>}
    </div>
  );
}
