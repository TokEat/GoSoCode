import { describe, it, expect } from "vitest";
import SparkMD5 from "spark-md5";
import {
  computeBlockList,
  computeBlockListSequential,
  formatHashNote,
  hashPercent,
  parseCheckProgress,
  shouldPublishHashProgress,
} from "./blockList";

function makeFile(bytes: number, fill = 0x5a): File {
  const buf = new Uint8Array(bytes);
  for (let i = 0; i < bytes; i++) buf[i] = (fill + i * 13) & 0xff;
  return new File([buf], `t-${bytes}.bin`, { type: "application/octet-stream" });
}

async function independentSequential(file: Blob, sliceSize: number): Promise<string[]> {
  const total = Math.max(1, Math.ceil(file.size / sliceSize));
  const out: string[] = [];
  for (let i = 0; i < total; i++) {
    const buf = await file.slice(i * sliceSize, Math.min((i + 1) * sliceSize, file.size)).arrayBuffer();
    out.push(SparkMD5.ArrayBuffer.hash(buf));
  }
  return out;
}

describe("computeBlockList parallel vs sequential", () => {
  const cases: { size: number; slice: number; label: string }[] = [
    { size: 0, slice: 8, label: "empty file still yields one md5" },
    { size: 7, slice: 8, label: "single short slice" },
    { size: 8, slice: 8, label: "exactly one slice" },
    { size: 16, slice: 8, label: "two full slices" },
    { size: 17, slice: 8, label: "two full + short last slice" },
    { size: 64, slice: 8, label: "many equal slices" },
    { size: 100, slice: 16, label: "uneven multi-slice" },
    { size: 1024, slice: 100, label: "larger multi-slice" },
  ];

  for (const c of cases) {
    it(c.label, async () => {
      const file = makeFile(c.size);
      const expected = await independentSequential(file, c.slice);
      const seq = await computeBlockListSequential(file, c.slice);
      const par2 = await computeBlockList(file, c.slice, () => {}, () => false, 2);
      const par4 = await computeBlockList(file, c.slice, () => {}, () => false, 4);
      expect(seq).toEqual(expected);
      expect(par2).toEqual(expected);
      expect(par4).toEqual(expected);
      expect(expected.length).toBe(Math.max(1, Math.ceil(c.size / c.slice) || 1));
    });
  }

  it("reports monotonic progress covering every slice", async () => {
    const file = makeFile(40);
    const events: number[] = [];
    const out = await computeBlockList(file, 8, (done, total) => {
      events.push(done);
      expect(total).toBe(5);
    }, () => false, 3);
    expect(out).toHaveLength(5);
    expect(events).toHaveLength(5);
    expect([...events].sort((a, b) => a - b)).toEqual([1, 2, 3, 4, 5]);
    expect(new Set(events).size).toBe(5);
  });

  it("returns null when canceled mid-hash", async () => {
    const file = makeFile(64);
    let done = 0;
    const out = await computeBlockList(file, 8, (n) => { done = n; }, () => done >= 2, 2);
    expect(out).toBeNull();
  });
});

describe("hash progress helpers", () => {
  it("formats a checksum note, not a byte count", () => {
    expect(formatHashNote(0)).toBe("正在校验，准备秒传… 0%");
    expect(formatHashNote(30)).toBe("正在校验，准备秒传… 30%");
    expect(formatHashNote(100)).toBe("正在校验，准备秒传… 100%");
    expect(hashPercent(3, 10)).toBe(30);
    expect(hashPercent(0, 10)).toBe(0);
    expect(hashPercent(1, 0)).toBe(100);
  });

  it("parses a percentage out of the checksum / client note", () => {
    expect(parseCheckProgress("正在校验，准备秒传… 30%")).toBe(30);
    expect(parseCheckProgress("正在计算文件指纹 5%")).toBe(5);
    expect(parseCheckProgress("no percent here")).toBeUndefined();
  });

  it("throttles intermediate updates but always publishes 0 and 100", () => {
    expect(shouldPublishHashProgress(-1, 0, 0)).toBe(true);
    expect(shouldPublishHashProgress(0, 0, 1000)).toBe(false);
    expect(shouldPublishHashProgress(0, 3, 50)).toBe(false);
    expect(shouldPublishHashProgress(0, 5, 50)).toBe(true);
    expect(shouldPublishHashProgress(10, 12, 200)).toBe(true);
    expect(shouldPublishHashProgress(90, 100, 0)).toBe(true);
  });
});
