import { configure, screen, waitFor } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { renderWithProviders } from "@/test/render";
import { API, server } from "@/test/server";
import { job } from "../fixtures";
import { JobScreen } from "./job-screen";

// Other work on this machine can slow the first renders; the default 1s is too tight.
configure({ asyncUtilTimeout: 5000 });

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

describe("JobScreen", () => {
  it("polls while the job is live and stops once it is terminal", async () => {
    let requests = 0;
    server.use(
      http.get(`${API}/validation-jobs/job-1`, () => {
        requests += 1;
        return HttpResponse.json(requests === 1 ? job({ state: "RUNNING" }) : job({ state: "COMPLETED", report_id: "rep-1" }));
      }),
    );
    renderWithProviders(<JobScreen jobId="job-1" />);
    expect(await screen.findByText(/Checking the backend every few seconds/)).toBeInTheDocument();

    expect(await screen.findByRole("link", { name: "Open the report" }, { timeout: 6000 })).toHaveAttribute("href", "/app/validation/reports/rep-1");
    expect(screen.queryByText(/Checking the backend/)).not.toBeInTheDocument();

    const seen = requests;
    await sleep(3200);
    expect(requests).toBe(seen);
  }, 15000);

  it("says the version failed when FAILED comes with a report", async () => {
    server.use(http.get(`${API}/validation-jobs/job-1`, () => HttpResponse.json(job({ state: "FAILED", report_id: "rep-1" }))));
    renderWithProviders(<JobScreen jobId="job-1" />);
    expect(await screen.findByText("The version failed validation")).toBeInTheDocument();
    expect(screen.queryByText("The job could not run")).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Open the report" })).toBeInTheDocument();
  });

  it("says the job could not run when FAILED has no report, and shows last_error", async () => {
    server.use(http.get(`${API}/validation-jobs/job-1`, () => HttpResponse.json(job({ state: "FAILED", last_error: "attempts exhausted" }))));
    renderWithProviders(<JobScreen jobId="job-1" />);
    expect(await screen.findByText("The job could not run")).toBeInTheDocument();
    expect(screen.getByText("attempts exhausted")).toBeInTheDocument();
    expect(screen.queryByText("The version failed validation")).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Open the report" })).not.toBeInTheDocument();
  });

  it("cancels a live job after confirmation and shows that it is stopping", async () => {
    server.use(
      http.get(`${API}/validation-jobs/job-1`, () => HttpResponse.json(job({ state: "RUNNING" }))),
      http.post(`${API}/validation-jobs/job-1/cancel`, () => HttpResponse.json(job({ state: "RUNNING", cancel_requested: true }), { status: 202 })),
    );
    const { default: userEvent } = await import("@testing-library/user-event");
    renderWithProviders(<JobScreen jobId="job-1" />);
    await userEvent.click(await screen.findByRole("button", { name: "Cancel job" }));
    const dialog = await screen.findByRole("alertdialog");
    await userEvent.click(dialog.querySelector("button:last-of-type") as HTMLElement);
    await waitFor(() => expect(screen.getByText(/Cancel requested/)).toBeInTheDocument());
  });

  it("shows an error with retry when the job cannot be loaded", async () => {
    server.use(http.get(`${API}/validation-jobs/job-1`, () => HttpResponse.json({ error: "nope", code: "not_found" }, { status: 404 })));
    renderWithProviders(<JobScreen jobId="job-1" />);
    expect(await screen.findByRole("button", { name: "Try again" })).toBeInTheDocument();
  });
});
