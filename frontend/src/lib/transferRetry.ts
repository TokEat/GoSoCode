export const MAX_SLICE_RETRY = 6;   // consecutive attempts on one slice/chunk before backing out
export const MAX_AUTO_RESUME = 3;   // automatic whole-transfer resumes after a stall; then pause for a click

export const sleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));

export function backoffMs(attempt: number, random: () => number = Math.random): number {
  return Math.min(1000 * 2 ** attempt, 8000) + Math.floor(random() * 400);
}

export function autoResumeDelayMs(resumeIndex: number, random: () => number = Math.random): number {
  return Math.min(2000 * 2 ** resumeIndex, 20000) + Math.floor(random() * 800);
}

export const PAUSE_NOTE = "网络持续异常，已暂停，点击「恢复」继续";
export const FINISH_PAUSE_NOTE = "云端保存未完成，已暂停，点击「恢复」重试";

export function sliceRetryNote(attempt: number, max = MAX_SLICE_RETRY): string {
  return `网络波动，正在重试 (${attempt}/${max})…`;
}

export function autoResumeNote(index: number, max = MAX_AUTO_RESUME): string {
  return `网络波动，即将自动恢复 (${index}/${max})…`;
}

export type AutoResumeDecision =
  | { resume: true; index: number; note: string }
  | { resume: false; note: string };

export function nextAutoResume(used: number, max = MAX_AUTO_RESUME): AutoResumeDecision {
  if (used >= max) return { resume: false, note: PAUSE_NOTE };
  const index = used + 1;
  return { resume: true, index, note: autoResumeNote(index, max) };
}

export function isRetryableTransferError(e: { status?: number; aborted?: boolean } | unknown): boolean {
  const err = e as { status?: number; aborted?: boolean };
  if (err?.aborted) return false;
  const s = err?.status;
  if (s === 401 || s === 403 || s === 404) return false;
  return true;
}

export function describeErr(e: unknown): string {
  const m = (e as Error)?.message || String(e);
  if (/failed to fetch|networkerror|load failed|err_/i.test(m)) return "网络中断或丢包，连接被重置";
  return m;
}
