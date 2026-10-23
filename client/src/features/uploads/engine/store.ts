import type { ResumeRecord } from "../types";

export type ResumeStore = {
  list(organizationId: string): Promise<ResumeRecord[]>;
  get(uploadId: string): Promise<ResumeRecord | undefined>;
  put(record: ResumeRecord): Promise<void>;
  remove(uploadId: string): Promise<void>;
};

export function memoryStore(): ResumeStore {
  const records = new Map<string, ResumeRecord>();
  return {
    async list(organizationId) {
      return [...records.values()].filter((r) => r.organization_id === organizationId);
    },
    async get(uploadId) {
      return records.get(uploadId);
    },
    async put(record) {
      records.set(record.upload_id, structuredClone(record));
    },
    async remove(uploadId) {
      records.delete(uploadId);
    },
  };
}

const DB_NAME = "osp-uploads";
const STORE = "multipart";

function openDb(): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const request = indexedDB.open(DB_NAME, 1);
    request.onupgradeneeded = () => request.result.createObjectStore(STORE, { keyPath: "upload_id" });
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => reject(request.error);
  });
}

async function run<T>(mode: IDBTransactionMode, action: (store: IDBObjectStore) => IDBRequest<T>): Promise<T> {
  const db = await openDb();
  try {
    return await new Promise<T>((resolve, reject) => {
      const request = action(db.transaction(STORE, mode).objectStore(STORE));
      request.onsuccess = () => resolve(request.result);
      request.onerror = () => reject(request.error);
    });
  } finally {
    db.close();
  }
}

// IndexedDB can be missing or blocked (private windows). The upload still
// works then; it just cannot be resumed after a reload.
export function indexedDbStore(): ResumeStore {
  return {
    async list(organizationId) {
      try {
        const all = await run<ResumeRecord[]>("readonly", (s) => s.getAll());
        return all.filter((r) => r.organization_id === organizationId);
      } catch {
        return [];
      }
    },
    async get(uploadId) {
      try {
        return await run<ResumeRecord | undefined>("readonly", (s) => s.get(uploadId));
      } catch {
        return undefined;
      }
    },
    async put(record) {
      try {
        await run("readwrite", (s) => s.put(record));
      } catch {
        // see above
      }
    },
    async remove(uploadId) {
      try {
        await run("readwrite", (s) => s.delete(uploadId));
      } catch {
        // see above
      }
    },
  };
}
