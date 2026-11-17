import { describe, it, expect } from "vitest";
import {
  cleanLogicalPath,
  filesHashFor,
  filesLocationFromHash,
  hashForPage,
  hashesEqual,
  parseAppHash,
  pageFromHash,
  viewFromFilesLocation,
} from "./hashRoute";

describe("parseAppHash / pageFromHash", () => {
  it("uses only the first segment as the page name", () => {
    expect(pageFromHash("#/files")).toBe("files");
    expect(pageFromHash("#/files/")).toBe("files");
    expect(pageFromHash("#/files/资料盘/docs")).toBe("files");
    expect(pageFromHash("#/files/资料盘/docs/a.txt")).toBe("files");
    expect(parseAppHash("#/files/资料盘/docs").rest).toBe("资料盘/docs");
  });

  it("still recognizes #/files?... and #/share?dir=", () => {
    expect(pageFromHash("#/files?foo=1")).toBe("files");
    expect(parseAppHash("#/files?foo=1")).toMatchObject({ page: "files", rest: "", query: "foo=1" });
    expect(pageFromHash("#/share?dir=%2F%E8%B5%84%E6%96%99%E7%9B%98")).toBe("share");
    const share = parseAppHash("#/share?dir=" + encodeURIComponent("/资料盘"));
    expect(share.page).toBe("share");
    expect(share.rest).toBe("");
    expect(share.query).toBe("dir=" + encodeURIComponent("/资料盘"));
  });

  it("falls back to overview for unknown pages, not for files subpaths", () => {
    expect(pageFromHash("#/overview")).toBe("overview");
    expect(pageFromHash("#/transfers")).toBe("transfers");
    expect(pageFromHash("#/not-a-page")).toBe("overview");
    expect(pageFromHash("#/not-a-page/foo")).toBe("overview");
    expect(pageFromHash("")).toBe("overview");
    expect(pageFromHash("#/")).toBe("overview");
  });
});

describe("hashForPage", () => {
  it("writes #/files at root when entering files from another page", () => {
    expect(hashForPage("files", "#/transfers")).toBe("#/files");
    expect(hashForPage("transfers", "#/files/资料盘")).toBe("#/transfers");
    expect(hashForPage("share", "#/files")).toBe("#/share");
  });

  it("does not clobber an existing files path", () => {
    expect(hashForPage("files", "#/files/资料盘/docs")).toBe(filesHashFor("/资料盘/docs"));
    expect(hashForPage("files", "#/files/")).toBe("#/files");
  });
});

describe("filesHashFor / filesLocationFromHash", () => {
  it("serializes root, directories and files", () => {
    expect(filesHashFor("/", "dir")).toBe("#/files");
    expect(filesHashFor("/资料盘/docs", "dir")).toBe("#/files/" + encodeURIComponent("资料盘") + "/docs");
    expect(filesHashFor("/资料盘/docs/a.txt", "file")).toBe(
      "#/files/" + encodeURIComponent("资料盘") + "/docs/" + encodeURIComponent("a.txt"),
    );
  });

  it("round-trips through encoding, including Chinese", () => {
    const dirHash = filesHashFor("/资料盘/docs", "dir");
    expect(filesLocationFromHash(dirHash)).toEqual({ path: "/资料盘/docs", explicitDir: false });
    const fileHash = filesHashFor("/资料盘/docs/年度报告.pdf", "file");
    expect(filesLocationFromHash(fileHash)).toEqual({ path: "/资料盘/docs/年度报告.pdf", explicitDir: false });
    expect(hashesEqual(dirHash, "#/files/资料盘/docs")).toBe(true);
  });

  it("treats a trailing slash as an explicit directory, and root as a directory", () => {
    expect(filesLocationFromHash("#/files")).toEqual({ path: "/", explicitDir: true });
    expect(filesLocationFromHash("#/files/")).toEqual({ path: "/", explicitDir: true });
    expect(filesLocationFromHash("#/files/资料盘/docs/")).toEqual({ path: "/资料盘/docs", explicitDir: true });
    expect(filesLocationFromHash("#/share?dir=/x")).toBeNull();
  });

  it("cleans .. and odd slashes without escaping the root", () => {
    expect(cleanLogicalPath("/资料盘/../etc")).toBe("/etc");
    expect(cleanLogicalPath("/../../etc")).toBe("/etc");
    expect(cleanLogicalPath("资料盘//docs/./a.txt")).toBe("/资料盘/docs/a.txt");
    expect(filesLocationFromHash("#/files/foo/../../secret")).toEqual({ path: "/secret", explicitDir: false });
  });
});

describe("viewFromFilesLocation", () => {
  const listing = [
    { path: "/资料盘/docs/a.txt", is_dir: false },
    { path: "/资料盘/docs/sub", is_dir: true },
  ];

  it("opens a directory path in that directory", () => {
    expect(viewFromFilesLocation({ path: "/资料盘/docs", explicitDir: true })).toEqual({
      cwd: "/资料盘/docs/", locatePath: "", pending: null,
    });
  });

  it("resolves a file path to the parent listing plus locatePath", () => {
    expect(viewFromFilesLocation({ path: "/资料盘/docs/a.txt", explicitDir: false }, listing, "/资料盘/docs/")).toEqual({
      cwd: "/资料盘/docs/", locatePath: "/资料盘/docs/a.txt", pending: null,
    });
  });

  it("resolves a non-file last segment as a directory once the parent listing is available", () => {
    expect(viewFromFilesLocation({ path: "/资料盘/docs/sub", explicitDir: false }, listing, "/资料盘/docs")).toEqual({
      cwd: "/资料盘/docs/sub/", locatePath: "", pending: null,
    });
  });

  it("asks for a parent listing when file vs dir is not yet known", () => {
    expect(viewFromFilesLocation({ path: "/资料盘/docs/a.txt", explicitDir: false })).toEqual({
      cwd: "/资料盘/docs/", locatePath: "", pending: "/资料盘/docs/a.txt",
    });
  });

  it("opens the files root", () => {
    expect(viewFromFilesLocation({ path: "/", explicitDir: true })).toEqual({
      cwd: "/", locatePath: "", pending: null,
    });
  });
});
