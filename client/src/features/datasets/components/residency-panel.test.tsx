import { screen } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { renderWithProviders } from "@/test/render";
import { API, server } from "@/test/server";
import { ResidencyPanel } from "./residency-panel";

describe("ResidencyPanel", () => {
  it("marks locations outside the dataset's rules", async () => {
    server.use(
      http.get(`${API}/datasets/ds1/versions/v1/residency`, () =>
        HttpResponse.json({
          allowed: ["ap-south-*"],
          require_customer_key: false,
          inside: false,
          locations: [
            { connection_id: "sc_in", location: "ap-south-1", location_verified: true, members: 40, bytes: 4096, allowed: true },
            { connection_id: "sc_us", location: "us-east-1", location_verified: false, members: 2, bytes: 100, allowed: false },
          ],
        }),
      ),
    );
    renderWithProviders(<ResidencyPanel id="ds1" versionId="v1" />, { permissions: ["datasets.read"] });
    expect(await screen.findByText("Outside its rules")).toBeInTheDocument();
    expect(screen.getByText("Outside")).toBeInTheDocument();
    expect(screen.getByText("Declared, not confirmed")).toBeInTheDocument();
    expect(screen.getByText("Allowed: ap-south-*.")).toBeInTheDocument();
  });
});
