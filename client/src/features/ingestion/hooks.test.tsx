import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { API, server } from "@/test/server";
import { renderWithProviders } from "@/test/render";
import { POLL_MS, useJob } from "./hooks";

function JobProbe() {
  const job = useJob("job-1");
  return <p>{job.status}</p>;
}

describe("useJob", () => {
  it("stops polling when the first load fails", async () => {
    let requests = 0;
    server.use(
      http.get(`${API}/ingestion/jobs/job-1`, () => {
        requests += 1;
        return HttpResponse.json({ error: "boom" }, { status: 500 });
      }),
    );
    const { findByText } = renderWithProviders(<JobProbe />);
    await findByText("error");
    await new Promise((resolve) => setTimeout(resolve, POLL_MS + 500));
    expect(requests).toBe(1);
  }, 15000);
});
