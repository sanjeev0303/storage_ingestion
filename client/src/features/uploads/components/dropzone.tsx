"use client";

import { UploadSimple } from "@phosphor-icons/react";
import { useRef, useState } from "react";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils/cn";

export function Dropzone({ onFiles, disabled }: { onFiles: (files: File[]) => void; disabled?: boolean }) {
  const input = useRef<HTMLInputElement>(null);
  const [over, setOver] = useState(false);

  function take(list: FileList | null) {
    const files = list ? Array.from(list) : [];
    if (files.length > 0) onFiles(files);
  }

  return (
    <div
      role="group"
      aria-label="Add files"
      onDragOver={(event) => {
        event.preventDefault();
        if (!disabled) setOver(true);
      }}
      onDragLeave={() => setOver(false)}
      onDrop={(event) => {
        event.preventDefault();
        setOver(false);
        if (!disabled) take(event.dataTransfer.files);
      }}
      className={cn(
        "neo-in flex flex-col items-start gap-4 p-6 transition-colors duration-150 sm:flex-row sm:items-center",
        over && "bg-faint",
      )}
    >
      <UploadSimple aria-hidden className="size-8 shrink-0" />
      <div className="min-w-0 flex-1">
        <p className="text-md font-semibold">Drop files here, or choose them from your computer</p>
        <p className="mt-0.5 text-sm text-muted">
          Files are read in pieces from disk, so large files do not fill the browser&apos;s memory. Folders are not
          supported; select the files inside them.
        </p>
      </div>
      <Button variant="primary" disabled={disabled} onClick={() => input.current?.click()}>
        Choose files
      </Button>
      <input
        ref={input}
        hidden
        type="file"
        multiple
        data-testid="file-input"
        onChange={(event) => {
          take(event.target.files);
          event.target.value = "";
        }}
      />
    </div>
  );
}
