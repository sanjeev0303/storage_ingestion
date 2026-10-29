import { screen } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { API, server } from "@/test/server";
import { renderWithProviders } from "@/test/render";
import type { TrainingRun } from "../types";
import { TrainingRuns } from "./training-runs";

const run: TrainingRun = {
  id: "run-1",
  name: "resnet-run",
  source: "MLFLOW",
  external_id: "42",
  url: "https://mlflow.example.com/#/runs/42",
  status: "FINISHED",
  params: { lr: "0.01" },
  metrics: { accuracy: 0.91234, epochs: 10 },
  dataset_version_ids: ["ver-1", "ver-0"],
  models: [{ id: "m-1", name: "resnet", version: "3", uri: "s3://models/resnet/3", created_at: "2026-10-29T10:00:00Z" }],
  created_by: "user-1",
  created_at: "2026-10-29T09:00:00Z",
  finished_at: "2026-10-29T10:00:00Z",
};

describe("training runs of a version", () => {
  it("lists each run with its source, status, model and metrics", async () => {
    let asked = "";
    server.use(
      http.get(`${API}/training-runs`, ({ request }) => {
        asked = new URL(request.url).searchParams.get("dataset_version_id") ?? "";
        return HttpResponse.json({ runs: [run] });
      }),
    );
    renderWithProviders(<TrainingRuns datasetId="ds-1" versionId="ver-1" />);

    expect(await screen.findByText("resnet-run")).toBeInTheDocument();
    expect(asked).toBe("ver-1");
    expect(screen.getByRole("link", { name: "MLflow" })).toHaveAttribute("href", run.url);
    expect(screen.getByText(/with 1 other version$/)).toBeInTheDocument();
    expect(screen.getByText("Finished")).toBeInTheDocument();
    expect(screen.getByText("resnet 3")).toBeInTheDocument();
    expect(screen.getByText("accuracy 0.9123, epochs 10")).toBeInTheDocument();
  });

  it("shows how to record one when there are none", async () => {
    server.use(http.get(`${API}/training-runs`, () => HttpResponse.json({ runs: [] })));
    renderWithProviders(<TrainingRuns datasetId="ds-1" versionId="ver-1" />);
    expect(await screen.findByText(/No training run has recorded reading this version yet/)).toBeInTheDocument();
    expect(screen.getByText(/track_run/)).toBeInTheDocument();
  });
});
