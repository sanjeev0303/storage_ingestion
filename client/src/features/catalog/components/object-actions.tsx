"use client";

import { useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { ConfirmDialog, Modal } from "@/components/ui/dialog";
import { Field, Input } from "@/components/ui/field";
import { SubmitButton } from "@/components/ui/submit-button";
import { notify } from "@/components/ui/toast";
import { describeError, isApiError } from "@/lib/api/errors";
import { useDeleteObject, useRename, useSetVersioning } from "../hooks";
import { cleanPath, pathProblem } from "../clean";
import type { CatalogObject } from "../types";

function renameProblem(error: unknown) {
  if (isApiError(error)) {
    if (error.code === "path_exists") return "Another object already uses that path in this dataset. Choose a different path.";
    if (error.code === "path_busy") return "An upload to that path is in progress. Try again when it has finished.";
    if (error.status === 412) return "The object changed while you were editing. It has been reloaded; check the path and try again.";
  }
  return describeError(error);
}

export function RenameButton({ object }: { object: CatalogObject }) {
  const [open, setOpen] = useState(false);
  const [path, setPath] = useState(object.path);
  const [error, setError] = useState<string | null>(null);
  const rename = useRename(object.id);

  const next = cleanPath(path);
  let pathError: string | undefined;
  if (!next) pathError = "Enter the new path.";
  else if (next === object.path) pathError = "Enter a path different from the current one.";
  else pathError = pathProblem(next) ?? undefined;
  const valid = !pathError;

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!valid) return;
    setError(null);
    try {
      await rename.mutateAsync({ revision: object.revision, path: next });
      notify.success("Object renamed");
      setOpen(false);
    } catch (e) {
      setError(renameProblem(e));
    }
  }

  return (
    <>
      <Button
        onClick={() => {
          setPath(object.path);
          setError(null);
          setOpen(true);
        }}
      >
        Rename
      </Button>
      <Modal
        open={open}
        onOpenChange={setOpen}
        title="Rename object"
        description="Changes the path only. The dataset stays the same, no bytes move and the version history is kept."
      >
        <form onSubmit={submit} className="flex flex-col gap-4">
          <Field label="New path" required error={pathError ?? error ?? undefined}>
            {(props) => (
              <Input
                {...props}
                value={path}
                onChange={(e) => {
                  setPath(e.target.value);
                  setError(null);
                }}
                maxLength={1024}
                autoComplete="off"
              />
            )}
          </Field>
          <div className="flex justify-end gap-2">
            <Button onClick={() => setOpen(false)}>Cancel</Button>
            <SubmitButton valid={valid} loading={rename.isPending} hint={pathError}>
              Rename object
            </SubmitButton>
          </div>
        </form>
      </Modal>
    </>
  );
}

export function VersioningToggle({ object }: { object: CatalogObject }) {
  const [confirm, setConfirm] = useState(false);
  const toggle = useSetVersioning(object.id);
  const enabled = object.versioning === "ENABLED";

  async function change(versioning: "ENABLED" | "DISABLED") {
    await toggle.mutateAsync({ revision: object.revision, versioning });
    notify.success(`Versioning ${versioning === "ENABLED" ? "enabled" : "disabled"}`);
  }

  return (
    <>
      <Button
        loading={toggle.isPending}
        onClick={() => (enabled ? setConfirm(true) : change("ENABLED").catch((e) => notify.error(e)))}
      >
        {enabled ? "Disable versioning" : "Enable versioning"}
      </Button>
      <ConfirmDialog
        open={confirm}
        onOpenChange={setConfirm}
        title="Disable versioning"
        description="From now on a new upload replaces the current version instead of adding to the history. The replaced version is handed to deletion; retention rules or a hold can keep its bytes for longer."
        confirmLabel="Disable versioning"
        onConfirm={() => change("DISABLED")}
      />
    </>
  );
}

export function DeleteObjectButton({ object }: { object: CatalogObject }) {
  const [open, setOpen] = useState(false);
  const remove = useDeleteObject(object.id);

  return (
    <>
      <Button variant="accent" onClick={() => setOpen(true)}>
        Delete object
      </Button>
      <ConfirmDialog
        open={open}
        onOpenChange={setOpen}
        title="Delete object"
        confirmText={object.path}
        confirmLabel="Delete object"
        description={
          <div className="flex flex-col gap-2">
            <p>
              Every published version is soft-deleted and the object leaves the default listing. Each version is
              restorable until its purge time (7 days by default, set by the organization).
            </p>
            <p>
              Retention rules or a legal hold may keep bytes for longer than you request. Versions still being
              uploaded or processed are not touched and will publish into this path later. The path and its history
              stay reserved; the next upload to it becomes the next version.
            </p>
          </div>
        }
        onConfirm={async () => {
          const result = await remove.mutateAsync();
          const waiting = result.versions_in_processing;
          notify.success(
            waiting > 0
              ? `Object deleted. ${waiting} version${waiting === 1 ? " is" : "s are"} still processing and will publish later.`
              : "Object deleted. Its versions are restorable until their purge time.",
          );
        }}
      />
    </>
  );
}
