import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, screen, waitFor, cleanup } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { PathNotePanel, NoteBadge, NotePreview, PathNoteHint } from "./PathNotePanel";

vi.mock("../lib/api", () => ({
  api: {
    getNote: vi.fn(),
    putNote: vi.fn(),
    deleteNote: vi.fn(),
    uploadNoteImage: vi.fn(),
    noteImageBlob: vi.fn(),
  },
}));

import { api } from "../lib/api";

const getNote = api.getNote as unknown as ReturnType<typeof vi.fn>;
const putNote = api.putNote as unknown as ReturnType<typeof vi.fn>;
const deleteNote = api.deleteNote as unknown as ReturnType<typeof vi.fn>;

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

beforeEach(() => {
  getNote.mockResolvedValue({ path: "/资料盘/a.pdf", is_dir: false, body: "", exists: false });
  putNote.mockImplementation(async (_p: string, _d: boolean, body: string) => ({
    ok: true, exists: !!body.trim(), path: "/资料盘/a.pdf", is_dir: false, body,
  }));
  deleteNote.mockResolvedValue({ ok: true });
});

const target = { path: "/资料盘/a.pdf", isDir: false, name: "a.pdf" };

describe("PathNotePanel", () => {
  it("shows an empty state for a path without a note", async () => {
    render(<PathNotePanel target={target} onClose={() => {}} onChanged={() => {}} />);
    expect(await screen.findByText("还没有备注")).toBeInTheDocument();
    expect(screen.getByText("a.pdf")).toBeInTheDocument();
  });

  it("renders a saved note and keeps typography tidy", async () => {
    getNote.mockResolvedValue({
      path: "/资料盘/a.pdf", is_dir: false, exists: true,
      body: "# 来源\n\n这是一份 **重要** 说明。",
    });
    render(<PathNotePanel target={target} onClose={() => {}} onChanged={() => {}} />);
    const heading = await screen.findByRole("heading", { name: "来源" });
    expect(heading.tagName).toBe("H2");
    expect(screen.getByText("重要").tagName).toBe("STRONG");
    expect(screen.getByRole("button", { name: "编辑" })).toBeInTheDocument();
  });

  it("saves a long note from the editor", async () => {
    const user = userEvent.setup();
    const onChanged = vi.fn();
    render(<PathNotePanel target={target} onClose={() => {}} onChanged={onChanged} />);
    await screen.findByText("还没有备注");
    await user.click(screen.getByRole("button", { name: "开始写" }));
    const box = screen.getByPlaceholderText(/写下备注/);
    const long = "第一段说明。\n\n" + "很长的备注内容。".repeat(40);
    await user.click(box);
    await user.paste(long);
    await user.click(screen.getByRole("button", { name: "完成" }));
    await waitFor(() => expect(putNote.mock.calls.some((c: any[]) => String(c[2]).includes("第一段说明") && String(c[2]).length > 80)).toBe(true));
    expect(onChanged).toHaveBeenCalled();
  });
});

describe("NoteBadge", () => {
  it("shows a visible 备注 label and previews on click", async () => {
    const user = userEvent.setup();
    const onClick = vi.fn();
    render(<NoteBadge onClick={onClick} />);
    const btn = screen.getByRole("button", { name: "查看备注" });
    expect(btn).toHaveTextContent("备注");
    await user.click(btn);
    expect(onClick).toHaveBeenCalledTimes(1);
  });
});

describe("NotePreview", () => {
  it("loads and shows the note body", async () => {
    getNote.mockResolvedValue({
      path: "/资料盘/a.pdf", is_dir: false, exists: true,
      body: "这是预览内容\n\n第二段",
    });
    const onEdit = vi.fn();
    render(
      <NotePreview
        target={target}
        x={40}
        y={80}
        onClose={() => {}}
        onEdit={onEdit}
      />,
    );
    expect(await screen.findByText("这是预览内容")).toBeInTheDocument();
    expect(screen.getByText("第二段")).toBeInTheDocument();
    expect(screen.getByText("文件备注")).toBeInTheDocument();
    await userEvent.setup().click(screen.getByRole("button", { name: "编辑" }));
    expect(onEdit).toHaveBeenCalled();
  });
});

describe("PathNoteHint", () => {
  it("renders short plain text inline and opens the preview callback on click", async () => {
    const user = userEvent.setup();
    const onClick = vi.fn();
    render(<PathNoteHint body="这是一份简短备注" onClick={onClick} />);
    const btn = screen.getByRole("button", { name: "查看备注" });
    expect(btn).toHaveClass("note-inline");
    expect(btn).toHaveTextContent("这是一份简短备注");
    expect(btn).not.toHaveClass("note-badge");
    await user.click(btn);
    expect(onClick).toHaveBeenCalledTimes(1);
  });

  it("keeps the 备注 badge for notes with images or long markdown", () => {
    const { rerender } = render(
      <PathNoteHint body={"封面\n\n![示意图](/api/notes/images/12)"} onClick={() => {}} />,
    );
    expect(screen.getByRole("button", { name: "查看备注" })).toHaveClass("note-badge");
    expect(screen.getByRole("button", { name: "查看备注" })).toHaveTextContent("备注");
    expect(screen.queryByRole("img")).not.toBeInTheDocument();
    expect(screen.queryByText(/\/api\/notes\/images/)).not.toBeInTheDocument();

    rerender(<PathNoteHint body={"# 标题\n\n" + "很长的备注内容。".repeat(20)} onClick={() => {}} />);
    expect(screen.getByRole("button", { name: "查看备注" })).toHaveClass("note-badge");
    expect(screen.queryByText("# 标题")).not.toBeInTheDocument();
  });
});
