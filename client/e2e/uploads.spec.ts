import { expect, test } from "@playwright/test";
import { createHash, randomBytes } from "node:crypto";
import { formatBytes } from "../src/lib/utils/format";
import { deleteCatalogObject, signIn, uploadThroughUi } from "./helpers";

const MIB = 1024 * 1024;

const cases = [
  { label: "a small file in one request", size: 2048 },
  { label: "a 20 MiB file in parts", size: 20 * MIB },
];

for (const { label, size } of cases) {
  test(`developer uploads ${label} and sees its size and SHA-256 in the catalog`, async ({ page }) => {
    const name = `e2e-upload-${Date.now()}-${size}.bin`;
    const body = randomBytes(size);
    const sha = createHash("sha256").update(body).digest("hex");
    let objectId = "";

    try {
      await signIn(page, "DEVELOPER");
      objectId = await uploadThroughUi(page, name, body);

      if (size > 16 * MIB) {
        await expect(page.getByText(/parts confirmed/)).toBeVisible();
        await expect(page.getByText(/upload in parts/)).toBeVisible();
      }

      // Find it by path prefix, then open it.
      await page.goto("/app/catalog");
      await page.getByLabel("Path prefix").fill(name);
      await page.getByRole("button", { name: /apply/i }).click();
      await page.getByRole("link", { name }).click();

      await expect(page.getByText(sha)).toBeVisible();
      await expect(page.getByRole("term").filter({ hasText: /^Size$/ }).locator("xpath=following-sibling::dd[1]")).toHaveText(formatBytes(size));
    } finally {
      await deleteCatalogObject(page, objectId);
    }
  });
}
