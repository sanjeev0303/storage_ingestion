import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { axe } from "vitest-axe";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { API, server } from "@/test/server";
import { renderWithProviders } from "@/test/render";
import { CreateDatasetForm } from "./create-dataset-form";
import { makeDataset } from "../fixtures";

const push = vi.fn();
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push }),
  usePathname: () => "/app/datasets/new",
}));

beforeEach(() => {
  push.mockReset();
  server.use(
    http.get(`${API}/validation-profiles`, () =>
      HttpResponse.json({
        profiles: [{ id: "builtin:general", version: 1, name: "General", description: "", built_in: true }],
      }),
    ),
  );
});

async function fillName(name: string) {
  await userEvent.type(screen.getByLabelText(/^Name/), name);
}

describe("CreateDatasetForm", () => {
  it("has no accessibility violations", async () => {
    const { container } = renderWithProviders(<CreateDatasetForm />);
    await screen.findByRole("option", { name: /General/ });
    expect(await axe(container)).toHaveNoViolations();
  });

  it("keeps submit disabled until the name is valid and shows the error and counter", async () => {
    renderWithProviders(<CreateDatasetForm />);
    const button = screen.getByRole("button", { name: "Create dataset" });
    expect(button).toBeDisabled();
    expect(screen.getByText("0 / 64")).toBeInTheDocument();
    await fillName("Bad Name");
    await userEvent.tab();
    expect(await screen.findByText(/lowercase letters, digits/)).toBeInTheDocument();
    expect(screen.getByText("8 / 64")).toBeInTheDocument();
    expect(button).toBeDisabled();
    await userEvent.clear(screen.getByLabelText(/^Name/));
    await fillName("pets");
    expect(button).toBeEnabled();
  });

  it("creates the dataset and opens it on 201", async () => {
    let body: Record<string, unknown> = {};
    server.use(
      http.post(`${API}/datasets`, async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json(makeDataset({ id: "ds-9", name: "pets" }), { status: 201 });
      }),
    );
    renderWithProviders(<CreateDatasetForm />);
    await fillName("pets");
    await userEvent.type(screen.getByLabelText("Tags"), "fixture, v1");
    await userEvent.selectOptions(screen.getByLabelText("Completion policy"), "PARTIAL");
    await userEvent.click(screen.getByRole("button", { name: "Create dataset" }));
    await vi.waitFor(() => expect(push).toHaveBeenCalledWith("/app/datasets/ds-9"));
    expect(body).toMatchObject({ name: "pets", tags: ["fixture", "v1"], completion_policy: "PARTIAL" });
  });

  it("says nothing new was made on 200", async () => {
    server.use(http.post(`${API}/datasets`, () => HttpResponse.json(makeDataset({ id: "ds-1" }), { status: 200 })));
    renderWithProviders(<CreateDatasetForm />);
    await fillName("pets");
    await userEvent.click(screen.getByRole("button", { name: "Create dataset" }));
    expect(await screen.findByText("This dataset already existed")).toBeInTheDocument();
    expect(push).not.toHaveBeenCalled();
    expect(screen.getByRole("link", { name: "Open pets" })).toHaveAttribute("href", "/app/datasets/ds-1");
  });

  it("explains an idempotency conflict", async () => {
    server.use(
      http.post(`${API}/datasets`, () =>
        HttpResponse.json({ error: "conflict", code: "idempotency_conflict" }, { status: 409 }),
      ),
    );
    renderWithProviders(<CreateDatasetForm />);
    await fillName("pets");
    await userEvent.click(screen.getByRole("button", { name: "Create dataset" }));
    expect(await screen.findByText(/already exists with different settings/)).toBeInTheDocument();
  });

  it("is denied without datasets.write", () => {
    renderWithProviders(<CreateDatasetForm />, { permissions: ["datasets.read"] });
    expect(screen.getByText("Your role cannot do this")).toBeInTheDocument();
  });
});
