import { screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { renderWithProviders } from "@/test/render";
import { newDraft } from "../draft";
import { FileUpload } from "../engine/file-upload";
import type { UploadSnapshot } from "../types";
import { QueueTable } from "./queue-table";

function renderRow(change: Partial<UploadSnapshot>) {
  const task = new FileUpload(new File(["x"], "big.bin"), { file_name: "big.bin", content_type: "application/octet-stream" });
  const entry = { id: task.getSnapshot().id, task, draft: newDraft("platform") };
  const snap = { ...task.getSnapshot(), uploadId: "mp1", mode: "multipart" as const, ...change };
  renderWithProviders(
    <QueueTable
      entries={[entry]}
      snapshots={[snap]}
      errors={new Map()}
      connections={[]}
      defaultConnectionId="platform"
      onDraft={() => undefined}
      onRemove={() => undefined}
      waitingIds={[]}
    />,
  );
}

describe("queue row actions", () => {
  it("offers Check again for an upload whose checks stopped while it was finishing", () => {
    renderRow({ state: "finalizing", pollingStopped: true, completionSent: true });
    expect(screen.getByRole("button", { name: "Check again" })).toBeInTheDocument();
  });

  it("offers Cancel while finishing only until the completion is sent", () => {
    renderRow({ state: "finalizing" });
    expect(screen.getByRole("button", { name: "Cancel" })).toBeInTheDocument();
  });

  it("hides Cancel once the completion is sent, because the server may already publish the file", () => {
    renderRow({ state: "finalizing", completionSent: true });
    expect(screen.queryByRole("button", { name: "Cancel" })).not.toBeInTheDocument();
  });
});
