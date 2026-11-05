// The words on the homepage, in one place so every section and diagram says the
// same true thing. Every claim here is something the backend does; nothing is
// invented (no customers, numbers, quotes or prices). Example data in diagrams
// is labelled as an example where it could be mistaken for a real result.
//
// House rules for anything added here: no em dashes, no "it's not X, it's Y",
// headlines of 32 characters or fewer, sentences a person would say.

export const STOPS = [
  { id: "question", name: "Question" },
  { id: "problem", name: "Problem" },
  { id: "connect", name: "Connect" },
  { id: "upload", name: "Upload" },
  { id: "verify", name: "Verify" },
  { id: "inspect", name: "Inspect" },
  { id: "ingest", name: "Ingest" },
  { id: "adopt", name: "Adopt" },
  { id: "catalog", name: "Catalog" },
  { id: "snapshot", name: "Snapshot" },
  { id: "validate", name: "Validate" },
  { id: "report", name: "Report" },
  { id: "readiness", name: "Readiness" },
  { id: "stream", name: "Stream" },
  { id: "lineage", name: "Lineage" },
  { id: "retain", name: "Retain" },
  { id: "lock", name: "Lock" },
  { id: "copies", name: "Copies" },
  { id: "access", name: "Access" },
  { id: "prove", name: "Prove" },
  { id: "begin", name: "Begin" },
] as const;

export type StopId = (typeof STOPS)[number]["id"];

export const copy = {
  hero: {
    title: "Know what was checked.",
    sentence:
      "Connect your storage, ingest datasets, and read which checks passed, which were sampled, and which never ran. Then decide whether a snapshot is ready to train on.",
    primary: "Open the console",
    secondary: "Follow one dataset",
    fragmentTitle: "Example report for a snapshot",
    modeLabel: "What to examine",
  },

  problem: {
    title: "Datasets fail quietly",
    lead: "A dataset passes through many hands before a model trains on it. Each hand-off can change it, skip a check, or leave no record. Follow one dataset through every hand-off, and see what the console keeps at each.",
    columns: [
      {
        run: "Quiet gaps.",
        text: "A check that never ran looks the same as a check that passed, unless the report says which.",
      },
      {
        run: "Moving targets.",
        text: "Edit a dataset after validating it and the report no longer describes what you have.",
      },
      {
        run: "Lost bytes.",
        text: "A part that failed halfway, a file that never finished, an object nobody scanned.",
      },
      {
        run: "Borrowed keys.",
        text: "Storage credentials pasted into tools that were never meant to hold them.",
      },
    ],
  },

  connect: {
    title: "Your storage, or ours",
    text: "Use platform-managed buckets, or connect Amazon S3, Cloudflare R2, Google Cloud Storage, Azure Blob Storage or any S3-compatible service. Credentials reach the server once, are sealed there, and never come back.",
    providers: [
      "Platform-managed storage",
      "Amazon S3",
      "Cloudflare R2",
      "Google Cloud Storage",
      "Azure Blob Storage",
      "S3-compatible services",
    ],
    steps: [
      { order: "First,", text: "add a connection. It starts Pending and takes no uploads." },
      { order: "Then,", text: "test it. A passing test makes it Active; a failing one says why and stores nothing new." },
      { order: "Last,", text: "make it the default for new uploads. Objects already stored never move between connections." },
    ],
    diagramLabel:
      "A storage connection moves from Pending to Active after a passing test, then can become the default. A failing test leaves it Pending. It can be disabled and enabled again, and deleted only while Pending or Disabled.",
  },

  upload: {
    title: "Large files go up in parts",
    text: "The browser hashes each file as it streams, splits it, and sends the parts straight to storage. A failed part is retried on its own. After a reload, parts already confirmed are not sent again while the upload session is still open.",
    note: "The platform has no resume call of its own, so the console keeps its own record of confirmed parts and says so.",
    diagramLabel:
      "A file is split into parts that upload four at a time. One part fails and is retried alone. When every part is confirmed the parts are joined into one object.",
  },

  verify: {
    title: "From bytes to a listed object",
    text: "Every object waits in quarantine until its size and SHA-256 match what was declared and a scan has run. Only then is it promoted and listed in the catalog.",
    stages: ["Upload", "Verify", "Scan", "Promote", "Catalog"],
    states: ["QUARANTINED", "PROCESSING", "AVAILABLE"],
    rejected: "REJECTED, with the reason",
    diagramLabel:
      "An object moves through five stages: upload, verify size and checksum, scan, promote, and catalog. Its status goes from quarantined to processing to available. A failed check ends in rejected, with a reason.",
  },

  ingest: {
    title: "A dataset from one manifest",
    text: "A JSON Lines manifest names every file with its size and checksum. A bad line becomes a visible error instead of failing the job, and every entry moves through the same states.",
    entry: { path: "images/cat-0412.jpg", size: "2.4 MiB", checksum: "sha256:9f2c7a…e41b" },
    states: ["Pending", "Uploading", "Published", "Invalid"],
    stateNotes: [
      "Waiting for a free upload slot.",
      "Bytes are on their way, with the lease renewed.",
      "Verified, scanned and listed in the catalog.",
      "The line could not be read, so it is listed with its error and never uploaded.",
    ],
    diagramLabel:
      "The same manifest entry shown in four states: pending, uploading, published, and invalid. An invalid line is reported with an error code.",
  },

  catalog: {
    title: "One object, many versions",
    text: "The catalog lists logical objects. Each version keeps its size, SHA-256 and status. Physical packs, offsets and storage keys are never shown.",
    rows: [
      { path: "images/cat-0412.jpg", versions: 3, size: "2.4 MiB", sha: "9f2c7a…e41b", status: "Available" },
      { path: "images/cat-0413.jpg", versions: 1, size: "2.1 MiB", sha: "1b84d0…77a3", status: "Available" },
      { path: "labels/train.jsonl", versions: 2, size: "318 KiB", sha: "c05e19…3d20", status: "Available" },
      { path: "labels/validation.jsonl", versions: 1, size: "41 KiB", sha: "7a3f6e…90bc", status: "Processing" },
      { path: "raw/session-08.bin", versions: 1, size: "1.8 GiB", sha: "e2d518…4a6f", status: "Quarantined" },
      { path: "raw/session-09.bin", versions: 1, size: "1.7 GiB", sha: "5c70a2…b8d1", status: "Rejected" },
    ],
    versionsOf: "images/cat-0412.jpg",
    versionRows: [
      { v: "Version 3", note: "Current, Available", sha: "9f2c7a…e41b" },
      { v: "Version 2", note: "Available, can be promoted again", sha: "44ab10…02fe" },
      { v: "Version 1", note: "Deleted, kept as a record, no download", sha: "0d9e3c…a715" },
    ],
  },

  snapshot: {
    title: "Drafts change. Snapshots do not.",
    text: "A snapshot pins exact object versions and a manifest hash. Add or remove members in the draft as often as you like; earlier snapshots keep their members, their checksums and their reports.",
    stages: [
      { name: "Draft", clause: "Start a draft of three members." },
      { name: "Snapshot 1", clause: "Pin it as Version 1." },
      { name: "Edit the draft", clause: "Swap a member. Version 1 stays." },
      { name: "Snapshot 2", clause: "Pin the new state as Version 2." },
    ],
    sliderLabel: "Drag through the stages of a dataset",
    diagramLabel:
      "A draft with members on one side and pinned versions on the other. Editing the draft changes its members. Version 1 keeps its original members and hash. Snapshotting again creates Version 2.",
  },

  validate: {
    title: "Pick how deep to look",
    statements: [
      {
        mode: "Metadata only",
        text: "Reads catalog records and never opens content. Fast, and it cannot tell you the stored bytes are intact.",
        read: 0,
        coverage: "None",
      },
      {
        mode: "Sampled",
        text: "Reads a fixed sample of members and states how many. A passing content check counts as partial.",
        read: 8,
        coverage: "Sampled",
      },
      {
        mode: "Full integrity",
        text: "Reads every member. The only mode that covers all of them, and the slowest.",
        read: 48,
        coverage: "Full",
      },
    ],
    total: 48,
    outcomes: ["Not checked", "Partial", "Passed"],
    diagramLabel:
      "A grid of 48 dataset members. In metadata mode none is read, in sampled mode 8 are read, and in full integrity mode all 48 are read.",
  },

  report: {
    title: "Reports that admit gaps",
    notes: {
      left: [
        {
          head: "Five outcomes, five shapes.",
          text: "Passed, Partial, Warning, Failed and Not checked each have their own mark, so a missing check never looks like a pass.",
        },
        {
          head: "Evidence stays attached.",
          text: "Checksum verification, scan results, and how much content was actually read are listed beside the verdict.",
        },
      ],
      right: [
        {
          head: "A verdict, not a score.",
          text: "The result comes from the profile's thresholds on errors and warnings. The console never invents a readiness number.",
        },
        {
          head: "What a pass does not prove.",
          text: "Validation does not show the data is correct, lawful to use, or good for training a model.",
        },
      ],
    },
  },

  readiness: {
    title: "Nine dimensions, no score",
    text: "A readiness assessment judges a snapshot against a profile on nine dimensions. Each reports its own status and how much was examined.",
    dimensions: [
      { name: "Completeness", text: "Labels, required metadata and every referenced object are present." },
      { name: "Integrity", text: "Recorded sizes and checksums agree, and content checks passed where they ran." },
      { name: "Security", text: "Malware scan evidence exists and is clean for each member." },
      { name: "Consistency", text: "Media types, paths and split assignments follow the profile." },
      { name: "Duplication", text: "Duplicate references and verified duplicate content are reported." },
      { name: "Distribution", text: "Labels, splits and file sizes are spread within the limits you set." },
      { name: "Reproducibility", text: "The snapshot is finalized and its pinned versions are still available." },
      { name: "Governance", text: "Required governance metadata is attached to every member." },
      { name: "Accessibility", text: "Every pinned version can still be read by the people who need it." },
    ],
    statuses: ["Pass", "Warn", "Fail", "Not checked"],
    coverage: ["Full", "Sampled", "Partial", "None"],
    noScore: "There is no overall score.",
  },

  lineage: {
    title: "Where every version came from",
    text: "Lineage records how datasets, versions, manifest imports, validation reports, transformations, training runs and models relate. Walk it upstream or downstream, a few levels at a time.",
    directions: ["Upstream", "Downstream"],
    relationNotes: [
      { name: "Contains", text: "A dataset holds its versions." },
      { name: "Derived from", text: "A version was built from an earlier one." },
      { name: "Imported from", text: "Members were loaded from a manifest import." },
      { name: "Validated by", text: "A version was checked, and a report was written." },
      { name: "Created from snapshot", text: "A new draft started from a pinned version." },
      { name: "Transformed by", text: "A transformation produced the data." },
      { name: "Trained on", text: "A training run read a snapshot, and the model it made points back to that run." },
    ],
    caption: "Example graph. Lineage is walked a few levels at a time and may be truncated in large graphs.",
    relations: ["Contains", "Derived from", "Imported from", "Validated by", "Created from snapshot", "Transformed by", "Trained on"],
    diagramLabel:
      "A dataset contains versions. Version 2 is derived from Version 1 through a transformation. Each version is validated by a report, and the dataset was imported from a manifest.",
  },

  retain: {
    title: "Deleting is not instant",
    text: "A deleted object waits out a grace period and can be restored until it is purged. Retention rules and legal holds can keep the bytes longer than a request asked.",
    states: [
      { name: "Live", note: "Listed in the catalog and readable." },
      { name: "Scheduled for removal", note: "Hidden from listings, restorable until the grace period ends." },
      { name: "Held", note: "A hold or retention rule keeps the bytes. Owners are not shown the hold itself." },
    ],
    diagramLabel:
      "An object is live. When deleted it enters a grace period during which it can be restored. When the grace period ends it is purged, unless a hold or retention rule keeps it.",
  },

  access: {
    title: "Roles to read, history to verify",
    text: "Four built-in roles, plus custom ones. Every sensitive action is written to a hash-chained audit log, and the chain can be verified.",
    roles: [
      {
        name: "Owner",
        text: "Everything, including granting ownership and deactivating the organization.",
      },
      {
        name: "Administrator",
        text: "Members, roles, settings and all data. Cannot grant ownership.",
      },
      {
        name: "Developer",
        text: "Uploads, ingests, reads and deletes data, and uses storage connections. Cannot manage them.",
      },
      {
        name: "Auditor",
        text: "Reads metadata, usage and the audit log. Cannot download or change anything.",
      },
    ],
    diagramLabel:
      "Audit events are linked in a chain, each holding the hash of the one before. Verifying walks the chain and reports where it breaks, if it does.",
  },

  inspect: {
    title: "Checks past the virus scan",
    text: "Once its checksum matches, every upload is opened and read for what its type can hide: code in model files, keys in text and archives, scripts in documents, attacks in images. Your policy decides whether a finding flags the file or rejects it.",
    pickLabel: "Pick an upload",
    samples: [
      {
        id: "model",
        file: "classifier.pt",
        check: "Model files",
        code: "model.unsafe_pickle",
        finding: "Loading it would call os.system. The pickle is walked, never run.",
        outcome: "rejected",
      },
      {
        id: "archive",
        file: "deploy.tar.gz",
        check: "Secrets",
        code: "secret.aws_access_key",
        finding: "An AWS key in deploy/.env inside the archive. The value is never copied.",
        outcome: "flagged",
      },
      {
        id: "pdf",
        file: "invoice.pdf",
        check: "Documents",
        code: "active_content.pdf_javascript",
        finding: "JavaScript in a stream behind ASCII85 and Flate filters.",
        outcome: "flagged",
      },
      {
        id: "image",
        file: "scan.png",
        check: "Images",
        code: "image.png_profile_text",
        finding: "A text chunk named profile, which ImageMagick reads as a file to embed.",
        outcome: "rejected",
      },
      {
        id: "reads",
        file: "reads.fastq.gz",
        check: "Formats",
        code: "No findings",
        finding: "Every record read to the end: header, sequence, quality.",
        outcome: "published",
      },
    ],
    stages: { upload: "Upload", sum: "SHA-256", inspect: "Content checks", scan: "Malware scan" },
    outcomes: { published: "Published", flagged: "Flagged, published", rejected: "Rejected" },
    notes: [
      { head: "Your engines too.", text: "A hook sends every upload to a PII, de-identification or YARA engine you run, and its verdict is recorded beside the platform's." },
      { head: "Signatures move on.", text: "Published files are scanned again when the scanner's signatures change, and an infected one stops being served at once." },
    ],
    diagramLabel:
      "An upload passes its SHA-256 check, then the content checks, then the malware scan. A rejecting finding stops it; a flagging finding lets it publish with the finding attached; a clean file is published.",
  },

  adopt: {
    title: "Data already in your bucket",
    text: "Point the platform at a list of what you already hold. Each object is read once where it sits, hashed against the digests the list gives, scanned, and published in place. Nothing is copied and the bucket is never listed.",
    pickLabel: "Pick a source",
    sources: [
      {
        id: "inventory",
        name: "S3 Inventory",
        reads: "manifest.json, then each CSV or Parquet data file",
        brings: "MD5 from single-part ETags",
        lands: "in place",
      },
      {
        id: "mhl",
        name: "ASC MHL",
        reads: "every generation in ascmhl/, checked against its chain",
        brings: "xxh64, MD5 and C4",
        lands: "in place",
      },
      {
        id: "bagit",
        name: "BagIt",
        reads: "bagit.txt, the payload manifests and bag-info",
        brings: "SHA-256, SHA-512 or MD5",
        lands: "in place",
      },
      {
        id: "stac",
        name: "STAC",
        reads: "a catalog's items and their assets, over the network guard",
        brings: "file:checksum multihashes",
        lands: "through an upload",
      },
      {
        id: "edge",
        name: "Edge agent",
        reads: "a watched folder beside the instrument, from a durable journal",
        brings: "SHA-256 worked out on the device",
        lands: "through an upload",
      },
    ],
    stages: { source: "Source list", pin: "Pin the version", read: "One read", match: "Digests match?" },
    outcomes: { inPlace: "Published in place", upload: "Uploaded, then checked", mismatch: "Refused: digest mismatch" },
    readNote: "hash, scan, checks",
    quota: "Quota is reserved for the whole job before the first object is adopted.",
    diagramLabel:
      "A source list names objects. Each version is pinned, read once to hash, scan and check it, and compared with the expected digests. A match is published in place or uploaded; a mismatch is refused.",
  },

  stream: {
    title: "Train straight from a snapshot",
    text: "A pinned snapshot streams as shards that never split a sample, in parallel, to the framework you train with. Each file is checked against the SHA-256 the snapshot pinned before your code sees it.",
    pickLabel: "Pick a framework",
    frameworks: [
      { id: "torch", name: "PyTorch", code: "from osp.torch import SnapshotDataset\nds = SnapshotDataset(client, dataset_id, version_id)" },
      { id: "tf", name: "TensorFlow", code: "from osp.tf import as_tf_dataset\nds = as_tf_dataset(stream, output_signature)" },
      { id: "hf", name: "Hugging Face", code: "from osp.hf import as_hf_dataset\nds = as_hf_dataset(stream)" },
      { id: "jax", name: "JAX", code: "from osp.jax import batches\nfor batch in batches(stream, batch_size=64): ..." },
      { id: "ray", name: "Ray", code: "from osp.ray import read_snapshot\nds = read_snapshot(client, dataset_id, version_id)" },
    ],
    stages: { snapshot: "Snapshot", shards: "Shards", check: "SHA-256 check", workers: "Workers" },
    shardNote: "about 128 MiB each, samples kept whole",
    bench: "osp bench measures how many streams this machine reads fastest with.",
    diagramLabel:
      "A pinned snapshot is cut into shards that keep samples whole. Each shard streams to a worker, every file is checked against its pinned SHA-256, and the framework receives the samples.",
  },

  lock: {
    title: "Holds outrank every delete",
    text: "Retention decides how long. This decides the order a delete must clear: a legal hold, then a retention period, then the bucket's own Object Lock. Any one keeps the bytes and the delete is tried again later. Erasure for a withdrawn subject waits in the same line, and every purge ends in a signed certificate.",
    toggleLabel: "Set what applies",
    toggles: [
      { id: "hold", name: "Legal hold", question: "Legal hold?" },
      { id: "retention", name: "Retention period", question: "Retention?" },
      { id: "lock", name: "Object Lock", question: "Object Lock?" },
    ],
    request: "Delete or erasure",
    outcomes: { kept: "Kept, checked again later", purged: "Purged", certificate: "Signed certificate" },
    notes: [
      { head: "COMPLIANCE when it must be.", text: "A COMPLIANCE lock cannot be shortened by anyone, so it is opt-in per policy and comes with guardrails." },
      { head: "Two people for the big ones.", text: "Bulk deletes and shortened retention can need a second approver, and a dry run shows what would go first." },
    ],
    diagramLabel:
      "A delete or erasure request is checked for a legal hold, a retention period and an Object Lock. If any applies the bytes are kept and checked again later. If none applies they are purged and a signed certificate records it.",
  },

  copies: {
    title: "A second copy that checks itself",
    text: "Replicate a dataset to a bucket at another provider. Scrubs re-read stored bytes against their SHA-256, a damaged chunk is rewritten from a verified replica, and reads move to the replica when the primary stops answering.",
    pickLabel: "Pick what happens",
    scenarios: [
      { id: "replicate", name: "Normal day", says: "Each new object is copied and verified. The console shows the recovery point: the age of the oldest object not yet copied." },
      { id: "repair", name: "A scrub finds damage", says: "The scrub reads a chunk that no longer matches its SHA-256. Only that chunk is rewritten, from the replica, after the replica's copy is checked." },
      { id: "failover", name: "The primary goes down", says: "The reader's request fails at the primary and is served from the replica, which holds the same verified bytes." },
    ],
    nodes: { writer: "New objects", primary: "Primary bucket", replica: "Replica, other provider", scrub: "Scrub", reader: "Reader" },
    diagramLabel:
      "New objects go to a primary bucket and are copied to a replica at another provider. A scrub re-reads the primary and repairs a damaged chunk from the replica. A reader is served from the primary, or from the replica when the primary fails.",
  },

  prove: {
    title: "Evidence an auditor can check",
    text: "The audit chain from the last stop does not stay in the database. On a schedule its head is signed and written outside it, so someone with database access still cannot rewrite history unnoticed, and the chain can leave for a SIEM or an inspector.",
    tamperLabel: "Change event 3",
    events: [
      { action: "dataset.snapshot", who: "dev@lab" },
      { action: "asset.download_url", who: "trainer-token" },
      { action: "retention.hold_place", who: "counsel@lab" },
      { action: "asset.download_url", who: "vendor-ws" },
      { action: "audit.export", who: "auditor@lab" },
    ],
    anchor: "Signed anchor, outside the database",
    intact: "Chain verified. The anchor's signature and head match.",
    broken: "Chain broken at event 3. Every later hash and the anchor disagree with it.",
    exports: [
      { head: "For your SIEM.", text: "Export the chain as JSON Lines or CEF, page by page, oldest first." },
      { head: "For an inspector.", text: "Custody reports, scheduled audit reviews and an EU AI Act training summary built from a model's lineage." },
    ],
    diagramLabel:
      "Five audit events, each holding the hash of the one before, end in a signed anchor kept outside the database. Changing an event breaks every hash after it and the anchor no longer matches.",
  },

  begin: {
    title: "Start with one connection.",
    text: "Add a storage connection, upload one file, and watch it move from quarantine to the catalog.",
    primary: "Sign in",
    dialogTitle: "Add a storage connection",
  },
} as const;
