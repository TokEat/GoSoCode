import type { FileItem } from "./api";

export function normDir(d: string): string {
  let s = d.startsWith("/") ? d : "/" + d;
  if (!s.endsWith("/")) s += "/";
  return s.replace(/\/+/g, "/");
}

export function fullPath(p: string): string {
  return p.startsWith("/") ? p : "/" + p;
}

export function parentDir(p: string): string {
  const fp = fullPath(p).replace(/\/+$/, "");
  const i = fp.lastIndexOf("/");
  return i <= 0 ? "/" : fp.slice(0, i) + "/";
}

export function baseName(p: string): string {
  const fp = fullPath(p).replace(/\/+$/, "");
  return fp.slice(fp.lastIndexOf("/") + 1);
}

/** Where the explorer should go after a search hit is chosen. */
export function searchLocateTarget(item: FileItem): { cwd: string; highlightPath: string } {
  const fp = fullPath(item.path);
  if (item.is_dir) {
    return { cwd: normDir(fp), highlightPath: "" };
  }
  return { cwd: parentDir(fp), highlightPath: fp };
}
