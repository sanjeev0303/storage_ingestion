import { expect, test } from "@playwright/test";

// Needs a backend started with AUTH_SELF_REGISTRATION=true. Against one that
// keeps registration closed the page must say so, and the test checks that instead.
test("a new person registers, owns an organization, and is signed in", async ({ page }) => {
  const email = `register-${Date.now()}@example.test`;
  const password = "correct horse battery staple";

  await page.goto("/register");
  await page.getByLabel(/organization name/i).fill("Registered Labs");
  await page.getByLabel("Your name").fill("Reggie");
  await page.getByLabel("Email").fill(email);
  await page.getByLabel("Password").fill(password);

  const answer = page.waitForResponse((res) => res.url().endsWith("/api/session/register"));
  await page.getByRole("button", { name: "Create account" }).click();
  const response = await answer;

  if (response.status() === 403) {
    await expect(page.locator("form").getByRole("alert")).toContainText("Sign-up is not open");
    return;
  }
  expect(response.status()).toBe(200);
  await expect(page).toHaveURL(/\/app\/overview/);

  // The browser holds only cookies; the API knows who this is and what they own.
  const me = await page.request.get("/api/proxy/auth/me");
  expect(me.ok()).toBe(true);
  const body = await me.json();
  expect(body.user.email).toBe(email);
  expect(JSON.stringify(body.memberships)).toContain("Registered Labs");
});

test("registering with an address that exists says to sign in", async ({ page }) => {
  await page.goto("/register");
  await page.getByLabel(/organization name/i).fill("Dup Labs");
  await page.getByLabel("Email").fill(process.env.E2E_OWNER_EMAIL ?? "owner@dev.local");
  await page.getByLabel("Password").fill("correct horse battery staple");
  await page.getByRole("button", { name: "Create account" }).click();
  await expect(page.locator("form").getByRole("alert")).toContainText(/already exists|not open/i);
});
