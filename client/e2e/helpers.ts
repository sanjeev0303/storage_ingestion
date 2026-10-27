import { expect, type Page } from "@playwright/test";

export type Who = "OWNER" | "AUDITOR" | "DEVELOPER";

export function credentials(who: Who) {
  const email = process.env[`E2E_${who}_EMAIL`];
  const password = process.env[`E2E_${who}_PASSWORD`];
  if (!email || !password) throw new Error(`Set E2E_${who}_EMAIL and E2E_${who}_PASSWORD (see README, End-to-end tests).`);
  return { email, password };
}

export const orgA = () => process.env.E2E_ORG_A ?? "Acme";
export const orgB = () => process.env.E2E_ORG_B ?? "Globex";

export async function signIn(page: Page, who: Who) {
  const { email, password } = credentials(who);
  await page.goto("/login");
  await page.getByLabel("Email").fill(email);
  await page.getByLabel("Password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(/\/app\//);
}

// People who belong to several organizations are asked to pick one first.
export async function openOrganization(page: Page, name: string) {
  const heading = page.getByRole("heading", { level: 1 }).first();
  await heading.waitFor();
  if ((await heading.textContent())?.includes("Choose an organization")) {
    await page.getByRole("listitem").filter({ hasText: name }).getByRole("button", { name: "Open" }).click();
    await expect(page.getByRole("heading", { level: 1, name: "Overview" })).toBeVisible();
  }
}

// Uploads an in-memory file through the console and returns the catalog object id once it is AVAILABLE.
export async function uploadThroughUi(page: Page, name: string, body: Buffer) {
  await page.goto("/app/ingestion/uploads");
  await page.getByTestId("file-input").setInputFiles({ name, mimeType: "application/octet-stream", buffer: body });
  await page.getByRole("button", { name: "Start upload" }).click();
  await expect(page.getByText("Available in the catalog")).toBeVisible({ timeout: 90_000 });
  const href = await page.getByRole("link", { name: "Open in catalog" }).getAttribute("href");
  return href!.split("/").pop()!;
}

// Soft-deletes a catalog object with the page's session cookies. Safe to call when it is already gone.
export async function deleteCatalogObject(page: Page, id: string) {
  await page.request.delete(`/api/proxy/catalog/objects/${id}`, { headers: { Origin: new URL(page.url()).origin } });
}
