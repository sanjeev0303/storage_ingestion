import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it, vi } from "vitest";
import { API, server } from "@/test/server";
import { renderWithProviders } from "@/test/render";
import { validateAction } from "../validate";
import { AuditScreen } from "./audit-screen";

vi.mock("next/navigation", () => ({ usePathname: () => "/app/audit" }));

function event(seq: number, extra: object = {}) {
  return {
    id: `evt-${seq}`,
    seq,
    actor_type: "USER",
    actor_id: "user-1",
    action: "member.suspend",
    resource_type: "membership",
    resource_id: "user-2",
    outcome: "SUCCESS",
    created_at: "2026-10-22T10:00:00Z",
    hash: `hash-${seq}`,
    ...extra,
  };
}

describe("validateAction", () => {
  it("accepts names and prefixes, rejects what the API would ignore", () => {
    expect(validateAction("")).toBeNull();
    expect(validateAction("member.")).toBeNull();
    expect(validateAction("auth.password_change")).toBeNull();
    expect(validateAction("Member.")).not.toBeNull();
    expect(validateAction("user-create")).not.toBeNull();
    expect(validateAction("s3.read")).not.toBeNull();
  });
});

describe("AuditScreen", () => {
  it("tells the user about a bad action filter and does not send it", async () => {
    const urls: string[] = [];
    server.use(
      http.get(`${API}/organization/audit`, ({ request }) => {
        urls.push(request.url);
        return HttpResponse.json({ events: [event(1)] });
      }),
    );
    renderWithProviders(<AuditScreen />);
    await screen.findByText("member.suspend");

    await userEvent.type(screen.getByLabelText("Action"), "Member-1");
    await userEvent.tab();

    expect(await screen.findByText(/lowercase letters, dots and underscores/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Apply filters" })).toBeDisabled();
    expect(urls).toHaveLength(1);
  });

  it("pages with before=next_before and stops when it is absent", async () => {
    const calls: { before: string | null; limit: string | null }[] = [];
    server.use(
      http.get(`${API}/organization/audit`, ({ request }) => {
        const url = new URL(request.url);
        const before = url.searchParams.get("before");
        calls.push({ before, limit: url.searchParams.get("limit") });
        if (!before) return HttpResponse.json({ events: [event(10), event(9)], next_before: 9 });
        return HttpResponse.json({ events: [event(8, { action: "member.remove" })] });
      }),
    );
    renderWithProviders(<AuditScreen />);
    await screen.findByText("2 loaded, more available");

    await userEvent.click(screen.getByRole("button", { name: "Load more" }));
    await screen.findByText("3 loaded, end of list");

    expect(calls).toEqual([
      { before: null, limit: "50" },
      { before: "9", limit: "50" },
    ]);
  });

  it("shows details as text, including the hash", async () => {
    server.use(
      http.get(`${API}/organization/audit`, () =>
        HttpResponse.json({
          events: [event(1, { correlation_id: "corr-1", details: { note: "<b>bold</b>" } })],
        }),
      ),
    );
    renderWithProviders(<AuditScreen />);
    await userEvent.click(await screen.findByRole("button", { name: /Details/ }));

    expect(screen.getByText("corr-1")).toBeInTheDocument();
    expect(screen.getByText("<b>bold</b>")).toBeInTheDocument();
    expect(screen.getAllByText("hash-1").length).toBeGreaterThan(0);
  });

  it("reports a broken chain honestly", async () => {
    server.use(
      http.get(`${API}/organization/audit`, () => HttpResponse.json({ events: [event(1)] })),
      http.get(`${API}/organization/audit/verify`, () =>
        HttpResponse.json({
          chain: "org-1",
          checked: 12,
          first_seq: 1,
          last_seq: 12,
          intact: false,
          broken_at: 7,
          problem: "hash mismatch",
        }),
      ),
    );
    renderWithProviders(<AuditScreen />);
    await userEvent.click(screen.getByRole("button", { name: "Verify audit chain" }));

    expect(await screen.findByText("Chain is broken")).toBeInTheDocument();
    expect(screen.getByText("hash mismatch")).toBeInTheDocument();
    expect(screen.getByText("1 to 12")).toBeInTheDocument();
  });

  it("explains missing permission without calling the API", async () => {
    let called = false;
    server.use(
      http.get(`${API}/organization/audit`, () => {
        called = true;
        return HttpResponse.json({ events: [] });
      }),
    );
    renderWithProviders(<AuditScreen />, { permissions: ["org.read"] });

    expect(await screen.findByText(/needs the audit.read permission/)).toBeInTheDocument();
    await waitFor(() => expect(called).toBe(false));
  });

  it("lets platform users read the platform chain", async () => {
    let platformChain: string | null = null;
    server.use(
      http.get(`${API}/admin/audit`, ({ request }) => {
        platformChain = new URL(request.url).searchParams.get("chain");
        return HttpResponse.json({ events: [event(1, { action: "platform.bootstrap" })] });
      }),
    );
    renderWithProviders(<AuditScreen />, {
      permissions: ["org.read"],
      user: { platform_role: "AUDITOR" },
    });

    expect(await screen.findByText("platform.bootstrap")).toBeInTheDocument();
    expect(platformChain).toBe("_platform");
  });
});
