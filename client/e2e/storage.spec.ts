import { expect, test } from "@playwright/test";
import { openOrganization, orgA, signIn } from "./helpers";

// Local MinIO from the dev stack. The keys come from .env.e2e, never from the spec.
const endpoint = process.env.E2E_MINIO_ENDPOINT ?? "http://127.0.0.1:9000";
const bucket = process.env.E2E_MINIO_BUCKET ?? "dev-assets";

test("owner connects MinIO, tests it, rotates with a wrong secret, then disables and deletes it", async ({ page }) => {
  const accessKey = process.env.E2E_MINIO_ACCESS_KEY;
  const secretKey = process.env.E2E_MINIO_SECRET_KEY;
  if (!accessKey || !secretKey) throw new Error("Set E2E_MINIO_ACCESS_KEY and E2E_MINIO_SECRET_KEY in .env.e2e.");

  const name = `e2e-minio-${Date.now()}`;
  let connectionId = "";
  let organizationId = "";
  page.on("request", (request) => {
    organizationId = request.headers()["x-organization-id"] ?? organizationId;
  });

  try {
    await signIn(page, "OWNER");
    await openOrganization(page, orgA());

    await page.goto("/app/storage/new");
    await page.getByLabel(/^Provider/).selectOption("S3_COMPATIBLE");
    await page.getByLabel(/^Authentication method/).selectOption("ACCESS_KEY");
    await page.getByLabel(/^Name/).fill(name);
    await page.getByLabel(/^Bucket/).fill(bucket);
    await page.getByLabel("Endpoint").fill(endpoint);
    await page.getByLabel(/^Access key ID/).fill(accessKey);
    await page.getByLabel(/^Secret access key/).fill(secretKey);
    await page.getByRole("button", { name: "Create connection" }).click();

    await page.getByRole("button", { name: "Test connection" }).click();
    await expect(page.getByText("Test passed")).toBeVisible();
    await page.getByRole("link", { name: "Open connection" }).click();
    await page.waitForURL(/\/app\/storage\/[^/]+$/);
    connectionId = page.url().split("/").pop()!;
    await expect(page.getByText("Active", { exact: true })).toBeVisible();

    // The secret is never shown again, on the page or in the DOM's form values.
    const shown = async () => (await page.locator("body").innerText()) + (await page.content());
    expect(await shown()).not.toContain(secretKey);

    // A wrong secret is refused and the old credentials stay.
    await page.getByRole("button", { name: "Rotate credentials" }).click();
    const dialog = page.getByRole("dialog", { name: "Rotate credentials" });
    await dialog.getByLabel(/^Access key ID/).fill(accessKey);
    await dialog.getByLabel(/^Secret access key/).fill(`wrong-secret-${Date.now()}`);
    await dialog.getByRole("button", { name: "Rotate credentials" }).click();
    await expect(dialog).toContainText(/old credentials (are still|stay)/i);
    expect(await shown()).not.toContain(secretKey);
    await dialog.getByRole("button", { name: "Cancel" }).click();

    await page.getByRole("button", { name: "Disable", exact: true }).click();
    await page.getByRole("alertdialog").getByRole("button", { name: "Disable connection" }).click();
    await expect(page.getByText("Disabled", { exact: true })).toBeVisible();

    await page.getByRole("button", { name: "Delete", exact: true }).click();
    const confirm = page.getByRole("alertdialog");
    await confirm.getByLabel(/Type .* to confirm/).fill(name);
    await confirm.getByRole("button", { name: "Delete connection" }).click();
    await page.waitForURL(/\/app\/storage$/);
    await expect(page.getByText(name)).toHaveCount(0);
    connectionId = "";
  } finally {
    if (connectionId) {
      const options = { headers: { Origin: new URL(page.url()).origin, "X-Organization-ID": organizationId } };
      await page.request.post(`/api/proxy/storage-connections/${connectionId}/disable`, options);
      await page.request.delete(`/api/proxy/storage-connections/${connectionId}`, options);
    }
  }
});
