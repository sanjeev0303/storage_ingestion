"use client";

import { DownloadSimple } from "@phosphor-icons/react";
import { useState } from "react";
import { Button, buttonClass } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/dialog";
import { Select } from "@/components/ui/field";
import { Time } from "@/components/ui/time";
import { notify } from "@/components/ui/toast";
import { describeError, isApiError } from "@/lib/api/errors";
import { useDownloadUrl, useRevokeLinks } from "../hooks";
import { downloadHref } from "../preview";
import type { DownloadMode, DownloadUrl } from "../types";

function downloadProblem(error: unknown) {
  if (isApiError(error)) {
    if (error.status === 409) return "This version is not available for delivery right now.";
    if (error.status === 404) return "This version no longer exists.";
    if (error.status === 503) {
      const wait = error.retryAfter ? ` Try again in ${error.retryAfter} seconds.` : " Try again shortly.";
      return `Downloads are temporarily unavailable.${wait}`;
    }
  }
  return describeError(error);
}

export function DownloadControl({
  assetId,
  fileName,
  canDownload,
  canRevoke,
}: {
  assetId: string;
  fileName: string;
  canDownload: boolean;
  canRevoke: boolean;
}) {
  const [mode, setMode] = useState<DownloadMode>("storage");
  // The link lives only in this component's state. It is never logged or stored.
  const [link, setLink] = useState<DownloadUrl | null>(null);
  const [problem, setProblem] = useState<string | null>(null);
  const [confirmRevoke, setConfirmRevoke] = useState(false);
  const create = useDownloadUrl(assetId);
  const revoke = useRevokeLinks(assetId);

  async function prepare() {
    setProblem(null);
    setLink(null);
    try {
      setLink(await create.mutateAsync({ mode, filename: fileName }));
    } catch (e) {
      setProblem(downloadProblem(e));
    }
  }

  return (
    <div className="flex flex-col gap-3">
      {canDownload ? (
        <div className="flex flex-wrap items-end gap-2">
          <label className="flex flex-col gap-1.5 text-sm font-medium">
            Link type
            <Select value={mode} onChange={(e) => setMode(e.target.value as DownloadMode)} className="w-64">
              <option value="storage">Direct from storage (cannot be revoked)</option>
              <option value="token">Through the API (revocable)</option>
            </Select>
          </label>
          <Button onClick={prepare} loading={create.isPending}>
            <DownloadSimple aria-hidden /> Create download link
          </Button>
        </div>
      ) : (
        <p className="text-sm text-muted">Your role cannot download files.</p>
      )}

      {problem && (
        <p role="alert" className="text-base font-medium">
          {problem}
        </p>
      )}

      {link && (
        <div className="neo-in-sm flex flex-col gap-2 p-4">
          <a
            href={downloadHref(link.url, link.delivery)}
            rel="noopener noreferrer"
            className={buttonClass("primary", "self-start")}
          >
            <DownloadSimple aria-hidden /> Download {fileName}
          </a>
          <p className="text-sm text-muted">
            {link.delivery === "token"
              ? "API link. It stops working when it expires or when link-based downloads are revoked."
              : `${link.delivery === "cdn" ? "CDN" : "Storage"} link. ${
                  link.revocable ? "" : "It cannot be revoked and works for anyone who holds it until it expires. "
                }Treat it like a password.`}{" "}
            Expires <Time value={link.expires_at} />.
          </p>
        </div>
      )}

      {canRevoke && (
        <div>
          <Button variant="ghost" onClick={() => setConfirmRevoke(true)}>
            Revoke link-based downloads
          </Button>
          <ConfirmDialog
            open={confirmRevoke}
            onOpenChange={setConfirmRevoke}
            title="Revoke link-based downloads"
            description="Every download link created through the API for this version stops working at once. Direct storage links cannot be revoked: they keep working until they expire."
            confirmLabel="Revoke links"
            onConfirm={async () => {
              const result = await revoke.mutateAsync();
              notify.success(`${result.revoked} link${result.revoked === 1 ? "" : "s"} revoked`);
            }}
          />
        </div>
      )}
    </div>
  );
}
