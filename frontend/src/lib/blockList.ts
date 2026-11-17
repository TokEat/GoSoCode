import SparkMD5 from "spark-md5";

// Bound the number of slices decoded and hashed at once. VIP/SVIP slices are
// 16/32MB; 3 in flight stays under ~96MB of extra browser memory.
export const HASH_CONCURRENCY = 3;

export function hashPercent(done: number, total: number): number {
  if (total <= 0) return 100;
  return Math.min(100, Math.max(0, Math.round((done * 100) / total)));
}

export function formatHashNote(pct: number): string {
  return `正在校验，准备秒传… ${pct}%`;
}

export function parseCheckProgress(text?: string | null): number | undefined {
  if (!text) return undefined;
  const m = text.match(/(\d+)\s*%/);
  if (!m) return undefined;
  return Math.min(100, Math.max(0, Number(m[1])));
}

export function shouldPublishHashProgress(prevPct: number, nextPct: number, elapsedMs: number): boolean {
  if (nextPct === prevPct) return false;
  if (nextPct === 0 || nextPct === 100) return true;
  return nextPct - prevPct >= 5 || elapsedMs >= 200;
}

export async function hashSlice(buf: ArrayBuffer): Promise<string> {
  return SparkMD5.ArrayBuffer.hash(buf);
}

// Sequential reference implementation (also used when concurrency is 1).
export async function computeBlockListSequential(
  file: Blob, sliceSize: number,
  onProgress: (done: number, total: number) => void = () => {},
  isCanceled: () => boolean = () => false,
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

// computeBlockList md5-hashes the file slice by slice (local read only, no
// network) — required by Baidu precreate and what unlocks rapid-upload (秒传).
// Slices are hashed with bounded parallelism; results stay in seq order and
// must match a sequential SparkMD5 pass over the same slices.
export async function computeBlockList(
  file: Blob, sliceSize: number,
  onProgress: (done: number, total: number) => void = () => {},
  isCanceled: () => boolean = () => false,
  concurrency = HASH_CONCURRENCY,
): Promise<string[] | null> {
  const total = Math.max(1, Math.ceil(file.size / sliceSize));
  const workers = Math.min(Math.max(1, concurrency), total);
  if (workers <= 1) return computeBlockListSequential(file, sliceSize, onProgress, isCanceled);

  const out: string[] = new Array(total);
  let next = 0;
  let completed = 0;

  const runWorker = async () => {
    for (;;) {
      if (isCanceled()) return;
      const i = next++;
      if (i >= total) return;
      const buf = await file.slice(i * sliceSize, Math.min((i + 1) * sliceSize, file.size)).arrayBuffer();
      if (isCanceled()) return;
      out[i] = SparkMD5.ArrayBuffer.hash(buf);
      completed++;
      onProgress(completed, total);
    }
  };

  await Promise.all(Array.from({ length: workers }, () => runWorker()));
  if (isCanceled()) return null;
  return out;
}
