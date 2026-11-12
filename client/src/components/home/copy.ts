// The homepage's words. Every claim is something the backend does (server/docs,
// PRODUCT.md). File names and hashes in the figures are examples; the readiness
// table says so in its title. No customers, benchmarks or prices.

export const nav = [
  { href: "#problem", label: "Problem" },
  { href: "#method", label: "Method" },
  { href: "#features", label: "Features" },
  { href: "#integrations", label: "Integrations" },
] as const;

export const actions = {
  primary: { href: "/register", label: "Create an account" },
  signIn: { href: "/login", label: "Sign in" },
};

export const hero = {
  title: "Verified dataset storage for ML teams",
  lead: "Orbidune hashes and scans every upload before anyone can read it, pins exact file versions in snapshots, and tells you which checks ran and which did not.",
  secondary: { href: "#method", label: "Read the method" },
  fact: "Runs on its own storage or yours: Amazon S3, Cloudflare R2, Google Cloud Storage, Azure Blob or any S3-compatible service.",
};

export type Station = { id: string; name: string; note: string };

// Figure 1: one file on its way to a snapshot.
export const figure1 = {
  number: "Figure 1",
  title: "One file, from upload to snapshot",
  file: "train-00042.parquet",
  hash: "sha256 9f2c7a0e…e41b",
  stations: [
    { id: "upload", name: "Upload", note: "Parts go straight to storage" },
    { id: "verify", name: "Verify", note: "Size and SHA-256 match" },
    { id: "inspect", name: "Inspect", note: "Content checks pass" },
    { id: "scan", name: "Scan", note: "No malware found" },
    { id: "catalog", name: "Catalog", note: "Listed as version 3" },
    { id: "snapshot", name: "Snapshot", note: "Pinned, read-only" },
  ] satisfies Station[],
  sealed: "Snapshot v3 published. Its versions can no longer change.",
  replay: "Replay figure",
};

export type Problem = { id: string; title: string; before: string; after: string };

// Figure 2: the four failures, each drawn twice.
export const problem = {
  heading: "Where training data goes wrong",
  intro: "Most teams keep training data in a bucket and track it in a spreadsheet. Data problems then surface weeks later, when someone notices them by hand.",
  number: "Figure 2",
  title: "4 failures, before and after",
  modes: { before: "Bucket and spreadsheet", after: "With Orbidune" },
  items: [
    {
      id: "drift",
      title: "Files change after they were checked",
      before: "Someone overwrites a file in the bucket, and the next run trains on bytes nobody tested.",
      after: "A snapshot pins exact versions and a manifest hash, so a published snapshot never changes.",
    },
    {
      id: "gaps",
      title: "Reports hide the checks that never ran",
      before: "A dashboard says ready while half its checks were skipped or only sampled.",
      after: "Every check is reported as passed, warning, failed, sampled or not checked.",
    },
    {
      id: "unsafe",
      title: "Unsafe files reach the people who open them",
      before: "A pickled model calls os.system when loaded. An archive carries an AWS key.",
      after: "Nothing is readable until its hash matches, its contents are inspected and a malware scan passes.",
    },
    {
      id: "lineage",
      title: "Nobody can trace a model back to its data",
      before: "Months later, no one can say which files trained the model in production.",
      after: "Lineage links imports, reports, training runs and models to the snapshot each one used.",
    },
  ] satisfies Problem[],
};

// Figure 3: four more failures, each card playing the problem and then the fix.
export const solved = {
  heading: "More problems Orbidune removes",
  intro: "Each picture plays the problem first, then the same moment with Orbidune.",
  number: "Figure 3",
  title: "4 more problems, before and after",
  without: "Without Orbidune",
  with: "With Orbidune",
  items: [
    {
      id: "uploads",
      title: "Uploads that fail halfway",
      before: "A dropped connection at 60% sends the whole file again.",
      after: "Large files upload in parts, and a failed part retries alone.",
    },
    {
      id: "holds",
      title: "Deletes that outrun a legal hold",
      before: "A hold noted in a spreadsheet does not stop a delete.",
      after: "Storage enforces the hold, so a delete on held data is refused.",
    },
    {
      id: "duplicates",
      title: "The same bytes stored twice",
      before: "Every copy of a file takes up space again.",
      after: "Identical content is stored once and referenced by its hash.",
    },
    {
      id: "erasure",
      title: "Erasure that misses copies",
      before: "Deleting one file leaves its copies in older snapshots.",
      after: "Erasure finds every copy, takes it out of snapshots and signs a certificate.",
    },
  ] satisfies Problem[],
};

export type Stage = { id: string; name: string; sub: string; text: string; features: string[] };

// Figure 3: the whole method, scrolled stage by stage.
export const method = {
  heading: "How Orbidune handles a dataset",
  intro: "Every object takes the same 8 stages. Governance, a second copy and an evidence trail run beside all of them.",
  number: "Figure 4",
  title: "The path of every object",
  rails: ["Governance", "Second copy", "Evidence"],
  stages: [
    {
      id: "connect",
      name: "Connect",
      sub: "storage",
      text: "Use platform storage or your own bucket. A new connection takes no uploads until its test passes.",
      features: ["6 storage backends", "Credentials sealed on the server, never sent back", "Bucket settings re-checked every 24 hours"],
    },
    {
      id: "arrive",
      name: "Arrive",
      sub: "upload or adopt",
      text: "Large files upload in parts that retry alone. Million-file imports run from one manifest.",
      features: ["Multipart uploads straight to storage", "JSON Lines manifests with a SHA-256 per file", "Adopt data in place, without uploading it again"],
    },
    {
      id: "verify",
      name: "Verify",
      sub: "size and hash",
      text: "Each object waits in quarantine until its size and SHA-256 match what the client sent.",
      features: ["Type decided by the file's own bytes", "Nothing readable while in quarantine"],
    },
    {
      id: "inspect",
      name: "Inspect",
      sub: "content and malware",
      text: "Contents are read for what the type can hide, then scanned for malware.",
      features: ["Model files, archives, documents, images and formats", "Malware scan, again when signatures change", "Your policy decides: flag or block"],
    },
    {
      id: "catalog",
      name: "Catalog",
      sub: "every version",
      text: "A passed object becomes one version of a logical object. Overwrites keep their history.",
      features: ["Stable IDs and version history", "Whole-file and chunk-level dedup", "Storage tiers and cost estimates"],
    },
    {
      id: "snapshot",
      name: "Snapshot",
      sub: "versions pinned",
      text: "A dataset snapshot pins exact versions and a manifest hash. Drafts branch and merge.",
      features: ["Published snapshots are read-only", "Branches, merges and conflict checks", "Public publications with a citation"],
    },
    {
      id: "readiness",
      name: "Readiness",
      sub: "what ran, what did not",
      text: "Validation runs at metadata, sampled or full depth, and every report says which.",
      features: ["9 readiness dimensions, no score", "Leakage, drift and split checks", "Profiles of every table and column"],
    },
    {
      id: "use",
      name: "Use",
      sub: "train and trace",
      text: "Training jobs stream a snapshot in shards, and every run is linked back to it.",
      features: ["Shards for PyTorch, TensorFlow, JAX, Ray and Hugging Face", "Python, TypeScript and Go SDKs", "Lineage from import to model"],
    },
  ] satisfies Stage[],
};

export type Dimension = { name: string; text: string; status: "Passed" | "Warning" | "Failed" | "Not checked"; coverage: string };

// Table 1: a readiness report, with example values.
export const report = {
  heading: "Reports that list what did not run",
  intro: "A readiness assessment judges a snapshot on 9 dimensions. Each one states its status and how much it examined. There is no overall score.",
  number: "Table 1",
  title: "Readiness of one snapshot, example values",
  columns: ["Dimension", "What it checks", "Status", "Examined"],
  dimensions: [
    { name: "Completeness", text: "Labels, required metadata and every referenced object are present.", status: "Passed", coverage: "All" },
    { name: "Integrity", text: "Recorded sizes and checksums agree, and content checks passed where they ran.", status: "Warning", coverage: "Sampled" },
    { name: "Security", text: "Each member has malware scan evidence, and it is clean.", status: "Passed", coverage: "All" },
    { name: "Consistency", text: "Media types, paths and split assignments follow the profile.", status: "Passed", coverage: "All" },
    { name: "Duplication", text: "Duplicate references and duplicate content are reported.", status: "Warning", coverage: "All" },
    { name: "Distribution", text: "Labels, splits and file sizes stay within the limits you set.", status: "Not checked", coverage: "None" },
    { name: "Reproducibility", text: "The snapshot is published and its pinned versions are still stored.", status: "Passed", coverage: "All" },
    { name: "Governance", text: "Required governance metadata is attached to every member.", status: "Failed", coverage: "All" },
    { name: "Accessibility", text: "Every pinned version can still be read by the people who need it.", status: "Passed", coverage: "All" },
  ] satisfies Dimension[],
};

export type Capability = { id: string; name: string; fixes: string; features: string[] };

// Table 2: everything in the console, by the problem it removes.
export const features = {
  heading: "Everything the console does",
  intro: "The console covers storage, ingestion, the catalog, datasets, readiness, retention, evidence and access, each scoped to the organization you are working in.",
  number: "Table 2",
  title: "Capabilities and the problems they remove",
  columns: ["Area", "Removes", "Includes"],
  rows: [
    {
      id: "storage",
      name: "Storage connections",
      fixes: "Data locked into one provider, and secrets pasted into scripts.",
      features: ["Platform storage, S3, R2, GCS, Azure Blob, S3-compatible", "Region pinning and your own keys"],
    },
    {
      id: "ingest",
      name: "Uploads and ingestion",
      fixes: "Uploads that fail halfway and imports one bad line can sink.",
      features: ["Multipart and delta uploads", "Manifest ingestion and adoption in place", "Edge agent for instruments"],
    },
    {
      id: "gate",
      name: "Quarantine and scanning",
      fixes: "Files readable before anyone checked them.",
      features: ["SHA-256 verification", "Content checks with policy", "Malware scan and re-scan"],
    },
    {
      id: "catalog",
      name: "Catalog",
      fixes: "Overwrites that lose history, and the same bytes stored twice.",
      features: ["Versions and stable IDs", "Dedup and packing of small files", "Tiers, restores and budgets"],
    },
    {
      id: "datasets",
      name: "Datasets and snapshots",
      fixes: "Training sets that change under a model after review.",
      features: ["Pinned snapshots, branches and merges", "Verified copies to a second provider", "Embedding indexes per version"],
    },
    {
      id: "readiness",
      name: "Validation and readiness",
      fixes: "Green reports over checks that never ran.",
      features: ["Profiles at 3 depths", "9 dimensions, no score", "Leakage and drift checks"],
    },
    {
      id: "governance",
      name: "Retention and erasure",
      fixes: "A delete that outruns a legal hold, and consent withdrawn but still in a snapshot.",
      features: ["Retention, legal holds and locks", "Two-person approvals", "Erasure with a signed certificate"],
    },
    {
      id: "evidence",
      name: "Audit and evidence",
      fixes: "An audit that cannot prove who did what, and when.",
      features: ["Hash-chained audit trail", "Evidence packs and exports", "Scheduled exception reviews"],
    },
    {
      id: "access",
      name: "Access and identity",
      fixes: "Shared passwords and links that never expire.",
      features: ["Roles per organization, SSO and SCIM", "Scoped API tokens", "Expiring links and download budgets"],
    },
  ] satisfies Capability[],
};

export const integrations = {
  heading: "Works with your storage and your training code",
  intro: "Connect the buckets your team already uses. Training code reads snapshots through the Python SDK, with adapters for 5 frameworks.",
  storageTitle: "Storage backends",
  storage: [
    { id: "platform", name: "Platform storage", needs: "Nothing to set up" },
    { id: "s3", name: "Amazon S3", needs: "Access key or role" },
    { id: "r2", name: "Cloudflare R2", needs: "Access key" },
    { id: "gcs", name: "Google Cloud Storage", needs: "Service account key" },
    { id: "azure", name: "Azure Blob Storage", needs: "Account key or SAS token" },
    { id: "s3c", name: "S3-compatible", needs: "Access key and endpoint" },
  ],
  codeTitle: "Read a snapshot for training",
  frameworks: [
    { id: "torch", name: "PyTorch", code: "from osp.torch import SnapshotDataset\n\nds = SnapshotDataset(client, dataset_id, version_id)" },
    { id: "tf", name: "TensorFlow", code: "from osp.tf import as_tf_dataset\n\nds = as_tf_dataset(stream, output_signature)" },
    { id: "hf", name: "Hugging Face", code: "from osp.hf import as_hf_dataset\n\nds = as_hf_dataset(stream)" },
    { id: "jax", name: "JAX", code: "from osp.jax import batches\n\nfor batch in batches(stream, batch_size=64):\n    ..." },
    { id: "ray", name: "Ray", code: "from osp.ray import read_snapshot\n\nds = read_snapshot(client, dataset_id, version_id)" },
  ],
  sdks: "SDKs for Python, TypeScript and Go. Git LFS and DICOMweb read straight from a snapshot.",
};

export const close = {
  heading: "Start with platform storage",
  text: "Platform storage works from your first sign-in, so trying Orbidune needs no cloud credentials.",
};

export const footer = {
  columns: [
    { title: "Product", links: nav },
    {
      title: "Account",
      links: [
        { href: "/login", label: "Sign in" },
        { href: "/register", label: "Create an account" },
      ],
    },
  ],
  trademarks: "Product names and logos are trademarks of their owners.",
};
