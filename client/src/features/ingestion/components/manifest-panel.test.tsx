import { beforeEach, describe, expect, it } from "vitest";
import { screen } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { axe } from "vitest-axe";
import { API, server } from "@/test/server";
import { renderWithProviders } from "@/test/render";
import { ManifestPanel } from "./manifest-panel";

describe("ManifestPanel", () => {
  beforeEach(() => {
    server.use(
      http.get(`${API}/organization/quota`, () =>
        HttpResponse.json({ limits: { max_manifest_entries: 1000, max_manifest_bytes: 1000000 }, usage: {} }),
      ),
    );
  });

  it("documents the keys and has no accessibility violations", async () => {
    const { container } = renderWithProviders(<ManifestPanel jobId="job-1" canWrite />);
    expect(screen.getByText("checksum")).toBeInTheDocument();
    expect(screen.getByText(/CSV is not supported/)).toBeInTheDocument();
    expect(screen.getByLabelText(/Manifest file/)).toBeInTheDocument();
    expect((await axe(container)).violations).toEqual([]);
  });

  it("keeps Submit disabled until a file has been checked", () => {
    renderWithProviders(<ManifestPanel jobId="job-1" canWrite />);
    expect(screen.getByRole("button", { name: "Submit manifest" })).toBeDisabled();
  });
});
