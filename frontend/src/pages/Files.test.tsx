import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, screen, waitFor, cleanup, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { FileItem, PathNoteIndex } from "../lib/api";
import { filesHashFor, filesLocationFromHash, hashesEqual } from "../lib/hashRoute";

vi.mock("../transfer", () => ({
  useTransfers: () => ({
    clientMode: false,
    transfers: [],
    startUpload: vi.fn(),
    startClientUpload: vi.fn(),
    startDownload: vi.fn(),
  }),
}));

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      listFiles: vi.fn(),
      noteIndex: vi.fn(),
      storage: vi.fn(),
      getNote: vi.fn(),
      searchFiles: vi.fn(),
    },
  };
});

import { api } from "../lib/api";
import Files from "./Files";

const listFiles = api.listFiles as unknown as ReturnType<typeof vi.fn>;
const noteIndex = api.noteIndex as unknown as ReturnType<typeof vi.fn>;
const storage = api.storage as unknown as ReturnType<typeof vi.fn>;
const getNote = api.getNote as unknown as ReturnType<typeof vi.fn>;

function item(partial: Partial<FileItem> & Pick<FileItem, "path" | "is_dir">): FileItem {
  return {
    id: partial.id ?? 0,
    path: partial.path,
    size: partial.size ?? 0,
    status: partial.status ?? "ready",
    created_at: partial.created_at ?? "",
    modified: partial.modified ?? "2024-01-01T00:00:00Z",
    is_dir: partial.is_dir,
    owner: partial.owner ?? "",
    mount: partial.mount,
    protected: partial.protected,
  };
}

function idx(entries: { path: string; body: string }[]): PathNoteIndex {
  return { paths: entries.map((e) => e.path), notes: entries };
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  window.location.hash = "";
});

beforeEach(() => {
  window.location.hash = "";
  storage.mockResolvedValue({ total: 0, used: 0, free: 0 });
  getNote.mockResolvedValue({ path: "", is_dir: false, body: "", exists: false });
  listFiles.mockImplementation(async (dir: string) => {
    if (dir === "/" || dir === "") {
      return [item({ id: 1, path: "/资料盘", is_dir: true, mount: true })];
    }
    return [
      item({ id: 2, path: "/资料盘/short.txt", is_dir: false }),
      item({ id: 3, path: "/资料盘/long.md", is_dir: false }),
      item({ id: 4, path: "/资料盘/pic", is_dir: true }),
    ];
  });
  noteIndex.mockImplementation(async (dir: string) => {
    if (dir === "/资料盘") {
      return idx([
        { path: "/资料盘", body: "当前目录的短备注" },
        { path: "/资料盘/short.txt", body: "这是一份简短备注" },
        { path: "/资料盘/long.md", body: "# 标题\n\n" + "很长的备注内容。".repeat(20) },
        { path: "/资料盘/pic", body: "封面\n\n![示意图](/api/notes/images/12)" },
      ]);
    }
    return idx([]);
  });
});

async function enterMount() {
  render(<Files jobs={{}} />);
  const folder = await screen.findByText("资料盘");
  await userEvent.setup().click(folder);
  await screen.findByText("short.txt");
  await waitFor(() => expect(noteIndex).toHaveBeenCalledWith("/资料盘", undefined));
  await screen.findByText("这是一份简短备注");
}

describe("Files path notes", () => {
  it("renders a short plain note as inline text instead of a 备注 badge", async () => {
    await enterMount();

    const shortRow = screen.getByText("short.txt").closest("tr");
    expect(shortRow).toBeTruthy();
    expect(shortRow).toHaveClass("has-note");
    expect(within(shortRow as HTMLElement).getByText("这是一份简短备注")).toBeInTheDocument();
    const shortHint = within(shortRow as HTMLElement).getByRole("button", { name: "查看备注" });
    expect(shortHint).toHaveClass("note-inline");
    expect(shortHint).not.toHaveClass("note-badge");

    expect(screen.getByText("当前目录的短备注")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "查看当前目录备注" })).toHaveClass("note-inline");
  });

  it("keeps the 备注 badge for image and long markdown notes, without dumping markup into the row", async () => {
    await enterMount();

    const longRow = screen.getByText("long.md").closest("tr");
    expect(longRow).toHaveClass("has-note");
    expect(within(longRow as HTMLElement).getByRole("button", { name: "查看备注" })).toHaveTextContent("备注");
    expect(within(longRow as HTMLElement).queryByText(/# 标题/)).not.toBeInTheDocument();
    expect(within(longRow as HTMLElement).queryByText(/很长的备注内容/)).not.toBeInTheDocument();

    const picRow = screen.getByText("pic").closest("tr");
    expect(picRow).toHaveClass("has-note");
    expect(within(picRow as HTMLElement).getByRole("button", { name: "查看备注" })).toHaveTextContent("备注");
    expect(within(picRow as HTMLElement).queryByText(/\/api\/notes\/images/)).not.toBeInTheDocument();
    expect(within(picRow as HTMLElement).queryByRole("img")).not.toBeInTheDocument();
    expect(within(picRow as HTMLElement).queryByText("封面")).not.toBeInTheDocument();
  });

  it("opens the existing preview when the inline note is clicked, without entering the folder", async () => {
    const user = userEvent.setup();
    getNote.mockResolvedValue({
      path: "/资料盘/pic",
      is_dir: true,
      exists: true,
      body: "封面\n\n![示意图](/api/notes/images/12)",
    });
    // Use the simple note on short.txt for the click — but also verify folder row click doesn't navigate.
    getNote.mockImplementation(async (path: string) => ({
      path,
      is_dir: path === "/资料盘",
      exists: true,
      body: path === "/资料盘" ? "当前目录的短备注" : "这是一份简短备注",
    }));

    await enterMount();
    expect(screen.getByText("short.txt")).toBeInTheDocument();

    const shortBtn = within(screen.getByText("short.txt").closest("tr") as HTMLElement)
      .getByRole("button", { name: "查看备注" });
    await user.click(shortBtn);

    const preview = await screen.findByRole("dialog", { name: "备注预览" });
    expect(within(preview).getByText("这是一份简短备注")).toBeInTheDocument();
    expect(within(preview).getByText("文件备注")).toBeInTheDocument();
    // still in the same folder (did not navigate away)
    expect(screen.getByText("long.md")).toBeInTheDocument();
    expect(screen.getByText("pic")).toBeInTheDocument();
  });
});

describe("Files URL path", () => {
  it("updates the hash when entering a folder or visiting a file", async () => {
    const user = userEvent.setup();
    await enterMount();
    expect(hashesEqual(window.location.hash, filesHashFor("/资料盘", "dir"))).toBe(true);
    expect(filesLocationFromHash(window.location.hash)).toEqual({ path: "/资料盘", explicitDir: false });

    await user.click(screen.getByText("short.txt"));
    expect(filesLocationFromHash(window.location.hash)).toEqual({
      path: "/资料盘/short.txt",
      explicitDir: false,
    });
    expect(screen.getByText("short.txt").closest("tr")).toHaveClass("locate");
    // still in the parent listing
    expect(screen.getByText("long.md")).toBeInTheDocument();
  });

  it("opens a directory hash on load", async () => {
    window.location.hash = filesHashFor("/资料盘", "dir");
    render(<Files jobs={{}} />);
    expect(await screen.findByText("short.txt")).toBeInTheDocument();
    await waitFor(() => expect(noteIndex).toHaveBeenCalledWith("/资料盘", undefined));
    expect(screen.getByText("这是一份简短备注")).toBeInTheDocument();
  });

  it("opens a file hash on load in the parent directory and highlights the file", async () => {
    window.location.hash = filesHashFor("/资料盘/short.txt", "file");
    render(<Files jobs={{}} />);
    const row = await screen.findByText("short.txt");
    await waitFor(() => expect(row.closest("tr")).toHaveClass("locate"));
    expect(screen.getByText("long.md")).toBeInTheDocument();
  });

  it("reacts to browser hash changes without a feedback loop", async () => {
    await enterMount();
    expect(screen.getByText("short.txt")).toBeInTheDocument();

    window.location.hash = "/files";
    await screen.findByText("资料盘");
    await waitFor(() => expect(screen.queryByText("short.txt")).not.toBeInTheDocument());
    expect(filesLocationFromHash(window.location.hash)).toEqual({ path: "/", explicitDir: true });
  });
});
