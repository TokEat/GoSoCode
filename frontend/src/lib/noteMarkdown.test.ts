import { describe, it, expect } from "vitest";
import { isSafeHref, isSimpleNoteText, noteImageId, parseNote, SIMPLE_NOTE_MAX_CHARS, simpleNoteLine } from "./noteMarkdown";

describe("parseNote", () => {
  it("parses headings, paragraphs, emphasis and lists", () => {
    const blocks = parseNote("# 标题\n\n这是 **粗体** 和 *斜体*，还有 `code`。\n\n- 一项\n- 二项");
    expect(blocks[0]).toMatchObject({ t: "h", level: 1 });
    expect(blocks[1].t).toBe("p");
    expect(blocks[2]).toMatchObject({ t: "ul" });
    const p = blocks[1] as { kids: { t: string; v?: string }[] };
    const types = p.kids.map((k) => k.t);
    expect(types).toContain("strong");
    expect(types).toContain("em");
    expect(types).toContain("code");
  });

  it("keeps a safe note image and drops unsafe urls", () => {
    const blocks = parseNote("![示意](/api/notes/images/12)\n\n![坏](javascript:alert(1))");
    const first = (blocks[0] as { kids: any[] }).kids[0];
    expect(first).toEqual({ t: "img", alt: "示意", src: "/api/notes/images/12" });
    expect(noteImageId("/api/notes/images/12")).toBe(12);
    const secondKids = (blocks[1] as { kids: any[] }).kids;
    expect(secondKids.some((k) => k.t === "img")).toBe(false);
  });

  it("rejects unsafe hrefs", () => {
    expect(isSafeHref("https://example.com/a.png")).toBe(true);
    expect(isSafeHref("/api/notes/images/3")).toBe(true);
    expect(isSafeHref("javascript:alert(1)")).toBe(false);
    expect(isSafeHref("/etc/passwd")).toBe(false);
  });
});

describe("isSimpleNoteText", () => {
  it("accepts a short plain line", () => {
    expect(isSimpleNoteText("这是一份简短备注")).toBe(true);
    expect(isSimpleNoteText("  hello world  ")).toBe(true);
  });

  it("accepts two short lines and joins them", () => {
    expect(isSimpleNoteText("第一行\n第二行")).toBe(true);
    expect(isSimpleNoteText("第一行\n\n第二行")).toBe(true);
    expect(simpleNoteLine("第一行\n\n第二行")).toBe("第一行 第二行");
  });

  it("accepts inline italic, bold and code", () => {
    expect(isSimpleNoteText("这是 **重要** 和 *临时* 的 `code`")).toBe(true);
  });

  it("rejects empty or whitespace-only text", () => {
    expect(isSimpleNoteText("")).toBe(false);
    expect(isSimpleNoteText("   \n  ")).toBe(false);
  });

  it("rejects more than two non-empty lines", () => {
    expect(isSimpleNoteText("a\nb\nc")).toBe(false);
  });

  it("rejects text longer than the row budget", () => {
    expect(isSimpleNoteText("啊".repeat(SIMPLE_NOTE_MAX_CHARS))).toBe(true);
    expect(isSimpleNoteText("啊".repeat(SIMPLE_NOTE_MAX_CHARS + 1))).toBe(false);
    expect(isSimpleNoteText("甲".repeat(70) + "\n" + "乙".repeat(70))).toBe(false);
  });

  it("rejects headings, lists, quotes, fences and images", () => {
    expect(isSimpleNoteText("# 标题")).toBe(false);
    expect(isSimpleNoteText("## 小标题")).toBe(false);
    expect(isSimpleNoteText("- 一项")).toBe(false);
    expect(isSimpleNoteText("* 一项")).toBe(false);
    expect(isSimpleNoteText("+ 一项")).toBe(false);
    expect(isSimpleNoteText("1. 一项")).toBe(false);
    expect(isSimpleNoteText("> 引用")).toBe(false);
    expect(isSimpleNoteText("```\ncode\n```")).toBe(false);
    expect(isSimpleNoteText("前言 ```js")).toBe(false);
    expect(isSimpleNoteText("见图 ![示意图](/api/notes/images/12)")).toBe(false);
    expect(isSimpleNoteText("![](https://example.com/a.png)")).toBe(false);
    expect(isSimpleNoteText("见 ](/api/notes/images/3)")).toBe(false);
  });
});
