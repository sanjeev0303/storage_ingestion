import { expect, test } from "@playwright/test";
import { randomBytes } from "node:crypto";
import { deleteCatalogObject, signIn, uploadThroughUi } from "./helpers";

test("developer builds a dataset, snapshots it, validates it and reads the report and lineage", async ({ page }) => {
  const stamp = Date.now();
  const objectName = `e2e-dataset-member-${stamp}.bin`;
  const datasetName = `e2e-ds-${stamp}`;
  let objectId = "";
  let datasetId = "";

  try {
    await signIn(page, "DEVELOPER");
    objectId = await uploadThroughUi(page, objectName, randomBytes(1024));

    await page.goto("/app/datasets/new");
    // The form may still be hydrating on the first visit, so fill and submit again until it navigates.
    await expect(async () => {
      await page.getByLabel(/^Name/).fill(datasetName);
      await page.getByRole("button", { name: "Create dataset" }).click();
      await page.waitForURL(/\/app\/datasets\/(?!new$)[^/]+$/, { timeout: 5_000 });
    }).toPass({ timeout: 30_000 });
    datasetId = page.url().split("/").pop()!;

    // Add the uploaded object by path.
    await page.goto(`/app/datasets/${datasetId}/members`);
    await page.getByRole("button", { name: "Add members" }).click();
    const dialog = page.getByRole("dialog", { name: "Add members" });
    await dialog.getByLabel("Path (required)").fill(objectName);
    await dialog.getByRole("button", { name: "Add member", exact: true }).click();
    await expect(page.getByRole("cell", { name: objectName })).toBeVisible();
    if (await dialog.isVisible()) await page.keyboard.press("Escape");

    // Snapshot.
    await page.getByRole("button", { name: "Create snapshot" }).click();
    await page.getByRole("dialog", { name: "Create snapshot" }).getByRole("button", { name: "Create snapshot" }).click();
    await expect(page.getByText("Version 1 created")).toBeVisible();
    await page.getByRole("button", { name: "Done" }).click();

    await page.goto(`/app/datasets/${datasetId}/versions`);
    const row = page.getByRole("row").filter({ hasText: "Version 1" });
    await expect(row).toContainText("FINALIZED");
    await expect(row).toContainText("Immutable");
    await expect(page.getByText("Editing the draft never changes one.")).toBeVisible();

    // Metadata validation with the built-in general profile.
    await page.goto(`/app/datasets/${datasetId}/validation`);
    const profile = page.getByLabel("Profile").first();
    const general = await profile.locator("option", { hasText: /general.*built in/i }).getAttribute("value");
    await profile.selectOption(general!);
    await page.getByRole("radio", { name: /Metadata only/ }).first().check();
    await page.getByRole("button", { name: "Start validation" }).click();
    await expect(page.getByRole("link", { name: "Open the job" })).toBeVisible();

    // Poll the jobs table until the job has a report.
    const openReport = page.getByRole("table", { name: "Validation jobs" }).getByRole("link", { name: "Open report" });
    await expect(async () => {
      await page.reload();
      await expect(openReport).toBeVisible({ timeout: 2_000 });
    }).toPass({ timeout: 90_000, intervals: [3_000] });
    await openReport.click();

    await expect(page.getByRole("heading", { name: "Verdict" })).toBeVisible();
    await expect(page.getByRole("main")).toContainText(/Ready with warnings|Ready|Failed/);
    const rules = page.getByRole("table", { name: "Rule results" });
    await expect(rules).toContainText(/Passed|Failed|Not checked|Partial/);
    await expect(page.getByText(/does not say .*semantic|nothing about semantic correctness/i).first()).toBeVisible();
    const text = (await page.getByRole("main").innerText()).toLowerCase();
    expect(text).not.toMatch(/score\s*[:=]?\s*\d/);
    expect(text).not.toMatch(/\d+(\.\d+)?\s*%\s*ready/);

    // Lineage: either edges in the table view or the honest empty state.
    await page.goto(`/app/datasets/${datasetId}/lineage`);
    const edges = page.getByRole("table", { name: "Lineage edges" });
    const tableView = page.getByRole("group", { name: "View" }).getByRole("button", { name: "Table" });
    if (await tableView.isVisible()) await tableView.click();
    const empty = page.getByText("No recorded lineage");
    await expect(edges.or(empty)).toBeVisible();
    if (await edges.isVisible()) {
      expect(await edges.getByRole("row").count()).toBeGreaterThan(1);
    }
  } finally {
    const headers = { Origin: new URL(page.url()).origin };
    if (datasetId) await page.request.post(`/api/proxy/datasets/${datasetId}/archive`, { headers });
    await deleteCatalogObject(page, objectId);
  }
});
