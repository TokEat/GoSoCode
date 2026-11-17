import { describe, it, expect } from "vitest";
import {
  MAX_AUTO_RESUME,
  MAX_SLICE_RETRY,
  PAUSE_NOTE,
  autoResumeDelayMs,
  autoResumeNote,
  backoffMs,
  describeErr,
  isRetryableTransferError,
  nextAutoResume,
  sliceRetryNote,
} from "./transferRetry";

describe("nextAutoResume", () => {
  it("allows a capped number of automatic resumes, then pauses for a click", () => {
    const first = nextAutoResume(0);
    expect(first).toEqual({ resume: true, index: 1, note: autoResumeNote(1) });
    const last = nextAutoResume(MAX_AUTO_RESUME - 1);
    expect(last).toEqual({ resume: true, index: MAX_AUTO_RESUME, note: autoResumeNote(MAX_AUTO_RESUME) });
    expect(nextAutoResume(MAX_AUTO_RESUME)).toEqual({ resume: false, note: PAUSE_NOTE });
    expect(nextAutoResume(99)).toEqual({ resume: false, note: PAUSE_NOTE });
  });

  it("keeps the familiar manual-resume wording when the cap is hit", () => {
    expect(PAUSE_NOTE).toContain("点击「恢复」继续");
    expect(sliceRetryNote(2)).toBe(`网络波动，正在重试 (2/${MAX_SLICE_RETRY})…`);
    expect(MAX_SLICE_RETRY).toBeGreaterThanOrEqual(5);
  });
});

describe("backoff", () => {
  it("grows exponentially and caps, using the injected RNG for the jitter", () => {
    expect(backoffMs(0, () => 0)).toBe(1000);
    expect(backoffMs(1, () => 0)).toBe(2000);
    expect(backoffMs(3, () => 0)).toBe(8000);
    expect(backoffMs(8, () => 0)).toBe(8000);
    expect(backoffMs(0, () => 0.5)).toBe(1000 + 200);
    expect(autoResumeDelayMs(0, () => 0)).toBe(2000);
    expect(autoResumeDelayMs(1, () => 0)).toBe(4000);
    expect(autoResumeDelayMs(8, () => 0)).toBe(20000);
  });
});

describe("isRetryableTransferError", () => {
  it("retries network / 5xx / 429, but not auth, missing sessions, or aborts", () => {
    expect(isRetryableTransferError(new Error("failed to fetch"))).toBe(true);
    expect(isRetryableTransferError({ status: 502 })).toBe(true);
    expect(isRetryableTransferError({ status: 429 })).toBe(true);
    expect(isRetryableTransferError({ status: 409 })).toBe(true);
    expect(isRetryableTransferError({ status: 401 })).toBe(false);
    expect(isRetryableTransferError({ status: 403 })).toBe(false);
    expect(isRetryableTransferError({ status: 404 })).toBe(false);
    expect(isRetryableTransferError({ aborted: true })).toBe(false);
  });
});

describe("describeErr", () => {
  it("maps browser network failures to a stable Chinese note", () => {
    expect(describeErr(new Error("Failed to fetch"))).toBe("网络中断或丢包，连接被重置");
    expect(describeErr(new Error("分片大小不符"))).toBe("分片大小不符");
  });
});
