import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import { ConfirmDialog } from "./dialog";

function dialog(open: boolean) {
  return (
    <ConfirmDialog
      open={open}
      onOpenChange={() => undefined}
      title="Delete it?"
      description="It cannot be undone."
      confirmLabel="Delete"
      confirmText="DELETE"
      onConfirm={() => {
        throw new Error("refused");
      }}
    />
  );
}

describe("ConfirmDialog", () => {
  it("starts clean when reopened after the parent closed it", async () => {
    const { rerender } = render(dialog(true));
    await userEvent.type(screen.getByRole("textbox"), "DELETE");
    await userEvent.click(screen.getByRole("button", { name: "Delete" }));
    expect(await screen.findByRole("alert")).toBeInTheDocument();

    // Closed through `open`, not through the dialog's own buttons.
    rerender(dialog(false));
    rerender(dialog(true));
    expect(screen.getByRole("textbox")).toHaveValue("");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });
});
