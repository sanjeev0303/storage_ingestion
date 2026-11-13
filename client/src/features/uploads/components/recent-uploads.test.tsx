import { screen } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { API, server } from "@/test/server";
import { renderWithProviders } from "@/test/render";
import { RecentUploads } from "./recent-uploads";

const upload = {
  upload_id: "up-1",
  status: "VERIFIED",
  file_name: "cat.jpg",
  content_type: "image/jpeg",
  size_bytes: 2048,
  multipart: false,
  created_at: "2026-11-12T09:00:00Z",
  updated_at: "2026-11-12T09:01:00Z",
  expires_at: "2026-11-12T10:00:00Z",
};

describe("RecentUploads", () => {
  it("links an upload to the asset processing made from it", async () => {
    server.use(
      http.get(`${API}/uploads`, () =>
        HttpResponse.json({ uploads: [{ ...upload, asset_id: "asset-123456789" }, { ...upload, upload_id: "up-2", file_name: "dog.jpg", status: "UPLOADING" }] }),
      ),
    );
    renderWithProviders(<RecentUploads />);

    const link = await screen.findByRole("link", { name: /Asset asset-12/ });
    expect(link).toHaveAttribute("href", "/app/operations/assets?id=asset-123456789");
    expect(screen.getAllByText(/last changed/)).toHaveLength(2);
    // An upload without an asset yet gets no link.
    expect(screen.getAllByRole("link")).toHaveLength(1);
  });
});
