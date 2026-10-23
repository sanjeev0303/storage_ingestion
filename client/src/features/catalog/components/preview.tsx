"use client";

import { useEffect, useState } from "react";
import { Button } from "@/components/ui/button";
import { describeError } from "@/lib/api/errors";
import { fetchContent } from "../api";
import { MAX_IMAGE_BYTES, MAX_TEXT_BYTES, planPreview } from "../preview";
import type { AssetStatus } from "../types";

type Loaded = { kind: "image"; url: string } | { kind: "text"; text: string };

export function Preview({
  assetId,
  contentType,
  sizeBytes,
  status,
}: {
  assetId: string;
  contentType: string;
  sizeBytes: number;
  status: AssetStatus;
}) {
  const plan = planPreview(contentType, sizeBytes, status);
  const [loaded, setLoaded] = useState<Loaded | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (loaded?.kind !== "image") return;
    const url = loaded.url;
    return () => URL.revokeObjectURL(url);
  }, [loaded]);

  if (plan.kind === "none") {
    return <p className="text-sm text-muted">{plan.reason}</p>;
  }

  async function load() {
    if (plan.kind === "none") return;
    setLoading(true);
    setError(null);
    try {
      const res = await fetchContent(assetId);
      const blob = await res.blob();
      const limit = plan.kind === "image" ? MAX_IMAGE_BYTES : MAX_TEXT_BYTES;
      if (blob.size > limit) {
        setError("The file is larger than the preview limit. Download it instead.");
        return;
      }
      if (plan.kind === "image") {
        setLoaded({ kind: "image", url: URL.createObjectURL(blob) });
      } else {
        setLoaded({ kind: "text", text: await blob.text() });
      }
    } catch (e) {
      setError(describeError(e));
    } finally {
      setLoading(false);
    }
  }

  if (!loaded) {
    return (
      <div className="flex flex-col items-start gap-2">
        <Button onClick={load} loading={loading}>
          Show preview
        </Button>
        {error && (
          <p role="alert" className="text-sm font-medium">
            {error}
          </p>
        )}
      </div>
    );
  }

  if (loaded.kind === "image") {
    // eslint-disable-next-line @next/next/no-img-element -- a blob URL, not a remote image
    return <img src={loaded.url} alt="Preview of the selected version" className="neo-in-sm max-h-96 max-w-full object-contain p-2" />;
  }

  return (
    <pre
      data-lenis-prevent
      tabIndex={0}
      aria-label="File preview"
      className="neo-in-sm mono-data max-h-96 overflow-auto p-3 whitespace-pre-wrap"
    >
      {loaded.text}
    </pre>
  );
}
