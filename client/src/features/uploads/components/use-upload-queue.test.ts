import { act, renderHook } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { FileUpload } from "../engine/file-upload";
import { useUploadQueue } from "./use-upload-queue";

const context = { connections: [], defaultConnectionId: "platform" };

function files(count: number) {
  return Array.from({ length: count }, (_, i) => new File(["x"], `file-${i}.txt`, { type: "text/plain" }));
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("upload queue", () => {
  it("hands each file to the queue once when Start is pressed twice", async () => {
    const run = vi.spyOn(FileUpload.prototype, "run").mockImplementation(() => new Promise(() => undefined));
    const { result } = renderHook(() => useUploadQueue(context));
    act(() => result.current.addFiles(files(5)));

    act(() => {
      result.current.start();
      result.current.start();
    });
    await act(async () => undefined);

    // Four at once; the fifth waits for a slot, and is listed once.
    expect(run).toHaveBeenCalledTimes(4);
    expect(result.current.waitingIds).toHaveLength(1);
  });

  it("cancels a removed file, so a queue already running cannot start it", () => {
    const cancel = vi.spyOn(FileUpload.prototype, "cancel");
    const { result } = renderHook(() => useUploadQueue(context));
    act(() => result.current.addFiles(files(1)));
    const [entry] = result.current.entries;

    act(() => result.current.remove(entry.id));

    expect(cancel).toHaveBeenCalledTimes(1);
    expect(entry.task.getSnapshot().state).toBe("cancelled");
    expect(result.current.entries).toHaveLength(0);
  });

  it("stops its uploads when the page goes away", () => {
    const { result, unmount } = renderHook(() => useUploadQueue(context));
    act(() => result.current.addFiles(files(2)));
    const tasks = result.current.entries.map((entry) => entry.task);
    const stops = tasks.map((task) => vi.spyOn(task, "stop"));

    unmount();

    for (const stop of stops) expect(stop).toHaveBeenCalledTimes(1);
  });
});
