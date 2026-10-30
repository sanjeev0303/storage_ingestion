import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { renderWithProviders } from "@/test/render";
import { API, server } from "@/test/server";
import { QualitySettingsPanel } from "./quality-settings-panel";
import { RunLeakage } from "./run-leakage";
import { VersionQuality } from "./version-quality";

const profile = {
  version_id: "v2",
  state: "DONE",
  files_profiled: 2,
  bytes_read: 4096,
  other_files: 1,
  baseline_version_id: "v1",
  drifted: true,
  tables: [
    {
      table: "scenes",
      format: "csv",
      files: 2,
      rows: 500,
      columns: [
        { name: "reflectance", kind: "number", count: 480, nulls: 20, distinct: 480, min: 0, max: 1299, mean: 620.5 },
        { name: "band", kind: "string", count: 500, nulls: 0, distinct: 2, top: [{ value: "B04", count: 375 }] },
      ],
    },
  ],
  skipped: [{ path: "huge/part-9.parquet", reason: "larger than the 4 GiB a file may be" }],
  drift: {
    baseline_version_id: "v1",
    version_id: "v2",
    thresholds: { max_psi: 0.2, max_null_change: 0.1, max_row_change: 0.5 },
    breaches: 2,
    not_compared: ["old.csv (gone)"],
    tables: [
      {
        table: "scenes",
        rows_before: 200,
        rows_after: 500,
        row_change: 1.5,
        columns_added: [],
        columns_removed: [],
        breached: ["rows"],
        columns: [{ name: "reflectance", kind_before: "number", kind_after: "number", psi: 4.2, null_rate_before: 0, null_rate_after: 0.04, breached: ["distribution"] }],
      },
    ],
  },
};

function base() {
  server.use(
    http.get(`${API}/datasets/ds1/versions/v2/profile`, () => HttpResponse.json(profile)),
    http.get(`${API}/datasets`, () => HttpResponse.json({ datasets: [{ id: "tr", name: "training-set" }], next_cursor: "" })),
    http.get(`${API}/datasets/tr/versions`, () => HttpResponse.json({ versions: [{ id: "tv3", version: 3 }] })),
  );
}

describe("VersionQuality", () => {
  it("shows the profile, its drift and what was not profiled", async () => {
    base();
    server.use(http.get(`${API}/datasets/ds1/releases`, () => HttpResponse.json({ releases: [] })));
    renderWithProviders(<VersionQuality id="ds1" versionId="v2" />, { permissions: ["datasets.read"] });
    expect(await screen.findByText("Profiled")).toBeInTheDocument();
    expect(screen.getByText("reflectance", { selector: "td" })).toBeInTheDocument();
    expect(screen.getByText("B04 (375)")).toBeInTheDocument();
    expect(screen.getByText(/larger than the 4 GiB/)).toBeInTheDocument();
    expect(screen.getByText("2 past the thresholds")).toBeInTheDocument();
    expect(screen.getByText(/distribution, PSI 4.2/)).toBeInTheDocument();
    expect(screen.getByText("Not compared: old.csv (gone)")).toBeInTheDocument();
    expect(screen.getByText("Not released.")).toBeInTheDocument();
  });

  it("checks leakage against a chosen training version", async () => {
    base();
    let asked: unknown;
    server.use(
      http.get(`${API}/datasets/ds1/releases`, () => HttpResponse.json({ releases: [] })),
      http.post(`${API}/datasets/ds1/versions/v2/leakage`, async ({ request }) => {
        asked = await request.json();
        return HttpResponse.json({
          train_version_id: "tv3",
          eval_version_id: "v2",
          checks: [
            { id: "c1", method: "EXACT_HASH", state: "LEAKED", overlap: 1, eval_members: 3, detail: "1 of 3 evaluation files have the same SHA-256",
              examples: [{ eval_path: "bench/copy.csv", train_path: "scenes/part-1.csv", key: "ab" }], train_version_id: "tv3", eval_version_id: "v2", created_at: "" },
            { id: "c2", method: "SUBJECT", state: "NOT_CHECKED", overlap: 0, eval_members: 0, detail: "the evaluation dataset names no subject key or pattern",
              examples: [], train_version_id: "tv3", eval_version_id: "v2", created_at: "" },
          ],
          not_checked: ["near-duplicate contamination: only exact hashes and subjects were compared"],
        });
      }),
    );
    renderWithProviders(<VersionQuality id="ds1" versionId="v2" />, { permissions: ["datasets.read"] });
    await screen.findByRole("option", { name: "training-set" });
    await userEvent.selectOptions(screen.getByLabelText("Training dataset"), "tr");
    await userEvent.selectOptions(await screen.findByLabelText("Training version"), await screen.findByRole("option", { name: "Version 3" }));
    await userEvent.click(screen.getByRole("button", { name: "Check for leakage" }));
    expect(await screen.findByText("Leaked")).toBeInTheDocument();
    expect(asked).toEqual({ train_version_id: "tv3" });
    expect(screen.getByText("scenes/part-1.csv")).toBeInTheDocument();
    expect(screen.getByText(/Not checked: near-duplicate/)).toBeInTheDocument();
  });

  it("releases the version with a label and notes", async () => {
    base();
    let body: unknown;
    server.use(
      http.get(`${API}/datasets/ds1/releases`, () => HttpResponse.json({ releases: [] })),
      http.put(`${API}/datasets/ds1/versions/v2/release`, async ({ request }) => {
        body = await request.json();
        return HttpResponse.json({ label: "DR2", version_id: "v2", version: 2, notes: "", members: 3, bytes: 10, released_at: "" });
      }),
    );
    renderWithProviders(<VersionQuality id="ds1" versionId="v2" />, { permissions: ["datasets.read", "datasets.manage"] });
    const label = await screen.findByLabelText(/^Label/);
    await userEvent.type(label, "bad label");
    expect(screen.getByRole("button", { name: "Release this version" })).toBeDisabled();
    await userEvent.clear(label);
    await userEvent.type(label, "DR2");
    await userEvent.type(screen.getByLabelText(/^Notes/), "Baseline 05.00");
    await userEvent.click(screen.getByRole("button", { name: "Release this version" }));
    await waitFor(() => expect(body).toEqual({ label: "DR2", notes: "Baseline 05.00" }));
  });
});

describe("QualitySettingsPanel", () => {
  it("saves thresholds as fractions and a subject pattern", async () => {
    let saved: unknown;
    server.use(
      http.get(`${API}/datasets/ds1/quality-settings`, () =>
        HttpResponse.json({ profile_snapshots: true, max_psi: 0.2, max_null_change: 0.1, max_row_change: 0.5, subject_key: "", subject_pattern: "" })),
      http.put(`${API}/datasets/ds1/quality-settings`, async ({ request }) => {
        saved = await request.json();
        return HttpResponse.json(saved as object);
      }),
    );
    renderWithProviders(<QualitySettingsPanel datasetId="ds1" />, { permissions: ["datasets.read", "datasets.manage"] });
    const nulls = await screen.findByLabelText("Null rate change");
    await userEvent.clear(nulls);
    await userEvent.type(nulls, "5");
    await userEvent.click(screen.getByLabelText(/A pattern on the path/));
    await userEvent.type(screen.getByLabelText("Path pattern"), "^patients/([[^/]+)/");
    await userEvent.click(screen.getByRole("button", { name: "Save quality settings" }));
    await waitFor(() =>
      expect(saved).toEqual({
        profile_snapshots: true,
        max_psi: 0.2,
        max_null_change: 0.05,
        max_row_change: 0.5,
        subject_key: "",
        subject_pattern: "^patients/([^/]+)/",
      }));
  });
});

describe("RunLeakage", () => {
  it("flags a run whose evaluation files are in training", async () => {
    server.use(
      http.get(`${API}/training-runs/r1/leakage`, () =>
        HttpResponse.json({ checks: [{ id: "c1", method: "EXACT_HASH", state: "LEAKED", overlap: 4, eval_members: 10, examples: [], detail: "", train_version_id: "a", eval_version_id: "b", created_at: "" }] })),
    );
    renderWithProviders(<RunLeakage runId="r1" roles={{ a: "TRAIN", b: "TEST" }} />);
    expect(await screen.findByText("Leakage")).toBeInTheDocument();
    expect(screen.getByText(/4 evaluation files in training/)).toBeInTheDocument();
  });

  it("says nothing for a run without roles", () => {
    const { container } = renderWithProviders(<RunLeakage runId="r2" />);
    expect(container).toBeEmptyDOMElement();
  });
});
