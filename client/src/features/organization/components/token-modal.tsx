"use client";

import type { ReactNode } from "react";
import { Button } from "@/components/ui/button";
import { CopyButton } from "@/components/ui/copy-button";
import { Modal } from "@/components/ui/dialog";
import { Time } from "@/components/ui/time";

type Props = {
  title: string;
  // Closing clears the token; it cannot be read again.
  token: string | null;
  expiresAt?: string;
  children?: ReactNode;
  onClose: () => void;
};

export function TokenModal({ title, token, expiresAt, children, onClose }: Props) {
  return (
    <Modal
      open={token !== null}
      onOpenChange={(open) => !open && onClose()}
      title={title}
      description="Copy it now. It is shown once and the server cannot show it again."
      footer={<Button variant="primary" onClick={onClose}>I have copied it</Button>}
    >
      <div className="flex flex-col gap-4">
        <p className="mono-data neo-in-sm p-3 text-sm break-all select-all">{token}</p>
        <div className="flex flex-wrap items-center gap-3">
          <CopyButton value={token ?? ""} label="Copy token" />
          {expiresAt && (
            <span className="text-sm text-muted">
              Expires <Time value={expiresAt} />
            </span>
          )}
        </div>
        {children}
      </div>
    </Modal>
  );
}
