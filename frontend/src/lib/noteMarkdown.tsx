import { Fragment, type ReactNode } from "react";

export type Inline =
  | { t: "text"; v: string }
  | { t: "strong"; kids: Inline[] }
  | { t: "em"; kids: Inline[] }
  | { t: "code"; v: string }
  | { t: "img"; alt: string; src: string }
  | { t: "link"; href: string; kids: Inline[] };

export type Block =
  | { t: "h"; level: 1 | 2 | 3; kids: Inline[] }
  | { t: "p"; kids: Inline[] }
  | { t: "ul"; items: Inline[][] }
  | { t: "ol"; items: Inline[][] }
  | { t: "quote"; kids: Inline[] };

const NOTE_IMG = /^\/api\/notes\/images\/(\d+)$/;

export function noteImageId(src: string): number | null {
  const m = src.trim().match(NOTE_IMG);
  return m ? Number(m[1]) : null;
}

export function isSafeHref(href: string): boolean {
  const h = href.trim();
  if (NOTE_IMG.test(h)) return true;
  return /^https?:\/\//i.test(h);
}

export function parseInlines(s: string): Inline[] {
  const out: Inline[] = [];
  const re = /(!?\[([^\]]*)\]\(([^)]+)\)|`([^`]+)`|\*\*([^*]+)\*\*|\*([^*]+)\*)/g;
  let last = 0;
  let m: RegExpExecArray | null;
  while ((m = re.exec(s))) {
    if (m.index > last) out.push({ t: "text", v: s.slice(last, m.index) });
    if (m[0].startsWith("![")) {
      const src = (m[3] || "").trim();
      if (isSafeHref(src)) out.push({ t: "img", alt: m[2] || "", src });
      else out.push({ t: "text", v: m[0] });
    } else if (m[0].startsWith("[")) {
      const href = (m[3] || "").trim();
      if (isSafeHref(href) && !NOTE_IMG.test(href)) {
        out.push({ t: "link", href, kids: parseInlines(m[2] || href) });
      } else out.push({ t: "text", v: m[0] });
    } else if (m[4] !== undefined) {
      out.push({ t: "code", v: m[4] });
    } else if (m[5] !== undefined) {
      out.push({ t: "strong", kids: parseInlines(m[5]) });
    } else if (m[6] !== undefined) {
      out.push({ t: "em", kids: parseInlines(m[6]) });
    }
    last = m.index + m[0].length;
  }
  if (last < s.length) out.push({ t: "text", v: s.slice(last) });
  return out;
}

export function parseNote(body: string): Block[] {
  const lines = body.replace(/\r\n/g, "\n").split("\n");
  const blocks: Block[] = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    if (line.trim() === "") { i++; continue; }
    const hm = line.match(/^(#{1,3})\s+(.+)$/);
    if (hm) {
      blocks.push({ t: "h", level: hm[1].length as 1 | 2 | 3, kids: parseInlines(hm[2]) });
      i++;
      continue;
    }
    if (/^>\s?/.test(line)) {
      const text = line.replace(/^>\s?/, "");
      blocks.push({ t: "quote", kids: parseInlines(text) });
      i++;
      continue;
    }
    if (/^[-*]\s+/.test(line)) {
      const items: Inline[][] = [];
      while (i < lines.length && /^[-*]\s+/.test(lines[i])) {
        items.push(parseInlines(lines[i].replace(/^[-*]\s+/, "")));
        i++;
      }
      blocks.push({ t: "ul", items });
      continue;
    }
    if (/^\d+\.\s+/.test(line)) {
      const items: Inline[][] = [];
      while (i < lines.length && /^\d+\.\s+/.test(lines[i])) {
        items.push(parseInlines(lines[i].replace(/^\d+\.\s+/, "")));
        i++;
      }
      blocks.push({ t: "ol", items });
      continue;
    }
    const para: string[] = [line];
    i++;
    while (i < lines.length && lines[i].trim() !== "" && !/^(#{1,3}\s+|[-*]\s+|\d+\.\s+|>\s?)/.test(lines[i])) {
      para.push(lines[i]);
      i++;
    }
    blocks.push({ t: "p", kids: parseInlines(para.join("\n")) });
  }
  return blocks;
}

export function renderInlines(
  kids: Inline[],
  renderImage: (src: string, alt: string) => ReactNode,
): ReactNode[] {
  return kids.map((n, i) => {
    if (n.t === "text") return <Fragment key={i}>{n.v}</Fragment>;
    if (n.t === "code") return <code key={i}>{n.v}</code>;
    if (n.t === "strong") return <strong key={i}>{renderInlines(n.kids, renderImage)}</strong>;
    if (n.t === "em") return <em key={i}>{renderInlines(n.kids, renderImage)}</em>;
    if (n.t === "link") return <a key={i} href={n.href} target="_blank" rel="noreferrer noopener">{renderInlines(n.kids, renderImage)}</a>;
    return <Fragment key={i}>{renderImage(n.src, n.alt)}</Fragment>;
  });
}

export function renderNote(
  body: string,
  renderImage: (src: string, alt: string) => ReactNode,
): ReactNode {
  const blocks = parseNote(body);
  if (blocks.length === 0) return null;
  return blocks.map((b, i) => {
    if (b.t === "h") {
      const Tag = (b.level === 1 ? "h2" : b.level === 2 ? "h3" : "h4") as "h2" | "h3" | "h4";
      return <Tag key={i}>{renderInlines(b.kids, renderImage)}</Tag>;
    }
    if (b.t === "quote") return <blockquote key={i}>{renderInlines(b.kids, renderImage)}</blockquote>;
    if (b.t === "ul") return <ul key={i}>{b.items.map((it, j) => <li key={j}>{renderInlines(it, renderImage)}</li>)}</ul>;
    if (b.t === "ol") return <ol key={i}>{b.items.map((it, j) => <li key={j}>{renderInlines(it, renderImage)}</li>)}</ol>;
    return <p key={i}>{renderInlines(b.kids, renderImage)}</p>;
  });
}
