import { describe, it, expect } from "vitest";
import { isSafeHref, noteImageId, parseNote } from "./noteMarkdown";

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
