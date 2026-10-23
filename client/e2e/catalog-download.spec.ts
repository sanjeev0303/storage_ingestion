import { expect, test } from "@playwright/test";
import { randomBytes } from "node:crypto";
import { deleteCatalogObject, signIn, uploadThroughUi } from "./helpers";

test("developer downloads an object byte for byte, and a deleted version offers no download", async ({ page }) => {
  const name = `e2e-download-${Date.now()}.bin`;
  const body = randomBytes(4096);
  let objectId = "";

  try {
    await signIn(page, "DEVELOPER");
    objectId = await uploadThroughUi(page, name, body);

    await page.goto(`/app/catalog/${objectId}`);
    const link = page.getByLabel("Link type");
    const hasToken = (await link.locator("option[value='token']").count()) > 0;
    await link.selectOption(hasToken ? "token" : "storage");
    await page.getByRole("button", { name: "Create download link" }).click();

    const download = page.getByRole("link", { name: `Download ${name}` });
    await expect(download).toBeVisible();
    const href = await download.getAttribute("href");
    const response = await page.request.get(href!);
    expect(response.ok()).toBe(true);
    expect(Buffer.compare(await response.body(), body)).toBe(0);

    // Delete the object; its version stays in the history as a tombstone with no download.
    await page.getByRole("button", { name: "Delete object" }).click();
    const dialog = page.getByRole("alertdialog");
    await dialog.getByLabel(/Type .* to confirm/).fill(name);
    await dialog.getByRole("button", { name: "Delete object" }).click();
    await expect(page.getByText(/This object was deleted on/)).toBeVisible();

    await page.goto(`/app/catalog/${objectId}?version=1`);
    await expect(page.getByText(/soft-deleted. It stays in the history but cannot be downloaded/i)).toBeVisible();
    await expect(page.getByRole("button", { name: "Create download link" })).toHaveCount(0);
    await expect(page.getByRole("link", { name: /^Download / })).toHaveCount(0);
  } finally {
    await deleteCatalogObject(page, objectId);
  }
});
