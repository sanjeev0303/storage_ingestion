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
    async get(key) {
      return (await load())[key];
    },
    async set(key, entry) {
      const entries = await load();
      entries[key] = entry;
      await save(entries);
    },
    async remove(key) {
      const entries = await load();
      delete entries[key];
      await save(entries);
    },
  };
}
