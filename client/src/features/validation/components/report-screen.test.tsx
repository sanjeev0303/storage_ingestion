import { configure, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { renderWithProviders } from "@/test/render";
import { API, server } from "@/test/server";
import { finding, report, reportBody } from "../fixtures";
import type { ReadinessReport } from "../types";
import { ReportScreen } from "./report-screen";

// Other work on this machine can slow the first renders; the default 1s is too tight.
configure({ asyncUtilTimeout: 5000 });

const readiness: ReadinessReport = {
  profile: "builtin:general@1",
  disclaimer: "This assessment does not judge semantic quality or legal compliance.",
  dimensions: [
    { dimension: "COMPLETENESS", status: "WARN", checks: ["meta"], coverage: "FULL", notes: ["2 members lack a license"] },
    { dimension: "INTEGRITY", status: "WARN", checks: ["content"], coverage: "SAMPLED" },
    { dimension: "SECURITY", status: "NOT_ASSESSED", checks: [], coverage: "NONE" },
    { dimension: "CONSISTENCY", status: "PASS", checks: [], coverage: "FULL" },
    { dimension: "DUPLICATION", status: "NOT_ASSESSED", checks: [], coverage: "NONE" },
    { dimension: "DISTRIBUTION", status: "NOT_ASSESSED", checks: [], coverage: "NONE" },
    { dimension: "REPRODUCIBILITY", status: "FAIL", checks: [], coverage: "FULL" },
    { dimension: "GOVERNANCE", status: "NOT_ASSESSED", checks: [], coverage: "NONE" },
    { dimension: "ACCESSIBILITY", status: "PASS", checks: [], coverage: "FULL" },
  ],
};

function mockReport(body = reportBody({ readiness }), readinessFlag = true) {
  server.use(http.get(`${API}/validation-reports/rep-1`, () => HttpResponse.json(report(body, { readiness: readinessFlag }))));
}

function rowFor(table: HTMLElement, text: string) {
  const row = within(table).getByText(text).closest("tr");
  if (!row) throw new Error(`no row for ${text}`);
  return within(row as HTMLElement);
}

describe("ReportScreen", () => {
  it("maps each vocabulary to its own outcome, and never shows Not checked as passed", async () => {
    mockReport();
    server.use(http.get(`${API}/validation-reports/rep-1/findings`, () => HttpResponse.json({ findings: [] })));
    renderWithProviders(<ReportScreen reportId="rep-1" />);
    const results = await screen.findByRole("table", { name: "Rule results" });

    expect(rowFor(results, "meta").getByText("Failed")).toBeInTheDocument();
    expect(rowFor(results, "content").getByText("Partial (sampled)")).toBeInTheDocument();
    expect(rowFor(results, "count").getByText("Passed")).toBeInTheDocument();
    const notAssessed = rowFor(results, "scan");
    expect(notAssessed.getByText("Not checked")).toBeInTheDocument();
    expect(notAssessed.queryByText("Passed")).not.toBeInTheDocument();

    const dims = screen.getByRole("table", { name: "Readiness dimensions" });
    expect(within(dims).getAllByRole("row")).toHaveLength(10);
    expect(rowFor(dims, "Security").getByText("Not checked")).toBeInTheDocument();
    expect(rowFor(dims, "Security").queryByText("Pass")).not.toBeInTheDocument();
    expect(rowFor(dims, "Completeness").getByText("Warn")).toBeInTheDocument();
    expect(rowFor(dims, "Reproducibility").getByText("Fail")).toBeInTheDocument();
    expect(rowFor(dims, "Consistency").getByText("Pass")).toBeInTheDocument();
    expect(rowFor(dims, "Integrity").getByText("Sampled")).toBeInTheDocument();
  });

  it("shows the verdict from thresholds and the server disclaimer verbatim, with no score or percentage", async () => {
    mockReport();
    server.use(http.get(`${API}/validation-reports/rep-1/findings`, () => HttpResponse.json({ findings: [] })));
    const { container } = renderWithProviders(<ReportScreen reportId="rep-1" />);
    await screen.findByText(readiness.disclaimer);

    expect(screen.getByText(/Ready with warnings/)).toBeInTheDocument();
    expect(screen.getByText(/It is not a score/)).toBeInTheDocument();
    expect(container.textContent).not.toMatch(/\d\s*%/);
    expect(container.textContent).not.toMatch(/readiness score|\/\s*100\b/i);
    expect(screen.queryByRole("meter")).not.toBeInTheDocument();
  });

  it("omits the readiness section for a plain validation", async () => {
    mockReport(reportBody(), false);
    server.use(http.get(`${API}/validation-reports/rep-1/findings`, () => HttpResponse.json({ findings: [] })));
    renderWithProviders(<ReportScreen reportId="rep-1" />);
    await screen.findByRole("table", { name: "Rule results" });
    expect(screen.queryByRole("table", { name: "Readiness dimensions" })).not.toBeInTheDocument();
  });

  it("pages findings by cursor and sends only severity and rule_id as filters", async () => {
    mockReport(reportBody({ readiness, findings: { by_severity: { WARNING: 2 }, by_rule: { meta: 2 }, stored: 2 } }));
    const queries: URLSearchParams[] = [];
    server.use(
      http.get(`${API}/validation-reports/rep-1/findings`, ({ request }) => {
        const params = new URL(request.url).searchParams;
        queries.push(params);
        if (params.get("cursor") === "c1") return HttpResponse.json({ findings: [finding(2)] });
        return HttpResponse.json({ findings: [finding(1)], next_cursor: "c1" });
      }),
    );
    renderWithProviders(<ReportScreen reportId="rep-1" />);

    const table = await screen.findByRole("table", { name: "Findings" });
    expect(within(table).getByText("Member 1 lacks license")).toBeInTheDocument();
    expect(within(table).getByText("Add a license key", { selector: "td" })).toBeInTheDocument();
    expect(within(table).getByRole("link", { name: "obj-1" })).toHaveAttribute("href", "/app/catalog/obj-1");

    await userEvent.click(screen.getByRole("button", { name: "Load more" }));
    expect(await screen.findByText("Member 2 lacks license")).toBeInTheDocument();
    expect(queries[1].get("cursor")).toBe("c1");

    await userEvent.selectOptions(screen.getByLabelText("Severity"), "ERROR");
    await userEvent.selectOptions(screen.getByLabelText("Rule"), "meta");
    await screen.findByRole("table", { name: "Findings" });
    const last = queries[queries.length - 1];
    expect(last.get("severity")).toBe("ERROR");
    expect(last.get("rule_id")).toBe("meta");
    expect(last.has("cursor")).toBe(false);
    for (const key of last.keys()) expect(["severity", "rule_id", "limit", "cursor"]).toContain(key);
  });

  it("filters the loaded findings on the client and says so", async () => {
    mockReport();
    server.use(
      http.get(`${API}/validation-reports/rep-1/findings`, () => HttpResponse.json({ findings: [finding(1), finding(2, { code: "OTHER", message: "Other problem" })] })),
    );
    renderWithProviders(<ReportScreen reportId="rep-1" />);
    await screen.findByText("Member 1 lacks license");
    expect(screen.getByText(/already loaded below/)).toBeInTheDocument();
    await userEvent.type(screen.getByLabelText("Search loaded findings"), "other");
    expect(screen.queryByText("Member 1 lacks license")).not.toBeInTheDocument();
    expect(screen.getByText("Other problem")).toBeInTheDocument();
  });

  it("notes when a rule has more failures counted than findings stored", async () => {
    mockReport(
      reportBody({
        readiness,
        findings: { by_severity: { WARNING: 12000 }, by_rule: { meta: 12000 }, stored: 10000 },
        results: [{ ...reportBody().results[0], findings_stored: 10000 }],
      }),
    );
    server.use(http.get(`${API}/validation-reports/rep-1/findings`, () => HttpResponse.json({ findings: [] })));
    renderWithProviders(<ReportScreen reportId="rep-1" />);
    expect(await screen.findByText("Some findings are not stored.")).toBeInTheDocument();
    expect(screen.getByText(/12,000 counted, 10,000 stored/)).toBeInTheDocument();
  });

  it("shows permission denied on 403", async () => {
    server.use(http.get(`${API}/validation-reports/rep-1`, () => HttpResponse.json({ error: "forbidden", code: "forbidden" }, { status: 403 })));
    renderWithProviders(<ReportScreen reportId="rep-1" />);
    expect(await screen.findByText("Your role cannot do this")).toBeInTheDocument();
  });
});
