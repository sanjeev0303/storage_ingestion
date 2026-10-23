import { formatBytes } from "@/lib/utils/format";
import type { Capabilities } from "../types";

const yesNo = (value: boolean) => (value ? "Yes" : "No");

export const CAPABILITY_ROWS: { label: string; value: (c: Capabilities) => string }[] = [
  { label: "Direct upload with a signed link", value: (c) => yesNo(c.presigned_upload) },
  { label: "Largest single upload", value: (c) => formatBytes(c.max_single_upload_bytes) },
  {
    label: "Multipart upload",
    value: (c) => (c.multipart ? `Yes${c.multipart_kind ? ` (${c.multipart_kind})` : ""}` : "No"),
  },
  { label: "Smallest multipart part", value: (c) => (c.min_part_bytes ? formatBytes(c.min_part_bytes) : "No minimum") },
  { label: "Most multipart parts", value: (c) => (c.max_parts ? String(c.max_parts) : "Not stated") },
  { label: "Copy inside the provider", value: (c) => yesNo(c.server_side_copy) },
  { label: "Range reads", value: (c) => yesNo(c.range_reads) },
  { label: "Conditional reads", value: (c) => yesNo(c.conditional_reads) },
  { label: "Signed download links", value: (c) => yesNo(c.presigned_download) },
  {
    label: "Provider versioning",
    value: (c) => (c.provider_versioning ? "Yes" : c.versioning_status ? c.versioning_status : "No"),
  },
  { label: "Checksums offered", value: (c) => (c.checksum_algorithms.length ? c.checksum_algorithms.join(", ") : "None") },
];
