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
  { id: "ingest", name: "Ingest" },
  { id: "catalog", name: "Catalog" },
  { id: "snapshot", name: "Snapshot" },
  { id: "validate", name: "Validate" },
  { id: "report", name: "Report" },
  { id: "readiness", name: "Readiness" },
  { id: "lineage", name: "Lineage" },
  { id: "retain", name: "Retain" },
  { id: "access", name: "Access" },
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
    text: "Lineage records how datasets, versions, manifest imports, validation reports and transformations relate. Walk it upstream or downstream, a few levels at a time.",
    directions: ["Upstream", "Downstream"],
    relationNotes: [
      { name: "Contains", text: "A dataset holds its versions." },
      { name: "Derived from", text: "A version was built from an earlier one." },
      { name: "Imported from", text: "Members were loaded from a manifest import." },
      { name: "Validated by", text: "A version was checked, and a report was written." },
      { name: "Created from snapshot", text: "A new draft started from a pinned version." },
      { name: "Transformed by", text: "A transformation produced the data." },
    ],
    caption: "Example graph. Lineage is walked a few levels at a time and may be truncated in large graphs.",
    relations: ["Contains", "Derived from", "Imported from", "Validated by", "Created from snapshot", "Transformed by"],
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

  begin: {
    title: "Start with one connection.",
    text: "Add a storage connection, upload one file, and watch it move from quarantine to the catalog.",
    primary: "Sign in",
    dialogTitle: "Add a storage connection",
  },
} as const;
