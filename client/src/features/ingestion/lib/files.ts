import type { IngestionEntry } from "../types";

export type FileIndex = Map<string, File>;

const lowerCache = new WeakMap<FileIndex, Map<string, File>>();

function clean(path: string) {
  return path
    .split("/")
    .filter((part) => part !== "" && part !== ".")
    .join("/");
}

// A picked folder gives "top/sub/a.jpg". The manifest path is relative to
// that folder, so the top folder name is dropped.
export function indexPickedFolder(files: Iterable<File>): FileIndex {
  const index: FileIndex = new Map();
  for (const file of files) {
    const relative = file.webkitRelativePath || file.name;
    const parts = relative.split("/");
    const path = file.webkitRelativePath ? parts.slice(1).join("/") : relative;
    index.set(clean(path), file);
  }
  return index;
}

export function findFile(index: FileIndex, entry: Pick<IngestionEntry, "path">): File | undefined {
  if (!entry.path) return undefined;
  const key = clean(entry.path);
  const exact = index.get(key);
  if (exact) return exact;
  // The server treats paths case-insensitively; try that before giving up.
  let lowered = lowerCache.get(index);
  if (!lowered) {
    lowered = new Map();
    for (const [path, file] of index) lowered.set(path.toLowerCase(), file);
    lowerCache.set(index, lowered);
  }
  return lowered.get(key.toLowerCase());
}

type DirectoryEntry = {
  isFile: boolean;
  isDirectory: boolean;
  name: string;
  file?: (ok: (file: File) => void, fail: (error: unknown) => void) => void;
  createReader?: () => { readEntries: (ok: (items: DirectoryEntry[]) => void, fail: (error: unknown) => void) => void };
};

async function walk(entry: DirectoryEntry, prefix: string, index: FileIndex) {
  if (entry.isFile && entry.file) {
    const file = await new Promise<File>((ok, fail) => entry.file?.(ok, fail));
    index.set(clean(`${prefix}${entry.name}`), file);
    return;
  }
  if (!entry.isDirectory || !entry.createReader) return;
  const reader = entry.createReader();
  // readEntries returns a limited batch each call and an empty one at the end.
  for (;;) {
    const batch = await new Promise<DirectoryEntry[]>((ok, fail) => reader.readEntries(ok, fail));
    if (batch.length === 0) return;
    for (const child of batch) await walk(child, `${prefix}${entry.name}/`, index);
  }
}

// A dropped folder: its own name is dropped too, like a picked folder.
export async function indexDroppedItems(items: DataTransferItemList): Promise<FileIndex> {
  const index: FileIndex = new Map();
  for (const item of Array.from(items)) {
    const entry = item.webkitGetAsEntry() as unknown as DirectoryEntry | null;
    if (!entry) continue;
    if (entry.isDirectory && entry.createReader) {
      const reader = entry.createReader();
      for (;;) {
        const batch = await new Promise<DirectoryEntry[]>((ok, fail) => reader.readEntries(ok, fail));
        if (batch.length === 0) break;
        for (const child of batch) await walk(child, "", index);
      }
    } else {
      await walk(entry, "", index);
    }
  }
  return index;
}
