import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Membership } from "@/lib/api/types/identity";
import { SessionContext } from "@/lib/auth/session";
import { makeSession } from "@/test/render";
import { OrganizationGate } from "./organization-gate";

function membership(id: string, name: string): Membership {
  return { organization_id: id, organization_name: name, user_id: "user-1", role_id: "developer", status: "ACTIVE", created_at: "2026-01-01T00:00:00Z" };
}

afterEach(() => sessionStorage.clear());

describe("OrganizationGate", () => {
  it("opens the chosen organization on the page that was linked, not the overview", async () => {
    // switchOrganization sends the user to the overview; a deep link such as a
    // share page must survive choosing an organization.
    const switchOrganization = vi.fn();
    const session = {
      ...makeSession({ organizationId: null, memberships: [membership("org-1", "Acme"), membership("org-2", "Labs")] }),
      switchOrganization,
    };
    render(
      <SessionContext.Provider value={session}>
        <OrganizationGate>
          <p>The shared file</p>
        </OrganizationGate>
      </SessionContext.Provider>,
    );

    await userEvent.click(screen.getAllByRole("button", { name: "Open" })[1]);
    expect(switchOrganization).not.toHaveBeenCalled();
    expect(sessionStorage.getItem("orb.organization")).toBe("org-2");
  });
});
