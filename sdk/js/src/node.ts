// Node helpers: a file on disk as a Source, and a journal in a JSON file that
// survives a restart.

import { open, readFile, rename, stat, writeFile } from "node:fs/promises";
import { basename } from "node:path";
import type { Journal, JournalEntry, Source } from "./index.ts";

export async function fileSource(path: string, type?: string): Promise<Source> {
  const info = await stat(path);
  return {
    size: info.size,
    name: basename(path),
    type,
    async read(start, end) {
      const handle = await open(path, "r");
      try {
        const buffer = new Uint8Array(end - start);
        await handle.read(buffer, 0, buffer.length, start);
        return buffer;
      } finally {
        await handle.close();
      }
    },
  };
}

// Each journal file's reads and writes, one at a time: two at once would both
// read the old file, and the second write would drop the first one's entry.
const queues = new Map<string, Promise<unknown>>();

function inTurn<T>(path: string, work: () => Promise<T>): Promise<T> {
  const turn = (queues.get(path) ?? Promise.resolve()).then(work);
  queues.set(path, turn.catch(() => undefined));
  return turn;
}

export function fileJournal(path: string): Journal {
  async function load(): Promise<Record<string, JournalEntry>> {
    try {
      return JSON.parse(await readFile(path, "utf8")) as Record<string, JournalEntry>;
    } catch {
      return {};
    }
  }
  async function save(entries: Record<string, JournalEntry>) {
    // Whole or not at all: a crash mid-write leaves the old journal.
    await writeFile(`${path}.tmp`, JSON.stringify(entries));
    await rename(`${path}.tmp`, path);
  }
  return {
    get(key) {
      return inTurn(path, async () => (await load())[key]);
    },
    set(key, entry) {
      return inTurn(path, async () => {
        const entries = await load();
        entries[key] = entry;
        await save(entries);
      });
    },
    remove(key) {
      return inTurn(path, async () => {
        const entries = await load();
        delete entries[key];
        await save(entries);
      });
    },
  };
}
