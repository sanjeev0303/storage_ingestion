// The homepage's words. Every claim is something the backend does (see
// server/docs); example values are labelled as examples where a visitor could
// take them for real results. No customers, logos, numbers of ours or prices.

export const nav = [
  { href: "#accession", label: "Checks" },
  { href: "#map", label: "How it works" },
  { href: "#fixes", label: "What it fixes" },
  { href: "#features", label: "Features" },
] as const;

export const hero = {
  title: "Storage that checks first.",
  sentence:
    "Every upload is hashed, scanned and read for what its type can hide before anyone can open it. Every report says what was checked, what was sampled and what never ran.",
  action: "Open the console",
};

export type Outcome = "passed" | "warning" | "failed" | "not_assessed";

export type Sample = {
  id: string;
  file: string;
  size: string;
  accession: string;
  sha: string;
  check: string;
  code: string;
  finding: string;
  released: "released" | "flagged" | "rejected";
};

// Example uploads for the accession slip. Hashes and numbers are examples.
export const slip = {
  title: "Accession slip, example",
  pick: "Pick an upload",
  samples: [
    {
      id: "model",
      file: "classifier.pt",
      size: "48.2 MiB",
      accession: "ACC-0412",
      sha: "9f2c7a0e…e41b",
      check: "Model files",
      code: "model.unsafe_pickle",
      finding: "Loading it would call os.system. The pickle is walked, never run.",
      released: "rejected",
    },
    {
      id: "archive",
      file: "deploy.tar.gz",
      size: "3.1 MiB",
      accession: "ACC-0413",
      sha: "1b84d0c2…77a3",
      check: "Secrets",
      code: "secret.aws_access_key",
      finding: "An AWS key in deploy/.env inside the archive. Its value is never copied.",
      released: "flagged",
    },
    {
      id: "image",
      file: "scan.png",
      size: "812 KiB",
      accession: "ACC-0414",
      sha: "c05e19a4…3d20",
      check: "Images",
      code: "image.png_profile_text",
      finding: "A text chunk named profile, which ImageMagick reads as a file to embed.",
      released: "rejected",
    },
    {
      id: "reads",
      file: "reads.fastq.gz",
      size: "1.7 GiB",
      accession: "ACC-0415",
      sha: "5c70a2f9…b8d1",
      check: "Formats",
      code: "",
      finding: "Every record read to the end: header, sequence, quality.",
      released: "released",
    },
  ] satisfies Sample[],
  rows: { sum: "Size and SHA-256", content: "Content checks", scan: "Malware scan", label: "Labels and meaning" },
  released: { released: "Released", flagged: "Released, flagged", rejected: "Rejected" },
  notTested: "Not tested by any upload check: whether the data is correct or lawful to use.",
};

export const depth = {
  heading: (read: number, total: number) => `Read ${read} of ${total}`,
  legend: "How deep to look",
  total: 48,
  modes: [
    {
      id: "metadata",
      label: "Metadata only",
      read: 0,
      text: "Reads catalog records and never opens content. Fast, and it cannot tell you the stored bytes are intact.",
    },
    {
      id: "sampled",
      label: "Sampled",
      read: 8,
      text: "Reads a fixed sample of members and says how many. A passing content check counts as partial.",
    },
    {
      id: "full",
      label: "Full integrity",
      read: 48,
      text: "Reads every member. The only mode that covers all of them, and the slowest.",
    },
  ],
} as const;

export const stages = {
  heading: "From bytes to a listed object",
  steps: [
    { name: "Upload", inside: "Parts go straight to storage", clause: "First, the browser hashes each file and sends it in parts." },
    { name: "Verify", inside: "Size and SHA-256 match", clause: "Then the object waits in quarantine until both match." },
    { name: "Inspect", inside: "Content checks, then scan", clause: "Next, its contents are read and scanned for malware." },
    { name: "Promote", inside: "Listed in the catalog", clause: "Only then is it readable, as one version of an object." },
    { name: "Pin", inside: "A snapshot of versions", clause: "Last, a snapshot pins exact versions and a manifest hash." },
  ],
};

export const board = {
  heading: "Everything after the label",
  tiles: [
    { id: "snapshots", name: "Snapshots and branches", line: "Pin exact versions; branch, edit and merge drafts." },
    { id: "holds", name: "Holds, locks and erasure", line: "Holds outrank deletes; every purge is certified." },
    { id: "adopt", name: "Adopt in place", line: "S3 Inventory, ASC MHL, BagIt or STAC, read where it sits." },
    { id: "copies", name: "A second copy", line: "Replicate, scrub and repair from another provider." },
    { id: "stream", name: "Train from a snapshot", line: "Shards to PyTorch, TensorFlow, JAX, Ray or Hugging Face." },
    { id: "lineage", name: "Lineage", line: "Imports, reports, training runs and models, linked." },
    { id: "audit", name: "Evidence", line: "A hash chain of every action, anchored outside the database." },
    { id: "edge", name: "Edge agent", line: "Instruments upload from a journal, within their windows." },
  ],
} as const;

export const readiness = {
  heading: "Nine dimensions, no score",
  intro: "A readiness assessment judges a snapshot on nine dimensions. Each states its own status and how much was examined. There is no overall score.",
  dimensions: [
    { name: "Completeness", text: "Labels, required metadata and every referenced object are present.", status: "Pass", coverage: "Full" },
    { name: "Integrity", text: "Recorded sizes and checksums agree, and content checks passed where they ran.", status: "Warn", coverage: "Sampled" },
    { name: "Security", text: "Malware scan evidence exists and is clean for each member.", status: "Pass", coverage: "Full" },
    { name: "Consistency", text: "Media types, paths and split assignments follow the profile.", status: "Pass", coverage: "Full" },
    { name: "Duplication", text: "Duplicate references and verified duplicate content are reported.", status: "Warn", coverage: "Full" },
    { name: "Distribution", text: "Labels, splits and file sizes are spread within the limits you set.", status: "Not checked", coverage: "None" },
    { name: "Reproducibility", text: "The snapshot is finalized and its pinned versions are still available.", status: "Pass", coverage: "Full" },
    { name: "Governance", text: "Required governance metadata is attached to every member.", status: "Fail", coverage: "Full" },
    { name: "Accessibility", text: "Every pinned version can still be read by the people who need it.", status: "Pass", coverage: "Full" },
  ],
  foot: "Statuses shown are an example report.",
} as const;

export const storage = {
  heading: "Your storage, or ours",
  sentence: "Credentials reach the server once, are sealed there and never come back.",
  lifecycle: "A new connection takes no uploads until a test passes. Objects already stored never move between connections.",
  labelTitle: "What a connection needs",
  providers: [
    { name: "Platform-managed", needs: "Nothing: the platform's own buckets." },
    { name: "Amazon S3", needs: "An access key, or a role the platform assumes." },
    { name: "Cloudflare R2", needs: "An access key for the account's endpoint." },
    { name: "Google Cloud Storage", needs: "A service account key." },
    { name: "Azure Blob Storage", needs: "An account key or a container SAS token." },
    { name: "S3-compatible", needs: "An access key and the service's endpoint." },
  ],
} as const;

export const start = {
  word: "Start",
  sentence: "Add a storage connection, upload one file and read its accession slip.",
  href: "/register",
};

export const footer = {
  columns: [
    { title: "Product", links: nav },
    {
      title: "Account",
      links: [
        { href: "/login", label: "Sign in" },
        { href: "/register", label: "Create account" },
      ],
    },
  ],
  line: "Every byte checked before it is readable.",
};

// The platform as one workflow: where data arrives, the gate it passes, where
// it lives and is used, and the three rails that run beside it.
export const map = {
  heading: "The whole platform, one path",
  intro:
    "Every object takes the same path: it arrives, passes the gate, is catalogued, pinned into a snapshot, reported on and used. Governance, a second copy and evidence run beside it the whole way. From bytes to a listed object, earlier on this page, zooms in on the first four: upload is Arrive, verify and inspect are the gate, promote is the catalog, pin is the snapshot.",
  pick: "Explore a stage",
  stages: [
    {
      id: "arrive",
      name: "Arrive",
      sub: "upload or adopt",
      fixes: "Large uploads that fail halfway, million-file imports one bad line can sink, and data you would otherwise upload again.",
      features: ["Multipart uploads with parts retried alone", "JSON Lines manifests with per-file SHA-256", "Adopt in place from S3 Inventory, ASC MHL, BagIt or STAC", "Edge agent for instruments", "Delta uploads that send only changed chunks"],
      chapter: "#fix-adopt",
    },
    {
      id: "gate",
      name: "Quarantine gate",
      sub: "hash, check, scan",
      fixes: "Bytes that become readable before anyone checked them, and threats a signature scanner never sees.",
      features: ["Size and SHA-256 must match", "Content checks for model files, secrets, documents, images and formats", "Malware scan, and a re-scan when signatures change"],
      chapter: "#fix-inspect",
    },
    {
      id: "catalog",
      name: "Catalog",
      sub: "every version",
      fixes: "Overwrites that lose history, and the same bytes stored twice.",
      features: ["Logical objects with versions", "Whole-file and chunk-level dedup", "Tiering that skips pinned and held objects"],
      chapter: "#features",
    },
    {
      id: "snapshot",
      name: "Snapshot",
      sub: "versions pinned",
      fixes: "A dataset that changes under a model after it was checked.",
      features: ["Snapshots pin exact versions and a manifest hash", "Drafts branch and merge", "Publishing with a DOI and licence"],
      chapter: "#features",
    },
    {
      id: "report",
      name: "Readiness",
      sub: "what ran, what not",
      fixes: "Reports that call a dataset ready when half the checks never ran.",
      features: ["Validation at metadata, sampled or full depth", "Nine readiness dimensions, no score", "Quality, drift and leakage checks"],
      chapter: "#readiness",
    },
    {
      id: "use",
      name: "Use",
      sub: "train and read",
      fixes: "GPUs waiting on slow reads of files nobody verified.",
      features: ["Shards streamed to PyTorch, TensorFlow, Hugging Face, JAX and Ray", "SQL over CSV, JSON and Parquet in the browser", "Python, Go and JS SDKs, Git LFS and DICOMweb"],
      chapter: "#fix-stream",
    },
    {
      id: "govern",
      name: "Governance",
      sub: "holds, locks, erasure",
      fixes: "A delete that outruns a legal hold, and consent withdrawn but still in a snapshot.",
      features: ["Retention and legal holds enforced by Object Lock", "Opt-in COMPLIANCE mode with guardrails", "Erasure by subject with a signed certificate", "Region pinning and your own keys"],
      chapter: "#fix-lock",
    },
    {
      id: "copy",
      name: "Second copy",
      sub: "another provider",
      fixes: "One provider, one outage, one damaged chunk and the data is gone.",
      features: ["Replication to a second provider", "Scrubs and repair from a verified replica", "Read failover and restore drills"],
      chapter: "#fix-copies",
    },
    {
      id: "evidence",
      name: "Evidence",
      sub: "audit chain, lineage",
      fixes: "History an admin could rewrite, and no answer to which data trained a model.",
      features: ["A hash chain of every action, reads included", "Chain head anchored outside the database", "Lineage from imports to models, SIEM export"],
      chapter: "#fix-prove",
    },
  ],
  more: "Read how it fixes this",
  diagramLabel:
    "Data arrives, passes the quarantine gate, enters the catalog, is pinned into a snapshot, gets a readiness report and is used. Governance, a second copy and evidence run beside the catalog and the gate.",
} as const;

// Six problems, each with the fix and a working diagram of it.
export const fixes = {
  heading: "Six problems it fixes",
  intro: "Each one is a real way data goes wrong. Each diagram is the fix, working: pick an option and watch the path change.",
  examples: "Names and values in the diagram are examples.",
  chapters: [
    {
      id: "inspect",
      problem: "Bad files pass the virus scan",
      fix: "A signature scanner misses a pickle that runs code, a key in a config file, JavaScript in a PDF or a PNG that attacks ImageMagick. Every upload is opened and read for what its type can hide, and your policy decides whether a finding flags it or rejects it.",
    },
    {
      id: "adopt",
      problem: "Uploading what you already hold",
      fix: "Copying petabytes in again costs time and money and breaks the hashes your tools already wrote. The platform reads the list you have, checks each object once where it sits against its digests, and publishes it in place.",
    },
    {
      id: "stream",
      problem: "Training reads unverified files",
      fix: "Per-object reads leave GPUs idle, and nothing checks the bytes on the way in. Snapshots stream as shards that keep samples whole, and every file is checked against the SHA-256 the snapshot pinned.",
    },
    {
      id: "lock",
      problem: "A delete outruns a legal hold",
      fix: "Courts sanction lost evidence and regulators fine early deletion. Every delete, erasure included, must clear holds, retention and the bucket's own lock in order, and every purge ends in a signed certificate.",
    },
    {
      id: "copies",
      problem: "One copy, one bad day",
      fix: "A provider outage, a deleted account or one flipped bit can take the only copy. Datasets replicate to a second provider, scrubs repair damaged chunks from a verified replica, and reads fail over.",
    },
    {
      id: "prove",
      problem: "History nobody can prove",
      fix: "A log that an admin can edit proves nothing. Every action joins a hash chain whose signed head lives outside the database, so a changed record shows at once.",
    },
  ],
} as const;

// Every feature, the problem it fixes, and how, grouped by the map's stages.
// All of them ship today.
export const index = {
  heading: "Every feature, the problem it fixes",
  filter: "Show stage",
  all: "All",
  groups: [
    {
      name: "Arrive",
      rows: [
        { feature: "Multipart uploads", problem: "Large files fail halfway.", fix: "Parts go straight to storage, four at a time; a failed part is retried alone." },
        { feature: "Manifest ingestion", problem: "One bad line sinks a huge import.", fix: "JSON Lines with per-file SHA-256; a bad line is reported, the rest go on." },
        { feature: "Adopt in place", problem: "Uploading data you already hold.", fix: "Objects are read once where they sit and published in place." },
        { feature: "Edge agent", problem: "Instruments stuck on Uploading.", fix: "Watched folders, a durable journal, bandwidth windows, delete only after confirmation." },
        { feature: "Delta uploads", problem: "Resending a file for a small change.", fix: "Only changed chunks are sent; the rest is reused from a snapshot you can read." },
      ],
    },
    {
      name: "Quarantine gate",
      rows: [
        { feature: "Checksum verification", problem: "Unchecked bytes become readable.", fix: "Size and SHA-256 must match before anything is listed." },
        { feature: "Content checks", problem: "Threats a virus scan cannot see.", fix: "Model files, secrets, documents, images and formats read and judged by your policy." },
        { feature: "Re-scan on new signatures", problem: "Clean yesterday, malware today.", fix: "Published files are scanned again and blocked if infected." },
      ],
    },
    {
      name: "Catalog",
      rows: [
        { feature: "Catalog and versions", problem: "Overwrites lose history.", fix: "Every version keeps its size, SHA-256 and status." },
        { feature: "Chunk storage", problem: "Near-identical files stored twice.", fix: "Shared chunks are stored once and checked on every read." },
        { feature: "Tiering and costs", problem: "Cold data on hot storage, surprise bills.", fix: "Tier rules that skip pinned and held objects, with estimates and budgets." },
      ],
    },
    {
      name: "Snapshot",
      rows: [
        { feature: "Snapshots and branches", problem: "Datasets change under a model.", fix: "Snapshots pin exact versions; drafts branch and merge." },
        { feature: "Publishing", problem: "No stable way to cite a version.", fix: "Publish a snapshot with its licence and a DOI." },
      ],
    },
    {
      name: "Readiness",
      rows: [
        { feature: "Validation and readiness", problem: "Passed hides what never ran.", fix: "Nine dimensions, each with its coverage; no invented score." },
        { feature: "Quality and drift", problem: "Silent shifts between versions.", fix: "Column profiles, drift against the parent, and train and eval overlap checks." },
      ],
    },
    {
      name: "Use",
      rows: [
        { feature: "Streaming to training", problem: "GPUs wait on slow reads.", fix: "Shards to PyTorch, TensorFlow, Hugging Face, JAX and Ray, checked on read." },
        { feature: "SQL in the browser", problem: "Downloading a file just to look.", fix: "Query CSV, JSON and Parquet in the console with DuckDB." },
        { feature: "Connectors and SDKs", problem: "Glue code for every tool.", fix: "Python, Go and JS SDKs, Git LFS, DICOMweb, Slack and Teams sharing." },
        { feature: "Webhooks", problem: "Polling for what changed.", fix: "Signed events when objects are published, flagged or blocked from deletion." },
      ],
    },
    {
      name: "Governance",
      rows: [
        { feature: "Retention and legal holds", problem: "A delete breaks a hold.", fix: "Holds by query, retention from a record's own date, enforced by Object Lock." },
        { feature: "COMPLIANCE locks", problem: "Locks an admin can lift.", fix: "Opt-in COMPLIANCE mode, with guardrails and two-person approval." },
        { feature: "Erasure", problem: "Withdrawn consent stays in snapshots.", fix: "Find by subject, successor snapshots, wait on holds, signed certificate." },
        { feature: "Residency and keys", problem: "Data in the wrong region or without your keys.", fix: "Region pinning and customer-managed keys per connection." },
        { feature: "Access control", problem: "Shared keys and silent mass downloads.", fix: "Roles, scoped expiring tokens, link control, read alerts, egress budgets, SSO and SCIM." },
        { feature: "Posture checks", problem: "A bucket set up unsafely.", fix: "Public access, SSE-C, versioning, lifecycle drift and out-of-band writes checked." },
      ],
    },
    {
      name: "Second copy",
      rows: [
        { feature: "Replication and repair", problem: "One provider, one outage.", fix: "Replication, scrubs, repair from a replica, read failover and restore drills." },
      ],
    },
    {
      name: "Evidence",
      rows: [
        { feature: "Audit chain", problem: "Logs an admin can rewrite.", fix: "A hash chain of every action, reads included, anchored outside the database." },
        { feature: "Lineage", problem: "Which data trained this model?", fix: "Imports, versions, reports, training runs and models, linked." },
        { feature: "Evidence exports", problem: "Audits assembled by hand.", fix: "SIEM export, custody reports and an EU AI Act training summary." },
      ],
    },
  ],
} as const;
