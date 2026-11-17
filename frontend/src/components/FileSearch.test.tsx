import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, waitFor, cleanup, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import FileSearch from "./FileSearch";
import { searchLocateTarget } from "../lib/searchLocate";
import type { FileItem } from "../lib/api";

function item(partial: Partial<FileItem> & Pick<FileItem, "path" | "is_dir">): FileItem {
  return {
    id: partial.id ?? 0,
    path: partial.path,
    size: partial.size ?? 0,
    status: partial.status ?? "ready",
    created_at: partial.created_at ?? "",
    modified: partial.modified ?? "",
    is_dir: partial.is_dir,
    owner: partial.owner ?? "",
    mount: partial.mount,
    protected: partial.protected,
  };
}

afterEach(() => {
  cleanup();
});

describe("searchLocateTarget", () => {
  it("enters a directory immediately", () => {
    expect(searchLocateTarget(item({ path: "/资料盘/报告归档", is_dir: true }))).toEqual({
      cwd: "/资料盘/报告归档/",
      highlightPath: "",
    });
  });

  it("opens the parent folder and highlights a file", () => {
    expect(searchLocateTarget(item({ path: "/资料盘/docs/年度报告.pdf", is_dir: false }))).toEqual({
      cwd: "/资料盘/docs/",
      highlightPath: "/资料盘/docs/年度报告.pdf",
    });
  });
});

describe("FileSearch UI", () => {
  it("shows files and directories, then jumps on click", async () => {
    const user = userEvent.setup();
    const hits: FileItem[] = [
      item({ id: 1, path: "/资料盘/docs/年度报告.pdf", is_dir: false }),
      item({ id: 2, path: "/资料盘/报告归档", is_dir: true }),
    ];
    const search = vi.fn().mockResolvedValue(hits);
    const onLocate = vi.fn();
    render(<FileSearch onLocate={onLocate} search={search} debounceMs={0} />);

    const input = screen.getByTestId("file-search-input");
    await user.type(input, "报告");

    await waitFor(() => expect(search).toHaveBeenCalledWith("报告"));
    const menu = await screen.findByTestId("file-search-menu");
    const fileHit = within(menu).getByRole("option", { name: /年度报告\.pdf/ });
    const dirHit = within(menu).getByRole("option", { name: /报告归档/ });
    expect(fileHit).toBeInTheDocument();
    expect(dirHit).toBeInTheDocument();
    expect(within(dirHit).getByText("文件夹")).toBeInTheDocument();
    expect(within(fileHit).getByText("文件")).toBeInTheDocument();
    expect(fileHit.querySelector("mark.search-mark")?.textContent).toBe("报告");

    await user.click(fileHit);
    expect(onLocate).toHaveBeenCalledTimes(1);
    expect(onLocate.mock.calls[0][0].path).toBe("/资料盘/docs/年度报告.pdf");
  });

  it("jumps to the active row on Enter", async () => {
    const user = userEvent.setup();
    const hits: FileItem[] = [
      item({ id: 1, path: "/资料盘/a.pdf", is_dir: false }),
      item({ id: 2, path: "/资料盘/报告归档", is_dir: true }),
    ];
    const search = vi.fn().mockResolvedValue(hits);
    const onLocate = vi.fn();
    render(<FileSearch onLocate={onLocate} search={search} debounceMs={0} />);

    const input = screen.getByTestId("file-search-input");
    await user.type(input, "报");
    await screen.findByTestId("file-search-menu");

    await user.keyboard("{ArrowDown}{Enter}");
    expect(onLocate).toHaveBeenCalledTimes(1);
    expect(onLocate.mock.calls[0][0].path).toBe("/资料盘/报告归档");
  });

  it("shows an empty state when nothing matches", async () => {
    const user = userEvent.setup();
    const search = vi.fn().mockResolvedValue([]);
    render(<FileSearch onLocate={() => {}} search={search} debounceMs={0} />);
    await user.type(screen.getByTestId("file-search-input"), "zzzz");
    expect(await screen.findByText("未找到匹配的文件或目录")).toBeInTheDocument();
  });
});
