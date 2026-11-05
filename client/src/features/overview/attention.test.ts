import { describe, expect, it } from "vitest";
import { rackJobs } from "./attention";
import type { JobSummary } from "./types";

const progress = { total: 4, published: 1, failed: 0, invalid: 0 };
const now = Date.parse("2026-11-04T12:00:00Z");
const job = (id: string, state: JobSummary["state"], updated_at: string): JobSummary => ({
  id,
  dataset_id: "d",
  state,
  progress,
  updated_at,
});

describe("rackJobs", () => {
  it("puts each job in exactly one rack and a stalled job in attention only", () => {
    const racks = rackJobs(
      [
        job("failed", "FAILED", "2026-11-04T11:59:00Z"),
        job("stalled", "RUNNING", "2026-11-04T09:00:00Z"),
        job("running", "RUNNING", "2026-11-04T11:58:00Z"),
        job("ready", "READY", "2026-11-04T08:00:00Z"),
        job("done", "COMPLETED", "2026-11-04T11:00:00Z"),
      ],
      now,
    );
    expect(racks.attention.map((a) => a.job.id)).toEqual(["failed", "stalled"]);
    expect(racks.moving.map((j) => j.id)).toEqual(["running"]);
    expect(racks.ready.map((j) => j.id)).toEqual(["ready"]);
    expect(racks.completed.map((j) => j.id)).toEqual(["done"]);
  });
});
