import { apiRaw } from "@/lib/api/client";
import { isApiError } from "@/lib/api/errors";
import { MIB, MIN_PART_BYTES } from "@/lib/upload/parts";
import { UploadNetworkError } from "@/lib/upload/put";
import { sleep as realSleep } from "@/lib/upload/retry";
import { abortError } from "@/lib/upload/sha256";

// Below this the screen says the connection is slow, and new uploads use the
// smallest parts the providers accept.
export const SLOW_BPS = 256 * 1024;

const BACKOFF_BASE_MS = 1000;
const BACKOFF_CAP_MS = 30_000;

// Exponential backoff with full jitter: anywhere from nothing up to the
// doubled delay, so uploads that failed together do not all come back at once.
export function backoffMs(attempt: number, random = Math.random) {
  return random() * Math.min(BACKOFF_CAP_MS, BACKOFF_BASE_MS * 2 ** attempt);
}

// No bytes for this long while a part is sending means the request is stuck.
export class StalledError extends Error {
  constructor() {
    super("No data reached the storage provider for a while, so the request was stopped.");
    this.name = "StalledError";
  }
}

export function isNetworkFailure(error: unknown) {
  if (error instanceof UploadNetworkError || error instanceof StalledError) return true;
  return isApiError(error) && error.status === 0;
}

// Part size for a new upload, from the speed earlier uploads measured: small
// parts on a slow link lose less when the connection drops mid-part, large
// ones on a fast link mean fewer requests. choosePartSize still applies the
// provider limits (5 MiB at least, 10,000 parts at most).
export function preferredPartBytes(measuredBps: number) {
  if (measuredBps <= 0) return 8 * MIB;
  if (measuredBps < SLOW_BPS) return MIN_PART_BYTES;
  if (measuredBps > 8 * MIB) return 16 * MIB;
  return 8 * MIB;
}

// Any answer from the API, even a refusal, proves the connection works.
async function probeApi() {
  try {
    await apiRaw("/auth/me", { org: false, timeoutMs: 5000 });
    return true;
  } catch (error) {
    return !(isApiError(error) && error.status === 0);
  }
}

type WatchOptions = {
  probe: () => Promise<boolean>;
  sleep: (ms: number, signal?: AbortSignal) => Promise<void>;
};

// One per page, shared by every upload: whether the API can be reached. The
// browser's online and offline events are hints (navigator.onLine is true on
// a network with no internet), so a failed request triggers a check against
// the API, and being back online is only believed once the API answers.
export class ConnectionWatch {
  offline = false;
  // Upload speed measured on this page, for sizing new uploads: the sum of the
  // uploads sending now, since they share one link. It keeps the last total
  // once they stop.
  measuredBps = 0;
  private speeds = new Map<object, number>();
  private options: WatchOptions;
  private listeners = new Set<() => void>();
  private checking: Promise<boolean> | undefined;
  private probing = false;
  private listening = false;

  constructor(options: Partial<WatchOptions> = {}) {
    this.options = { probe: probeApi, sleep: realSleep, ...options };
  }

  subscribe(listener: () => void) {
    this.listen();
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  }

  reportSpeed(upload: object, bps: number) {
    this.speeds.set(upload, bps);
    let total = 0;
    for (const speed of this.speeds.values()) total += speed;
    this.measuredBps = total;
  }

  // The upload stopped sending, so it no longer counts towards the total.
  forgetSpeed(upload: object) {
    this.speeds.delete(upload);
  }

  // A request failed for a network reason. Resolves true when the API cannot
  // be reached either; every upload is then paused until it can.
  async reportFailure() {
    if (this.offline) return true;
    const reachable = await this.check();
    if (!reachable) this.setOffline(true);
    return !reachable;
  }

  // Checks now and, when the API answers, lets paused uploads go on.
  async recheck() {
    const reachable = await this.check();
    if (reachable) this.setOffline(false);
    return reachable;
  }

  // Checks now, sharing one check between callers.
  check() {
    if (!this.checking) {
      this.checking = this.options.probe().finally(() => (this.checking = undefined));
    }
    return this.checking;
  }

  waitUntilOnline(signal: AbortSignal) {
    return new Promise<void>((resolve, reject) => {
      if (signal.aborted) return reject(abortError());
      if (!this.offline) return resolve();
      const stop = this.subscribe(() => {
        if (this.offline) return;
        done();
        resolve();
      });
      const onAbort = () => {
        done();
        reject(abortError());
      };
      const done = () => {
        stop();
        signal.removeEventListener("abort", onAbort);
      };
      signal.addEventListener("abort", onAbort, { once: true });
    });
  }

  private listen() {
    if (this.listening || typeof window === "undefined") return;
    this.listening = true;
    window.addEventListener("offline", () => this.setOffline(true));
    window.addEventListener("online", () => void this.recheck());
  }

  private setOffline(offline: boolean) {
    if (this.offline === offline) return;
    this.offline = offline;
    for (const listener of [...this.listeners]) listener();
    if (offline) void this.probeUntilBack();
  }

  // The online event does not always come (a captive portal, a router that
  // lost its uplink), so the API is asked again every few seconds, backing
  // off to 30 seconds.
  private async probeUntilBack() {
    if (this.probing) return;
    this.probing = true;
    try {
      for (let attempt = 0; this.offline; attempt++) {
        await this.options.sleep(Math.min(2000 * 2 ** attempt, BACKOFF_CAP_MS));
        if (!this.offline) return;
        await this.recheck();
      }
    } finally {
      this.probing = false;
    }
  }
}

let shared: ConnectionWatch | undefined;

export function sharedConnection() {
  shared ??= new ConnectionWatch();
  return shared;
}
