import { fullPath, normDir, parentDir } from "./searchLocate";

export const APP_PAGES = [
  "overview", "files", "share", "transfers", "profile",
  "users", "nodes", "traffic", "logs", "settings", "client",
] as const;

export type AppPage = typeof APP_PAGES[number];

export interface ParsedHash {
  page: AppPage;
  /** Raw path after the page segment, before `?` (may be encoded, may end with `/`). */
  rest: string;
  query: string;
}

export interface FilesLocation {
  path: string;
  /** True when the hash path is empty/root or ended with `/`, so it is unambiguously a directory. */
  explicitDir: boolean;
}

export interface FilesView {
  cwd: string;
  locatePath: string;
  /** Path that still needs a parent listing to decide file vs directory. */
  pending: string | null;
}

export function safeDecode(seg: string): string {
  try {
    return decodeURIComponent(seg);
  } catch {
    return seg;
  }
}

/** Clean a logical filesystem path: rooted, no `.` / `..` escape, collapsed slashes. */
export function cleanLogicalPath(input: string): string {
  const raw = String(input || "").replace(/\\/g, "/").replace(/\u0000/g, "");
  const parts: string[] = [];
  for (const seg of raw.split("/")) {
    const s = safeDecode(seg);
    if (!s || s === ".") continue;
    if (s === "..") {
      if (parts.length) parts.pop();
      continue;
    }
    parts.push(s);
  }
  return parts.length ? "/" + parts.join("/") : "/";
}

export function parseAppHash(hash: string): ParsedHash {
  let h = String(hash || "");
  if (h.startsWith("#")) h = h.slice(1);
  if (h.startsWith("/")) h = h.slice(1);
  const qIdx = h.indexOf("?");
  const query = qIdx >= 0 ? h.slice(qIdx + 1) : "";
  const pathPart = qIdx >= 0 ? h.slice(0, qIdx) : h;
  const slash = pathPart.indexOf("/");
  const pageSeg = slash === -1 ? pathPart : pathPart.slice(0, slash);
  const rest = slash === -1 ? "" : pathPart.slice(slash + 1);
  const page = (APP_PAGES as readonly string[]).includes(pageSeg) ? (pageSeg as AppPage) : "overview";
  return { page, rest, query };
}

export function pageFromHash(hash: string = typeof window !== "undefined" ? window.location.hash : ""): AppPage {
  return parseAppHash(hash).page;
}

/** Hash for a nav page. When already on files, keep the current files path. */
export function hashForPage(page: AppPage, currentHash: string = ""): string {
  if (page === "files") {
    const cur = parseAppHash(currentHash);
    if (cur.page === "files") return normalizeHash(currentHash) || "#/files";
    return "#/files";
  }
  return "#/" + page;
}

export function filesHashFor(path: string, _kind?: "dir" | "file"): string {
  const cleaned = cleanLogicalPath(path);
  if (cleaned === "/") return "#/files";
  const encoded = cleaned.split("/").filter(Boolean).map(encodeURIComponent).join("/");
  return "#/files/" + encoded;
}

export function filesLocationFromHash(hash: string): FilesLocation | null {
  const parsed = parseAppHash(hash);
  if (parsed.page !== "files") return null;
  const rest = parsed.rest;
  const explicitDir = rest === "" || rest.endsWith("/");
  return { path: cleanLogicalPath(rest), explicitDir };
}

/**
 * Decide cwd / locatePath from a files hash location.
 * When the last segment may be a file or a directory, pass the parent listing
 * to resolve it; otherwise `pending` asks the caller to list the parent first.
 */
export function viewFromFilesLocation(
  loc: FilesLocation,
  listing?: { path: string; is_dir: boolean }[],
  listedDir?: string,
): FilesView {
  if (loc.path === "/") return { cwd: "/", locatePath: "", pending: null };
  if (loc.explicitDir) return { cwd: normDir(loc.path), locatePath: "", pending: null };
  const fp = fullPath(loc.path).replace(/\/+$/, "");
  const parent = parentDir(fp);
  if (listing && listedDir !== undefined && normDir(listedDir) === normDir(parent)) {
    const fileHit = listing.some((f) => !f.is_dir && fullPath(f.path).replace(/\/+$/, "") === fp);
    if (fileHit) return { cwd: normDir(parent), locatePath: fp, pending: null };
    return { cwd: normDir(fp), locatePath: "", pending: null };
  }
  return { cwd: normDir(parent), locatePath: "", pending: fp };
}

export function normalizeHash(hash: string): string {
  const parsed = parseAppHash(hash);
  if (parsed.page !== "files") {
    return "#/" + parsed.page + (parsed.query ? "?" + parsed.query : "");
  }
  const loc = filesLocationFromHash(hash);
  return loc ? filesHashFor(loc.path) : "#/files";
}

export function hashesEqual(a: string, b: string): boolean {
  return normalizeHash(a) === normalizeHash(b);
}
