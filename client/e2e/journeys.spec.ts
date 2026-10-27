import AxeBuilder from "@axe-core/playwright";
import { expect, test } from "@playwright/test";
import { credentials, openOrganization, orgA, orgB, signIn, uploadThroughUi } from "./helpers";

test.describe("signing in", () => {
  test("rejects a wrong password with a clear message and stays on the page", async ({ page }) => {
    await page.goto("/login");
    await page.getByLabel("Email").fill(credentials("OWNER").email);
    await page.getByLabel("Password", { exact: true }).fill("not the password");
    await page.getByRole("button", { name: "Sign in" }).click();
    await expect(page.getByText("The email or password is wrong.")).toBeVisible();
    await expect(page).toHaveURL(/\/login/);
  });

  test("sends a signed-out visitor from the console to sign in, then back to where they were going", async ({ page }) => {
    await page.goto("/app/catalog");
    await expect(page).toHaveURL(/\/login\?next=%2Fapp%2Fcatalog/);
    const { email, password } = credentials("OWNER");
    await page.getByLabel("Email").fill(email);
    await page.getByLabel("Password", { exact: true }).fill(password);
    await page.getByRole("button", { name: "Sign in" }).click();
    await page.waitForURL(/\/app\/catalog/);
  });

  test("keeps tokens out of script: only HttpOnly cookies, nothing in storage", async ({ page, context }) => {
    await signIn(page, "OWNER");
    const cookies = await context.cookies();
    const session = cookies.filter((c) => c.name.startsWith("osp_"));
    expect(session.map((c) => c.name).sort()).toEqual(["osp_at", "osp_rt"]);
    for (const cookie of session) {
      expect(cookie.httpOnly).toBe(true);
      expect(cookie.sameSite).toBe("Strict");
    }
    const exposed = await page.evaluate(() => ({
      local: JSON.stringify({ ...localStorage }),
      session: JSON.stringify({ ...sessionStorage }),
      cookie: document.cookie,
    }));
    expect(exposed.local + exposed.session + exposed.cookie).not.toMatch(/sat_|srt_/);
  });

  test("signing out ends the session", async ({ page }) => {
    await signIn(page, "OWNER");
    await openOrganization(page, orgA());
    await page.getByRole("button", { name: "Account menu" }).click();
    await page.getByRole("menuitem", { name: "Sign out" }).click();
    await page.waitForURL(/\/login/);
    await page.goto("/app/overview");
    await expect(page).toHaveURL(/\/login/);
  });
});

test.describe("organizations and tenant isolation", () => {
  test("data from one organization is never shown in another", async ({ page }) => {
    const name = `e2e-isolation-${Date.now()}.txt`;
    let organizationId = "";
    page.on("request", (request) => {
      organizationId = request.headers()["x-organization-id"] ?? organizationId;
    });
    await signIn(page, "OWNER");
    await openOrganization(page, orgA());
    const objectId = await uploadThroughUi(page, name, Buffer.from("only in one organization"));
    const ownOrganization = organizationId;

    try {
      await page.goto("/app/catalog");
      await expect(page.getByText(name)).toBeVisible();

      // Switch to the other organization through the shell.
      await page.getByRole("button", { name: new RegExp(orgA()) }).click();
      await page.getByRole("menuitem", { name: new RegExp(orgB()) }).click();
      await page.waitForURL(/\/app\/overview/);

      await page.goto("/app/catalog");
      await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
      await expect(page.getByText(name)).toHaveCount(0);
      // Asked for by id from the other organization, the object does not exist.
      expect(organizationId).not.toBe(ownOrganization);
      const other = await page.request.get(`/api/proxy/catalog/objects/${objectId}`, {
        headers: { "X-Organization-ID": organizationId },
      });
      expect(other.status()).toBe(404);
    } finally {
      await page.request.delete(`/api/proxy/catalog/objects/${objectId}`, {
        headers: { Origin: new URL(page.url()).origin, "X-Organization-ID": ownOrganization },
      });
    }
  });
});

test.describe("roles", () => {
  test("an auditor can read but is not offered storage changes", async ({ page }) => {
    await signIn(page, "AUDITOR");
    await page.goto("/app/storage");
    await expect(page.getByRole("heading", { name: "Storage", exact: true })).toBeVisible();
    await expect(page.getByRole("link", { name: /add connection/i })).toHaveCount(0);
    await expect(page.getByRole("button", { name: /add connection/i })).toHaveCount(0);
  });

  test("the API refuses what the UI hides, even when asked directly", async ({ page }) => {
    await signIn(page, "AUDITOR");
    const status = await page.evaluate(async () => {
      const res = await fetch("/api/proxy/storage-connections", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ name: "x", provider: "S3_COMPATIBLE" }),
      });
      return res.status;
    });
    expect(status).toBe(403);
  });
});

test.describe("accessibility", () => {
  for (const path of ["/", "/login"]) {
    test(`no serious axe violations on ${path}`, async ({ page }) => {
      await page.goto(path);
      const results = await new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa"]).analyze();
      expect(results.violations.filter((v) => v.impact === "serious" || v.impact === "critical")).toEqual([]);
    });
  }

  test("no serious axe violations on the overview and the catalog", async ({ page }) => {
    await signIn(page, "DEVELOPER");
    for (const path of ["/app/overview", "/app/catalog", "/app/storage"]) {
      await page.goto(path);
      await expect(page.getByRole("main")).toBeVisible();
      const results = await new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa"]).analyze();
      expect(results.violations.filter((v) => v.impact === "serious" || v.impact === "critical"), path).toEqual([]);
    }
  });
});
