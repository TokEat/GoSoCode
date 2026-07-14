import { createContext, useCallback, useContext, useEffect, useRef, useState, ReactNode } from "react";
import SparkMD5 from "spark-md5";
import { auth, clientApi, ClientTask } from "./lib/api";

const CHUNK = 4 * 1024 * 1024; // 4MB browser->server chunk (legacy store-and-forward)
const MAX_RETRY = 5;           // consecutive-stall retries before giving up

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
const backoff = (n: number) => Math.min(1000 * 2 ** n, 8000) + Math.floor(Math.random() * 400);
function describeErr(e: unknown): string {
  const m = (e as Error)?.message || String(e);
  if (/failed to fetch|networkerror|load failed|err_/i.test(m)) return "网络中断或丢包，连接被重置";
  return m;
}

export type TransferStatus = "running" | "paused" | "finalizing" | "done" | "error" | "canceled";

export interface Transfer {
  id: string;
  kind: "upload" | "download";
  name: string;
  path: string;
  total: number;
  transferred: number;
  speed: number; // bytes/sec (browser leg)
  synced?: number;    // bytes confirmed cloud-synced (streaming uploads)
  syncSpeed?: number; // cloud-sync bytes/sec (streaming uploads)
  eta?: number;       // estimated seconds remaining (undefined when unknown)
  status: TransferStatus;
  note?: string;  // transient message, e.g. "retrying (2/5)…"
  error?: string; // terminal failure reason
}

interface StreamCtl {
  sessionId: string;
  sliceSize: number;
  total: number;
  window: number;
  pending: number[];              // slice seqs not yet cloud-synced
  doneBytes: number;              // browser bytes of confirmed slices
  inflight: Map<number, number>;  // seq -> xhr upload.loaded
  xhrs: Set<XMLHttpRequest>;
}

interface Ctl {
  kind: "upload" | "download";
  name: string;
  path: string;
  total: number;
  sent: number;
  shown: number; // monotonic display value (progress bar never jumps backwards)
  synced: number;
  paused: boolean;
  canceled: boolean;
  file?: File;
  uploadId?: string;
  fileId?: number;
  downloadUrl?: string;
  abort?: AbortController;
  chunks: Uint8Array[];
  jobId?: string;
  stream?: StreamCtl;
  dlSeq: number;   // next IndexedDB chunk sequence for this download
  idbOff?: boolean; // IndexedDB unavailable: keep download buffer in memory only
  retrying: boolean;
  lastTime: number;
  lastBytes: number;
  lastSynced: number;
  emaSpeed?: number; // smoothed rates for a stable ETA
  emaSync?: number;
}

interface TransferApi {
  clientMode: boolean;
  transfers: Transfer[];
  startUpload: (file: File, path: string) => void;
  startClientUpload: (localPath: string, remoteDir: string) => Promise<void>;
  startDownload: (item: { id: number; path: string; size: number; download_url?: string }) => void | Promise<void>;
  pause: (id: string) => void;
  resume: (id: string) => void;
  cancel: (id: string) => void;
  remove: (id: string) => void;
  clearFinished: () => void;
  resolveJob: (jobId: string, status: string, error?: string) => void;
}

const Ctx = createContext<TransferApi>(null as unknown as TransferApi);
export const useTransfers = () => useContext(Ctx);

export function TransferProvider({ children, mode = "server" }: { children: ReactNode; mode?: "client" | "server" }) {
  return mode === "client"
    ? <ClientTransferProvider>{children}</ClientTransferProvider>
    : <BrowserTransferProvider>{children}</BrowserTransferProvider>;
}

const uid = () => Math.random().toString(36).slice(2) + Date.now().toString(36);
const authHeader = () => ({ Authorization: `Bearer ${auth.token}` });

async function postJSON(url: string, body: unknown) {
  const res = await fetch(url, {
    method: "POST",
    headers: { ...authHeader(), "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    const err = new Error(data.error || res.statusText) as Error & { status?: number; data?: Record<string, unknown> };
    err.status = res.status; err.data = data;
    throw err;
  }
  return data;
}

async function getJSON(url: string) {
  const res = await fetch(url, { headers: authHeader() });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    const err = new Error(data.error || res.statusText) as Error & { status?: number };
    err.status = res.status;
    throw err;
  }
  return data;
}

async function getUploadOffset(uploadId: string): Promise<number> {
  return (await getJSON(`/api/files/upload/status?upload_id=${uploadId}`)).offset as number;
}

function saveBlob(blob: Blob, name: string) {
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url; a.download = name; a.click();
  URL.revokeObjectURL(url);
}

// pickFile opens the native file dialog (used to re-attach the original file
// when resuming an upload after a page refresh).
function pickFile(): Promise<File | null> {
  return new Promise((resolve) => {
    const input = document.createElement("input");
    input.type = "file";
    let done = false;
    const finish = () => { if (!done) { done = true; resolve(input.files?.[0] || null); } };
    input.onchange = finish;
    // no reliable cancel event: fall back to "window refocused without a pick"
    window.addEventListener("focus", () => setTimeout(finish, 800), { once: true });
    input.click();
  });
}

// ---- persistence: survive page refresh -------------------------------------
// Transfer metadata is mirrored to localStorage; the picked upload File and the
// received download bytes are persisted in IndexedDB (browsers can store
// File/Blob objects). Together with the server-side sessions this keeps the
// frontend and backend in sync across refreshes/interruptions: uploads resume
// without re-picking the file, downloads resume from the stored byte offset.

const STORE_KEY = "gsc_transfers_v1";

// ---- IndexedDB helpers ------------------------------------------------------

let idbPromise: Promise<IDBDatabase> | null = null;
function openIDB(): Promise<IDBDatabase> {
  if (!idbPromise) {
    idbPromise = new Promise((resolve, reject) => {
      const req = indexedDB.open("gsc-transfers", 1);
      req.onupgradeneeded = () => {
        const db = req.result;
        if (!db.objectStoreNames.contains("files")) db.createObjectStore("files");
        if (!db.objectStoreNames.contains("chunks")) db.createObjectStore("chunks");
      };
      req.onsuccess = () => resolve(req.result);
      req.onerror = () => reject(req.error);
    });
  }
  return idbPromise;
}
function idbTx<T>(store: string, mode: IDBTransactionMode, fn: (s: IDBObjectStore) => IDBRequest<T>): Promise<T> {
  return openIDB().then((db) => new Promise<T>((resolve, reject) => {
    const req = fn(db.transaction(store, mode).objectStore(store));
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error);
  }));
}
const fileSave = (id: string, f: File) => idbTx("files", "readwrite", (s) => s.put(f, id));
const fileLoad = (id: string) => idbTx<File | undefined>("files", "readonly", (s) => s.get(id) as IDBRequest<File | undefined>);
const fileDrop = (id: string) => idbTx("files", "readwrite", (s) => s.delete(id)).catch(() => {});
const chunkKey = (id: string, seq: number) => `${id}:${String(seq).padStart(8, "0")}`;
const chunkRange = (id: string) => IDBKeyRange.bound(`${id}:`, `${id}:￿`);
const chunkSave = (id: string, seq: number, b: Blob) => idbTx("chunks", "readwrite", (s) => s.put(b, chunkKey(id, seq)));
const chunksLoad = (id: string) => idbTx<Blob[]>("chunks", "readonly", (s) => s.getAll(chunkRange(id)));
const chunksDrop = (id: string) => idbTx("chunks", "readwrite", (s) => s.delete(chunkRange(id))).catch(() => {});
async function chunksInfo(id: string): Promise<{ count: number; bytes: number }> {
  const parts = await chunksLoad(id);
  return { count: parts.length, bytes: parts.reduce((n, b) => n + b.size, 0) };
}
const dropStored = (id: string) => { fileDrop(id); chunksDrop(id); };

interface PersistEntry {
  id: string; kind: "upload" | "download"; name: string; path: string;
  total: number; transferred: number; synced?: number;
  status: TransferStatus; error?: string;
  uploadId?: string; fileId?: number; downloadUrl?: string;
  stream?: { sessionId: string; sliceSize: number; total: number; window: number };
}

function restoreTransfers(): { list: Transfer[]; ctls: Map<string, Ctl>; resumeIds: string[] } {
  const ctls = new Map<string, Ctl>();
  const list: Transfer[] = [];
  const resumeIds: string[] = [];
  try {
    const raw = JSON.parse(localStorage.getItem(STORE_KEY) || "[]") as PersistEntry[];
    for (const e of raw) {
      if (!e || !e.id) continue;
      const wasActive = e.status === "running" || e.status === "paused" || e.status === "finalizing";
      const status: TransferStatus = wasActive ? "paused" : e.status;
      // transfers that were mid-flight get auto-resumed once their local data
      // (upload File / download buffer) is reattached from IndexedDB
      if (e.status === "running" || e.status === "finalizing") resumeIds.push(e.id);
      const transferred = e.transferred || 0;
      const synced = e.stream ? Math.min(e.synced || 0, e.total) : undefined;
      list.push({
        id: e.id, kind: e.kind, name: e.name, path: e.path, total: e.total,
        transferred, synced, speed: 0, status, error: e.error,
      });
      ctls.set(e.id, {
        kind: e.kind, name: e.name, path: e.path, total: e.total,
        sent: transferred, shown: transferred, synced: synced || 0,
        paused: true, canceled: e.status === "canceled",
        uploadId: e.uploadId, fileId: e.fileId, downloadUrl: e.downloadUrl,
        chunks: [], dlSeq: 0, retrying: false,
        lastTime: Date.now(), lastBytes: transferred, lastSynced: synced || 0,
        stream: e.stream ? {
          sessionId: e.stream.sessionId, sliceSize: e.stream.sliceSize,
          total: e.stream.total, window: e.stream.window,
          pending: [], doneBytes: synced || 0, inflight: new Map(), xhrs: new Set(),
        } : undefined,
      });
    }
  } catch { /* corrupt store: start clean */ }
  return { list, ctls, resumeIds };
}

// putSliceXHR PUTs one raw slice via XMLHttpRequest so the browser->server leg
// is observable (upload.onprogress), while the 200 response means the server
// finished pushing the slice to Baidu — that's the cloud-sync confirmation.
function putSliceXHR(
  url: string, body: ArrayBuffer,
  onProgress: (loaded: number) => void,
  xhrs: Set<XMLHttpRequest>,
): Promise<{ done_count: number; total: number }> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open("PUT", url);
    xhr.setRequestHeader("Authorization", `Bearer ${auth.token}`);
    xhr.upload.onprogress = (e) => onProgress(e.loaded);
    xhr.onload = () => {
      xhrs.delete(xhr);
      let data: Record<string, unknown> = {};
      try { data = JSON.parse(xhr.responseText || "{}"); } catch { /* not json */ }
      if (xhr.status >= 200 && xhr.status < 300) resolve(data as { done_count: number; total: number });
      else {
        const err = new Error((data.error as string) || `HTTP ${xhr.status}`) as Error & { status?: number; data?: Record<string, unknown> };
        err.status = xhr.status;
        err.data = data;
        reject(err);
      }
    };
    xhr.onerror = () => { xhrs.delete(xhr); reject(new Error("网络中断或丢包，连接被重置")); };
    xhr.onabort = () => {
      xhrs.delete(xhr);
      const e = new Error("aborted") as Error & { aborted?: boolean };
      e.aborted = true;
      reject(e);
    };
    xhrs.add(xhr);
    xhr.send(body);
  });
}

// SLICE_PART is the browser->server request size for big slices. VIP/SVIP
// nodes use 16/32MB Baidu slices; a single request that large rarely survives
// a flaky link, so putSlice ships it as sequential 4MB sub-chunks (?off=) that
// the server assembles. After any error the server replies 409 with the
// acknowledged offset, so a retry resumes mid-slice instead of starting over.
const SLICE_PART = 4 * 1024 * 1024;

async function putSlice(
  url: string, body: ArrayBuffer,
  onProgress: (loaded: number) => void,
  xhrs: Set<XMLHttpRequest>,
): Promise<{ done_count: number; total: number }> {
  if (body.byteLength <= SLICE_PART) return putSliceXHR(url, body, onProgress, xhrs);
  let off = 0;
  for (;;) {
    const end = Math.min(off + SLICE_PART, body.byteLength);
    try {
      const res = await putSliceXHR(
        `${url}&off=${off}`, body.slice(off, end),
        (loaded) => onProgress(off + loaded), xhrs,
      );
      if (end >= body.byteLength) return res;
      off = end;
    } catch (e) {
      const err = e as Error & { status?: number; data?: { expected?: number } };
      if (err.status === 409 && typeof err.data?.expected === "number" && err.data.expected !== off) {
        off = err.data.expected; // re-sync with what the server already has
        continue;
      }
      throw e;
    }
  }
}

// computeBlockList md5-hashes the file slice by slice (local read only, no
// network) — required by Baidu precreate and what unlocks rapid-upload (秒传).
async function computeBlockList(
  file: File, sliceSize: number,
  onProgress: (done: number, total: number) => void,
  isCanceled: () => boolean,
): Promise<string[] | null> {
  const total = Math.max(1, Math.ceil(file.size / sliceSize));
  const out: string[] = [];
  for (let i = 0; i < total; i++) {
    if (isCanceled()) return null;
    const buf = await file.slice(i * sliceSize, Math.min((i + 1) * sliceSize, file.size)).arrayBuffer();
    out.push(SparkMD5.ArrayBuffer.hash(buf));
    onProgress(i + 1, total);
  }
  return out;
}

function BrowserTransferProvider({ children }: { children: ReactNode }) {
  // restore persisted transfers exactly once, before first render
  const restored = useRef<{ list: Transfer[]; ctls: Map<string, Ctl>; resumeIds: string[] } | null>(null);
  if (restored.current === null) restored.current = restoreTransfers();
  const [transfers, setTransfers] = useState<Transfer[]>(restored.current.list);
  const ctls = useRef(restored.current.ctls);

  // mirror the list to localStorage so a refresh doesn't lose it
  useEffect(() => {
    const entries: PersistEntry[] = transfers.slice(0, 30).map((t) => {
      const ctl = ctls.current.get(t.id);
      return {
        id: t.id, kind: t.kind, name: t.name, path: t.path, total: t.total,
        transferred: t.transferred, synced: t.synced, status: t.status, error: t.error,
        uploadId: ctl?.uploadId, fileId: ctl?.fileId, downloadUrl: ctl?.downloadUrl,
        stream: ctl?.stream ? {
          sessionId: ctl.stream.sessionId, sliceSize: ctl.stream.sliceSize,
          total: ctl.stream.total, window: ctl.stream.window,
        } : undefined,
      };
    });
    try { localStorage.setItem(STORE_KEY, JSON.stringify(entries)); } catch { /* quota */ }
  }, [transfers]);

  const patch = useCallback((id: string, p: Partial<Transfer>) => {
    setTransfers((list) => list.map((t) => (t.id === id ? { ...t, ...p } : t)));
  }, []);

  // tick pushes progress to the UI, throttled to ~3 updates/sec, and with a
  // monotonic display value so retries/aborts never make the bar jump back.
  const tick = useCallback((id: string) => {
    const ctl = ctls.current.get(id);
    if (!ctl) return;
    const now = Date.now();
    const dt = (now - ctl.lastTime) / 1000;
    if (dt < 0.35) return;
    const speed = Math.max(0, (ctl.sent - ctl.lastBytes) / dt);
    ctl.emaSpeed = ctl.emaSpeed ? ctl.emaSpeed * 0.7 + speed * 0.3 : speed;
    ctl.shown = Math.max(ctl.shown, Math.min(ctl.sent, ctl.total || ctl.sent));
    const p: Partial<Transfer> = { transferred: ctl.shown, speed };
    // ETA from the smoothed rate; for streaming uploads completion is gated by
    // the cloud-sync leg, so estimate from that rate instead.
    let rate = ctl.emaSpeed;
    let remaining = ctl.total - ctl.sent;
    if (ctl.stream) {
      const syncSpeed = Math.max(0, (ctl.synced - ctl.lastSynced) / dt);
      ctl.emaSync = ctl.emaSync ? ctl.emaSync * 0.7 + syncSpeed * 0.3 : syncSpeed;
      p.synced = ctl.synced;
      p.syncSpeed = syncSpeed;
      rate = ctl.emaSync;
      remaining = ctl.total - ctl.synced;
    }
    p.eta = ctl.total > 0 && rate > 1024 && remaining > 0 ? remaining / rate : undefined;
    ctl.lastTime = now; ctl.lastBytes = ctl.sent; ctl.lastSynced = ctl.synced;
    patch(id, p);
  }, [patch]);

  // ---- legacy upload (store-and-forward: temp file + async Baidu job) -------
  const driveUpload = useCallback(async (id: string) => {
    const ctl = ctls.current.get(id);
    if (!ctl || !ctl.file) return;
    const size = ctl.file.size;
    try {
      if (!ctl.uploadId) {
        const r = await postJSON("/api/files/upload/init", { name: ctl.file.name, path: ctl.path, size });
        ctl.uploadId = r.upload_id;
      }
      ctl.lastTime = Date.now(); ctl.lastBytes = ctl.sent;
      while (ctl.sent < size) {
        if (ctl.canceled) return;
        if (ctl.paused) { patch(id, { status: "paused", speed: 0 }); return; }
        const end = Math.min(ctl.sent + CHUNK, size);
        const buf = await ctl.file.slice(ctl.sent, end).arrayBuffer();

        // Send this chunk, retrying transient failures. Idempotent by offset.
        let attempt = 0;
        for (;;) {
          try {
            const res = await fetch(`/api/files/upload/chunk?upload_id=${ctl.uploadId}&offset=${ctl.sent}`, {
              method: "PUT", headers: authHeader(), body: buf,
            });
            if (!res.ok) throw new Error((await res.json().catch(() => ({}))).error || `HTTP ${res.status}`);
            ctl.sent = (await res.json()).offset;
            if (ctl.retrying) { ctl.retrying = false; patch(id, { note: undefined }); }
            break;
          } catch (e) {
            if (ctl.paused || ctl.canceled) return;
            // The chunk may have landed before the connection dropped: re-sync.
            try {
              const off = await getUploadOffset(ctl.uploadId!);
              if (off > ctl.sent) { ctl.sent = off; if (ctl.retrying) { ctl.retrying = false; patch(id, { note: undefined }); } break; }
            } catch { /* ignore, will retry */ }
            attempt++;
            if (attempt >= MAX_RETRY) throw new Error(`分片上传失败（已重试 ${MAX_RETRY} 次）：${describeErr(e)}`);
            ctl.retrying = true;
            patch(id, { note: `网络波动，正在重试 (${attempt}/${MAX_RETRY})…`, speed: 0 });
            await sleep(backoff(attempt - 1));
          }
        }
        tick(id);
      }
      patch(id, { status: "finalizing", speed: 0, transferred: ctl.total, note: undefined });
      const fin = await postJSON("/api/files/upload/finish", { upload_id: ctl.uploadId });
      ctl.jobId = fin.job_id; // flips to done via resolveJob when Baidu leg completes
      fileDrop(id); // server owns the bytes now
    } catch (e: unknown) {
      if (ctl.canceled || ctl.paused) return;
      if (ctl.uploadId) {
        // the server-side session is still valid: pause instead of failing so
        // resume continues from the server offset (no restart, no re-pick)
        ctl.paused = true;
        patch(id, { status: "paused", note: "网络持续异常，已暂停，点击「恢复」继续", speed: 0 });
      } else {
        ctl.file = undefined;
        dropStored(id);
        patch(id, { status: "error", error: describeErr(e), speed: 0, note: undefined });
      }
    }
  }, [patch, tick]);

  // ---- streaming upload (pipelined: slices sync to Baidu as they arrive) ----

  const recomputeSent = (ctl: Ctl) => {
    const st = ctl.stream!;
    let inflight = 0;
    st.inflight.forEach((v) => { inflight += v; });
    ctl.sent = Math.min(ctl.total, st.doneBytes + inflight);
  };

  const sliceLen = (st: StreamCtl, size: number, seq: number) =>
    seq === st.total - 1 ? size - (st.total - 1) * st.sliceSize : st.sliceSize;

  // streamFinish asks the server to merge (create). It stays in "running"
  // (never shows a separate finalizing state) and only reports 100%/done once
  // the cloud confirms — no premature progress spikes.
  const streamFinish = useCallback(async (id: string) => {
    const ctl = ctls.current.get(id);
    if (!ctl || !ctl.stream) return;
    let attempt = 0;
    for (;;) {
      try {
        await postJSON("/api/files/stream/finish", { session_id: ctl.stream.sessionId });
        ctl.sent = ctl.total; ctl.shown = ctl.total; ctl.synced = ctl.total;
        ctl.file = undefined;
        fileDrop(id);
        patch(id, { status: "done", transferred: ctl.total, synced: ctl.total, speed: 0, syncSpeed: 0, note: undefined });
        return;
      } catch (e) {
        const err = e as Error & { status?: number; data?: { need_seqs?: number[] } };
        if (err.status === 409 && err.data?.need_seqs?.length) {
          // a slice confirmation raced/got lost — re-send the missing ones,
          // and re-derive byte counters so they are not double-counted
          const st = ctl.stream;
          st.pending = err.data.need_seqs;
          const missing = st.pending.reduce((sum, seq) => sum + sliceLen(st, ctl.total, seq), 0);
          st.doneBytes = ctl.total - missing;
          ctl.synced = st.doneBytes;
          recomputeSent(ctl);
          throw Object.assign(new Error("need-more-slices"), { needMore: true });
        }
        attempt++;
        if (attempt >= MAX_RETRY) throw new Error(`云端保存失败（已重试 ${MAX_RETRY} 次）：${describeErr(e)}`);
        patch(id, { note: `正在完成云端保存，重试 (${attempt}/${MAX_RETRY})…` });
        await sleep(backoff(attempt - 1));
      }
    }
  }, [patch]);

  // streamLoop drives the sliding window: up to `window` slices are in flight
  // at once, so the browser uploads slice N+1 while the server pushes slice N
  // to Baidu (the two legs overlap).
  const streamLoop = useCallback(async (id: string) => {
    const ctl = ctls.current.get(id);
    if (!ctl || !ctl.file || !ctl.stream) return;
    const st = ctl.stream;
    const file = ctl.file;
    ctl.lastTime = Date.now(); ctl.lastBytes = ctl.sent; ctl.lastSynced = ctl.synced;

    for (;;) {
      let fatal: Error | null = null;
      const workers: Promise<void>[] = [];
      const n = Math.max(1, Math.min(st.window, st.pending.length));
      for (let w = 0; w < n; w++) {
        workers.push((async () => {
          for (;;) {
            if (fatal || ctl.canceled || ctl.paused) return;
            const seq = st.pending.shift();
            if (seq === undefined) return;
            const start = seq * st.sliceSize;
            const buf = await file.slice(start, start + sliceLen(st, file.size, seq)).arrayBuffer();
            let attempt = 0;
            for (;;) {
              try {
                await putSlice(
                  `/api/files/stream/slice?session=${st.sessionId}&seq=${seq}`, buf,
                  (loaded) => { st.inflight.set(seq, Math.min(loaded, buf.byteLength)); recomputeSent(ctl); tick(id); },
                  st.xhrs,
                );
                st.inflight.delete(seq);
                st.doneBytes += buf.byteLength;
                ctl.synced += buf.byteLength;
                recomputeSent(ctl);
                if (ctl.retrying) { ctl.retrying = false; patch(id, { note: undefined }); }
                tick(id);
                break; // slice done: leave the retry loop, take the next seq
              } catch (e) {
                st.inflight.delete(seq);
                recomputeSent(ctl);
                const err = e as Error & { status?: number; aborted?: boolean };
                if (err.aborted || ctl.paused || ctl.canceled) return; // pause/cancel aborts are expected
                if (err.status === 404) {
                  fatal = Object.assign(new Error("上传会话已失效，请重新上传"), { sessionLost: true });
                  return;
                }
                attempt++;
                if (attempt >= MAX_RETRY) { fatal = new Error(`上传失败（已重试 ${MAX_RETRY} 次）：${describeErr(e)}`); return; }
                ctl.retrying = true;
                patch(id, { note: `网络波动，正在重试 (${attempt}/${MAX_RETRY})…`, speed: 0 });
                await sleep(backoff(attempt - 1));
              }
            }
          }
        })().catch((e) => { fatal = fatal || (e as Error); }));
      }
      await Promise.all(workers);

      if (ctl.canceled) return;
      if (ctl.paused) { patch(id, { status: "paused", speed: 0, syncSpeed: 0 }); return; }
      if (fatal) {
        if ((fatal as { sessionLost?: boolean }).sessionLost) {
          // the server no longer knows this session: nothing to resume
          ctl.file = undefined;
          dropStored(id);
          patch(id, { status: "error", error: describeErr(fatal), speed: 0, syncSpeed: 0, note: undefined });
        } else {
          // transient exhaustion: keep session + file, resume continues where it stopped
          ctl.paused = true;
          patch(id, { status: "paused", speed: 0, syncSpeed: 0, note: "网络持续异常，已暂停，点击「恢复」继续" });
        }
        return;
      }
      try {
        await streamFinish(id);
        return;
      } catch (e) {
        if ((e as { needMore?: boolean }).needMore) continue; // missing slices were re-queued
        // finish kept failing: the session is still resumable — pause, don't discard
        ctl.paused = true;
        patch(id, { status: "paused", speed: 0, syncSpeed: 0, note: "云端保存未完成，已暂停，点击「恢复」重试" });
        return;
      }
    }
  }, [patch, tick, streamFinish]);

  // driveStreamUpload runs plan -> local md5 pre-scan -> init (rapid-upload
  // short-circuit) -> sliding-window slices -> finish. Returns "fallback" when
  // the server asks for store-and-forward instead.
  const driveStreamUpload = useCallback(async (id: string): Promise<"ok" | "fallback"> => {
    const ctl = ctls.current.get(id);
    if (!ctl || !ctl.file) return "ok";
    const file = ctl.file;
    // plan failing (older backend, transient error) is not fatal: fall back
    let plan: { enabled?: boolean; slice_size?: number; window?: number };
    try {
      plan = await getJSON(`/api/files/stream/plan?size=${file.size}&path=${encodeURIComponent(ctl.path)}`);
    } catch {
      return "fallback";
    }
    if (!plan.enabled || !plan.slice_size) return "fallback";
    try {
      let sliceSize: number = plan.slice_size;
      const win: number = plan.window || 3;

      // local md5 pre-scan (no network); also what makes 秒传 possible.
      // Intentionally silent: no note/progress during initialization.
      let blockList = await computeBlockList(file, sliceSize, () => {}, () => ctl.canceled);
      if (!blockList) return "ok"; // canceled during hashing

      let init: Record<string, unknown>;
      for (;;) {
        try {
          init = await postJSON("/api/files/stream/init", {
            name: file.name, path: ctl.path, size: file.size, slice_size: sliceSize, block_list: blockList,
          });
          break;
        } catch (e) {
          const err = e as Error & { status?: number; data?: { fallback?: boolean; slice_size?: number } };
          if (err.data?.fallback) return "fallback";
          // node changed between plan and init and implies another slice size: re-hash once
          if (err.status === 409 && err.data?.slice_size && err.data.slice_size !== sliceSize) {
            sliceSize = err.data.slice_size;
            blockList = await computeBlockList(file, sliceSize, () => {}, () => ctl.canceled);
            if (!blockList) return "ok";
            continue;
          }
          throw e;
        }
      }
      patch(id, { note: undefined });

      const total = Math.max(1, Math.ceil(file.size / sliceSize));
      ctl.stream = {
        sessionId: init.session_id as string,
        sliceSize, total, window: win,
        pending: (init.need_seqs as number[]) || [],
        doneBytes: 0, inflight: new Map(), xhrs: new Set(),
      };
      patch(id, { synced: 0, syncSpeed: 0 });

      if (init.rapid) { // 秒传：文件已在云端，零分片上传
        ctl.stream.doneBytes = file.size;
        ctl.sent = file.size; ctl.synced = file.size;
        patch(id, { note: "云端已有相同文件，已秒传", transferred: file.size, synced: file.size });
        await streamFinish(id);
        return "ok";
      }

      // resumed subset (precreate said some slices already exist)
      const missingBytes = ctl.stream.pending.reduce((s, seq) => s + sliceLen(ctl.stream!, file.size, seq), 0);
      ctl.stream.doneBytes = file.size - missingBytes;
      ctl.synced = ctl.stream.doneBytes;
      ctl.sent = ctl.stream.doneBytes;

      await streamLoop(id);
      return "ok";
    } catch (e) {
      if (!ctl.canceled && !ctl.paused) {
        if (ctl.stream) {
          // session exists: recoverable — pause so resume picks it back up
          ctl.paused = true;
          patch(id, { status: "paused", speed: 0, syncSpeed: 0, note: "已暂停，点击「恢复」继续" });
        } else {
          ctl.file = undefined;
          dropStored(id);
          patch(id, { status: "error", error: describeErr(e), speed: 0, syncSpeed: 0, note: undefined });
        }
      }
      return "ok";
    }
  }, [patch, streamLoop, streamFinish]);

  // resumeStream re-syncs the missing-slice set from the server (superfile2 is
  // idempotent, so anything aborted mid-flight is simply re-sent).
  const resumeStream = useCallback(async (id: string) => {
    const ctl = ctls.current.get(id);
    if (!ctl || !ctl.stream) return;
    const st = ctl.stream;
    try {
      const status = await getJSON(`/api/files/stream/status?session=${st.sessionId}`);
      st.pending = (status.need_seqs as number[]) || [];
      st.inflight.clear();
      const missingBytes = st.pending.reduce((s, seq) => s + sliceLen(st, ctl.total, seq), 0);
      st.doneBytes = ctl.total - missingBytes;
      ctl.synced = st.doneBytes;
      ctl.sent = st.doneBytes;
      ctl.shown = st.doneBytes;
      patch(id, { transferred: ctl.sent, synced: ctl.synced });
      if (st.pending.length > 0 && !ctl.file) throw new Error("缺少文件内容，无法继续上传");
      if (st.pending.length === 0) {
        try {
          await streamFinish(id);
          return;
        } catch (e) {
          if (!(e as { needMore?: boolean }).needMore) throw e;
          // fall through: missing slices were re-queued into st.pending
        }
      }
      await streamLoop(id);
    } catch (e) {
      const err = e as Error & { status?: number };
      if (ctl.canceled || ctl.paused) return;
      if (err.status === 404) { // session truly gone on the server
        ctl.file = undefined;
        dropStored(id);
        patch(id, { status: "error", error: "上传会话已失效，请重新上传", speed: 0, syncSpeed: 0, note: undefined });
      } else { // transient: stay resumable
        ctl.paused = true;
        patch(id, { status: "paused", speed: 0, syncSpeed: 0, note: "恢复失败，已暂停，点击「恢复」重试" });
      }
    }
  }, [patch, streamLoop, streamFinish]);

  // ---- download (ranged streaming, resumable, retrying) --------------------
  // Received bytes are flushed to IndexedDB so an interruption or page refresh
  // continues from the stored offset instead of restarting.
  const DL_FLUSH = 8 * 1024 * 1024;

  const flushDownload = useCallback(async (id: string) => {
    const ctl = ctls.current.get(id);
    if (!ctl || ctl.idbOff || ctl.chunks.length === 0) return;
    const part = new Blob(ctl.chunks as BlobPart[]);
    try {
      await chunkSave(id, ctl.dlSeq, part);
      ctl.dlSeq++;
      ctl.chunks = [];
    } catch {
      ctl.idbOff = true; // storage unavailable: keep buffering in memory
    }
  }, []);

  const driveDownload = useCallback(async (id: string) => {
    const ctl = ctls.current.get(id);
    if (!ctl) return;
    let attempt = 0;
    let pendBytes = ctl.chunks.reduce((n, c) => n + c.length, 0);
    for (;;) {
      const before = ctl.sent;
      try {
        ctl.abort = new AbortController();
        const headers: Record<string, string> = { ...authHeader() };
        if (ctl.sent > 0) headers.Range = `bytes=${ctl.sent}-`;
        const url = ctl.downloadUrl || `/api/files/${ctl.fileId}/download`;
        const res = await fetch(url, { headers, signal: ctl.abort.signal });
        if (!res.ok && res.status !== 206) throw new Error(`HTTP ${res.status}`);
        if (ctl.sent > 0 && res.status === 200) { // server ignored Range: restart clean
          ctl.chunks = []; ctl.sent = 0; ctl.shown = 0; ctl.dlSeq = 0; pendBytes = 0;
          await chunksDrop(id);
        }
        const reader = res.body!.getReader();
        ctl.lastTime = Date.now(); ctl.lastBytes = ctl.sent;
        if (ctl.retrying) { ctl.retrying = false; patch(id, { note: undefined }); }
        for (;;) {
          const { done, value } = await reader.read();
          if (done) break;
          if (ctl.canceled || ctl.paused) {
            reader.cancel().catch(() => {});
            if (ctl.paused) await flushDownload(id); // keep resume point durable
            return;
          }
          if (value) {
            ctl.chunks.push(value);
            ctl.sent += value.length;
            pendBytes += value.length;
            if (pendBytes >= DL_FLUSH) { await flushDownload(id); pendBytes = 0; }
            tick(id);
          }
        }
        await flushDownload(id);
        const finalSize = ctl.total || ctl.sent;
        let parts: Blob[] = [];
        if (!ctl.idbOff) { try { parts = await chunksLoad(id); } catch { /* fall back to memory */ } }
        saveBlob(new Blob([...parts, ...(ctl.chunks as BlobPart[])]), ctl.name);
        ctl.chunks = [];
        chunksDrop(id);
        patch(id, { status: "done", transferred: finalSize, speed: 0, note: undefined });
        postJSON("/api/transfers/log", { kind: "download", name: ctl.name, path: ctl.path, size: finalSize }).catch(() => {});
        return;
      } catch (e: unknown) {
        if (ctl.paused || ctl.canceled) { if (ctl.paused) flushDownload(id); return; }
        attempt = ctl.sent > before ? 1 : attempt + 1; // reset budget when progress was made
        if (attempt >= MAX_RETRY) {
          // keep the received bytes: pause instead of failing, resume continues via Range
          ctl.paused = true;
          await flushDownload(id);
          patch(id, { status: "paused", note: "网络持续异常，已暂停，点击「恢复」继续", speed: 0 });
          return;
        }
        ctl.retrying = true;
        patch(id, { note: `网络中断，正在重试 (${attempt}/${MAX_RETRY})…`, speed: 0 });
        await sleep(backoff(attempt - 1)); // resumes from ctl.sent via Range on next loop
      }
    }
  }, [patch, tick, flushDownload]);

  // ---- public api ----------------------------------------------------------
  const startUpload = useCallback((file: File, path: string) => {
    const id = uid();
    ctls.current.set(id, {
      kind: "upload", name: file.name, path, total: file.size, sent: 0, shown: 0, synced: 0,
      paused: false, canceled: false, file, chunks: [], dlSeq: 0, retrying: false,
      lastTime: Date.now(), lastBytes: 0, lastSynced: 0,
    });
    // persist the File so a refresh/interruption never needs re-picking
    fileSave(id, file).catch(() => {});
    setTransfers((l) => [{ id, kind: "upload", name: file.name, path, total: file.size, transferred: 0, speed: 0, status: "running" }, ...l]);
    (async () => {
      // streaming first; fall back to store-and-forward when unavailable
      if (file.size > 0) {
        try {
          if (await driveStreamUpload(id) === "ok") return;
        } catch { /* fall back */ }
      }
      driveUpload(id);
    })();
  }, [driveStreamUpload, driveUpload]);

  const startClientUpload = useCallback(async () => {
    throw new Error("当前页面未连接本地客户端");
  }, []);

  const startDownload = useCallback((item: { id: number; path: string; size: number; download_url?: string }) => {
    const id = uid();
    const name = item.path.split("/").pop() || "download";
    ctls.current.set(id, {
      kind: "download", name, path: item.path, total: item.size, sent: 0, shown: 0, synced: 0,
      paused: false, canceled: false, fileId: item.id, downloadUrl: item.download_url,
      chunks: [], dlSeq: 0, retrying: false,
      lastTime: Date.now(), lastBytes: 0, lastSynced: 0,
    });
    setTransfers((l) => [{ id, kind: "download", name, path: item.path, total: item.size, transferred: 0, speed: 0, status: "running" }, ...l]);
    driveDownload(id);
  }, [driveDownload]);

  const pause = useCallback((id: string) => {
    const ctl = ctls.current.get(id);
    if (!ctl) return;
    ctl.paused = true;
    patch(id, { status: "paused", speed: 0, syncSpeed: 0 });
    if (ctl.abort) { try { ctl.abort.abort(); } catch { /* ignore */ } }
    if (ctl.stream) ctl.stream.xhrs.forEach((x) => { try { x.abort(); } catch { /* ignore */ } });
  }, [patch]);

  const resume = useCallback(async (id: string) => {
    const ctl = ctls.current.get(id);
    if (!ctl) return;
    if (ctl.kind === "download") {
      ctl.paused = false;
      patch(id, { status: "running", note: undefined });
      driveDownload(id);
      return;
    }
    // Upload restored after a refresh: first try the File persisted in
    // IndexedDB (no user interaction); only if that's unavailable ask the user
    // to re-pick the original file (must match name+size).
    if (!ctl.file) {
      const stored = await fileLoad(id).catch(() => undefined);
      if (stored && stored.name === ctl.name && stored.size === ctl.total) {
        ctl.file = stored;
      } else {
        const f = await pickFile();
        if (!f) return; // picker dismissed: stay paused
        if (f.name !== ctl.name || f.size !== ctl.total) {
          patch(id, { note: "所选文件与原文件不一致（需同名同大小），未恢复" });
          return;
        }
        ctl.file = f;
        fileSave(id, f).catch(() => {});
      }
    }
    ctl.paused = false;
    ctl.shown = Math.min(ctl.shown, ctl.sent);
    patch(id, { status: "running", note: undefined });
    if (ctl.stream) { resumeStream(id); return; }
    if (ctl.uploadId) {
      // legacy resumable session: re-sync the server-side offset first
      try {
        ctl.sent = await getUploadOffset(ctl.uploadId);
        ctl.shown = ctl.sent;
        patch(id, { transferred: ctl.sent });
      } catch {
        ctl.file = undefined;
        patch(id, { status: "error", error: "上传会话已失效，请重新上传", speed: 0, note: undefined });
        return;
      }
      driveUpload(id);
      return;
    }
    // no session yet (refresh happened before init): start over from scratch
    ctl.sent = 0; ctl.shown = 0;
    if (ctl.total > 0) {
      try {
        if (await driveStreamUpload(id) === "ok") return;
      } catch { /* fall back */ }
    }
    driveUpload(id);
  }, [patch, driveUpload, driveDownload, resumeStream, driveStreamUpload]);

  const cancel = useCallback((id: string) => {
    const ctl = ctls.current.get(id);
    if (!ctl) return;
    ctl.canceled = true;
    ctl.chunks = [];   // free download buffer
    ctl.file = undefined;
    if (ctl.abort) { try { ctl.abort.abort(); } catch { /* ignore */ } }
    if (ctl.stream) {
      ctl.stream.xhrs.forEach((x) => { try { x.abort(); } catch { /* ignore */ } });
      postJSON("/api/files/stream/cancel", { session_id: ctl.stream.sessionId }).catch(() => {});
    } else if (ctl.kind === "upload" && ctl.uploadId) {
      postJSON("/api/files/upload/cancel", { upload_id: ctl.uploadId }).catch(() => {});
    }
    dropStored(id);
    patch(id, { status: "canceled", speed: 0, syncSpeed: 0 });
  }, [patch]);

  const remove = useCallback((id: string) => {
    ctls.current.delete(id);
    dropStored(id);
    setTransfers((l) => l.filter((t) => t.id !== id));
  }, []);

  const clearFinished = useCallback(() => {
    setTransfers((l) => l.filter((t) => {
      const keep = t.status === "running" || t.status === "paused" || t.status === "finalizing";
      if (!keep) { ctls.current.delete(t.id); dropStored(t.id); }
      return keep;
    }));
  }, []);

  const resolveJob = useCallback((jobId: string, status: string, error?: string) => {
    for (const [id, ctl] of ctls.current) {
      if (ctl.jobId !== jobId) continue;
      if (status === "done") { dropStored(id); patch(id, { status: "done", transferred: ctl.total, speed: 0 }); }
      else if (status === "failed") patch(id, { status: "error", error: error || "云端处理失败", speed: 0 });
    }
  }, [patch]);

  // After a refresh: reattach local data from IndexedDB (upload File handles,
  // download buffers), then automatically continue transfers that were running.
  const booted = useRef(false);
  useEffect(() => {
    if (booted.current) return;
    booted.current = true;
    const boot = restored.current;
    if (!boot || boot.list.length === 0) return;
    (async () => {
      for (const t of boot.list) {
        const ctl = ctls.current.get(t.id);
        if (!ctl) continue;
        if (t.status !== "paused") { dropStored(t.id); continue; } // finished entries: clean strays
        if (ctl.kind === "upload" && !ctl.file) {
          const f = await fileLoad(t.id).catch(() => undefined);
          if (f && f.name === ctl.name && f.size === ctl.total) ctl.file = f;
          else patch(t.id, { note: "原文件不可用，恢复时请重新选择" });
        }
        if (ctl.kind === "download") {
          // IndexedDB is the source of truth for how much really arrived
          const info = await chunksInfo(t.id).catch(() => ({ count: 0, bytes: 0 }));
          ctl.dlSeq = info.count;
          ctl.sent = info.bytes;
          ctl.shown = info.bytes;
          patch(t.id, { transferred: info.bytes });
        }
        // continue automatically if it was mid-flight when the page unloaded
        if (boot.resumeIds.includes(t.id) && (ctl.kind === "download" || ctl.file)) resume(t.id);
      }
    })();
  }, [patch, resume]);

  return (
    <Ctx.Provider value={{ clientMode: false, transfers, startUpload, startClientUpload, startDownload, pause, resume, cancel, remove, clearFinished, resolveJob }}>
      {children}
    </Ctx.Provider>
  );
}

function taskToTransfer(task: ClientTask): Transfer {
  const active = task.status === "waiting" || task.status === "hashing" || task.status === "running";
  const status: TransferStatus = active
    ? "running"
    : task.status === "done" ? "done" : task.status === "error" ? "error" : "canceled";
  const note = task.status === "waiting"
    ? "等待本地客户端调度"
    : task.status === "hashing"
      ? "正在本机计算文件指纹"
      : undefined;
  return {
    id: task.id,
    kind: task.kind,
    name: task.name,
    path: task.kind === "upload" ? task.remote_path : task.local_path || task.remote_path,
    total: task.size,
    transferred: task.done,
    speed: task.speed,
    eta: task.status === "running" && task.speed > 1024 && task.size > task.done
      ? (task.size - task.done) / task.speed
      : undefined,
    status,
    note,
    error: task.error,
  };
}

// In client mode the browser is only a control surface. The daemon reads and
// writes local files, talks to Baidu, and exposes lightweight task snapshots.
function ClientTransferProvider({ children }: { children: ReactNode }) {
  const [transfers, setTransfers] = useState<Transfer[]>([]);

  const refresh = useCallback(async () => {
    const tasks = await clientApi.tasks();
    setTransfers(tasks.map(taskToTransfer));
  }, []);

  useEffect(() => {
    let alive = true;
    const poll = async () => {
      try {
        const tasks = await clientApi.tasks();
        if (alive) setTransfers(tasks.map(taskToTransfer));
      } catch { /* login/server transitions are handled by the surrounding UI */ }
    };
    poll();
    const timer = window.setInterval(poll, 800);
    return () => { alive = false; window.clearInterval(timer); };
  }, []);

  const startClientUpload = useCallback(async (localPath: string, remoteDir: string) => {
    await clientApi.upload(localPath, remoteDir);
    await refresh();
  }, [refresh]);

  const startDownload = useCallback(async (item: { id: number; path: string; size: number }) => {
    const name = item.path.split("/").pop() || "download";
    await clientApi.download({ path: item.path, name });
    await refresh();
  }, [refresh]);

  const cancel = useCallback(async (id: string) => {
    await clientApi.cancel(id);
    await refresh();
  }, [refresh]);

  const remove = useCallback(async (id: string) => {
    await clientApi.remove(id);
    setTransfers((list) => list.filter((t) => t.id !== id));
  }, []);

  const clearFinished = useCallback(() => {
    const finished = transfers.filter((t) => t.status === "done" || t.status === "error" || t.status === "canceled");
    Promise.all(finished.map((t) => clientApi.remove(t.id).catch(() => undefined))).then(refresh);
  }, [transfers, refresh]);

  const noop = useCallback(() => {}, []);
  const startUpload = useCallback(() => {
    throw new Error("客户端模式请从本地文件选择器添加上传任务");
  }, []);

  return (
    <Ctx.Provider value={{
      clientMode: true,
      transfers,
      startUpload,
      startClientUpload,
      startDownload,
      pause: noop,
      resume: noop,
      cancel,
      remove,
      clearFinished,
      resolveJob: noop,
    }}>
      {children}
    </Ctx.Provider>
  );
}
