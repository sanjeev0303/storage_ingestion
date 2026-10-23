import { configure, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { axe } from "vitest-axe";
import { renderWithProviders } from "@/test/render";
import { API, server } from "@/test/server";
import { dataset, job, profileList, versions } from "../fixtures";
import { StartForm } from "./start-form";

// Other work on this machine can slow the first renders; the default 1s is too tight.
configure({ asyncUtilTimeout: 5000 });

function mockReads() {
  server.use(
    http.get(`${API}/validation-profiles`, () => HttpResponse.json(profileList)),
    http.get(`${API}/datasets/ds-1/versions`, () => HttpResponse.json(versions)),
  );
}

describe("StartForm", () => {
  it("has no accessibility violations", async () => {
    mockReads();
    const { container } = renderWithProviders(<StartForm dataset={dataset} kind="validation" />);
    await screen.findByRole("option", { name: /Version 2 \(current\)/ }, { timeout: 5000 });
    expect(await axe(container)).toHaveNoViolations();
  });

  it("explains each mode and shows the sample size only for SAMPLED", async () => {
    mockReads();
    renderWithProviders(<StartForm dataset={dataset} kind="validation" />);
    expect(screen.getByText(/never reads object content/)).toBeInTheDocument();
    expect(screen.getByText(/integrity of every member/)).toBeInTheDocument();
    expect(screen.queryByLabelText("Sample size")).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("radio", { name: /Sampled integrity/ }));
    expect(screen.getByLabelText("Sample size")).toBeInTheDocument();
  });

  it("does not send sample_size for METADATA even after SAMPLED was tried", async () => {
    mockReads();
    let body: Record<string, unknown> = {};
    server.use(
      http.post(`${API}/datasets/ds-1/validations`, async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json(job({ state: "QUEUED" }), { status: 202 });
      }),
    );
    renderWithProviders(<StartForm dataset={dataset} kind="validation" />);
    await screen.findByRole("option", { name: /Version 2/ });
    await userEvent.click(screen.getByRole("radio", { name: /Sampled integrity/ }));
    await userEvent.type(screen.getByLabelText("Sample size"), "50");
    await userEvent.click(screen.getByRole("radio", { name: /Metadata only/ }));
    await userEvent.click(screen.getByRole("button", { name: "Start validation" }));

    await screen.findByRole("link", { name: "Open the job" });
    expect(body.mode).toBe("METADATA");
    expect(body).not.toHaveProperty("sample_size");
    expect(body.profile_id).toBe("builtin:general");
    expect(typeof body.idempotency_key).toBe("string");
  });

  it("disables Start and shows the error after leaving a bad sample size", async () => {
    mockReads();
    renderWithProviders(<StartForm dataset={dataset} kind="validation" />);
    await screen.findByRole("option", { name: /Version 2/ });
    await userEvent.click(screen.getByRole("radio", { name: /Sampled integrity/ }));
    const input = screen.getByLabelText("Sample size");
    await userEvent.type(input, "20000");
    expect(screen.getByRole("button", { name: "Start validation" })).toBeDisabled();
    expect(screen.queryByText(/whole number from 1 to 10,000/)).not.toBeInTheDocument();
    await userEvent.tab();
    expect(await screen.findByText(/whole number from 1 to 10,000/)).toBeInTheDocument();
    await userEvent.clear(input);
    await userEvent.type(input, "1,000");
    expect(screen.getByRole("button", { name: "Start validation" })).toBeEnabled();
  });

  it("reuses the idempotency key when the same attempt is retried, and says when the key was seen before", async () => {
    mockReads();
    const keys: string[] = [];
    server.use(
      http.post(`${API}/datasets/ds-1/validations`, async ({ request }) => {
        keys.push(((await request.json()) as { idempotency_key: string }).idempotency_key);
        if (keys.length === 1) return HttpResponse.json({ error: "busy", code: "too_many_jobs" }, { status: 429, headers: { "Retry-After": "60" } });
        return HttpResponse.json(job(), { status: 200 });
      }),
    );
    renderWithProviders(<StartForm dataset={dataset} kind="validation" />);
    await screen.findByRole("option", { name: /Version 2/ });
    await userEvent.click(screen.getByRole("button", { name: "Start validation" }));
    expect(await screen.findByText(/about 60 seconds/)).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Start validation" }));
    expect(await screen.findByText(/already had a job for this request/)).toBeInTheDocument();
    expect(keys[0]).toBe(keys[1]);
  });

  it("explains a missing snapshot", async () => {
    mockReads();
    server.use(http.post(`${API}/datasets/ds-1/validations`, () => HttpResponse.json({ error: "no version", code: "no_version" }, { status: 409 })));
    renderWithProviders(<StartForm dataset={dataset} kind="validation" />);
    await screen.findByRole("option", { name: /Version 2/ });
    await userEvent.click(screen.getByRole("button", { name: "Start validation" }));
    expect(await screen.findByText(/no snapshot yet/)).toBeInTheDocument();
  });

  it("requires a profile for a readiness assessment", async () => {
    mockReads();
    let calls = 0;
    server.use(http.post(`${API}/datasets/ds-1/readiness-assessments`, () => (calls++, HttpResponse.json(job(), { status: 202 }))));
    // profile list without the built-in general profile and a dataset with no profile
    server.use(http.get(`${API}/validation-profiles`, () => HttpResponse.json({ profiles: [], rule_types: [] })));
    renderWithProviders(<StartForm dataset={dataset} kind="readiness" />);
    await screen.findByRole("option", { name: /Version 2/ });
    expect(screen.getByRole("button", { name: "Start assessment" })).toBeDisabled();
    expect(screen.getByText("Choose a profile to continue.")).toBeInTheDocument();
    await waitFor(() => expect(calls).toBe(0));
  });

  it("blocks starting without the datasets.write permission", async () => {
    mockReads();
    renderWithProviders(<StartForm dataset={dataset} kind="validation" />, { permissions: ["datasets.read"] });
    expect(screen.getByRole("button", { name: "Start validation" })).toBeDisabled();
    expect(screen.getByText(/datasets.write permission/)).toBeInTheDocument();
  });
});
