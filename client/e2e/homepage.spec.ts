import AxeBuilder from "@axe-core/playwright";
import { expect, test } from "@playwright/test";

const STOPS = [
  "question",
  "problem",
  "connect",
  "upload",
  "verify",
  "ingest",
  "catalog",
  "snapshot",
  "validate",
  "report",
  "readiness",
  "lineage",
  "retain",
  "access",
  "begin",
];

test.describe("homepage", () => {
  test("has fifteen titled sections and one h1", async ({ page }) => {
    await page.goto("/");
    await expect(page.locator("main > section")).toHaveCount(15);
    await expect(page.getByRole("heading", { level: 1 })).toHaveCount(1);
    for (const id of STOPS) {
      const heading = page.locator(`#${id}`).getByRole("heading").first();
      await expect(heading, id).toBeAttached();
    }
  });

  test("the live report says what was not checked, and changes with the mode", async ({ page }) => {
    await page.goto("/");
    const report = page.getByRole("group", { name: /example report/i });
    await expect(report).toContainText("Not checked");
    await expect(report).toContainText("Content was never read");

    await report.locator("label", { hasText: "Sampled" }).click();
    await expect(report).toContainText("Partial");
    await expect(report).toContainText("Read 100 of 1,204 objects");

    await report.locator("label", { hasText: "Full integrity" }).click();
    await expect(report).toContainText("Read 1,204 of 1,204 objects");
    // The verdict never turns into a score.
    await expect(report).toContainText("There is no score");
    await expect(report).not.toContainText("%");
  });

  test("the snapshot slider is keyboard operable and names each stage", async ({ page }) => {
    await page.goto("/");
    const slider = page.getByRole("slider", { name: /stages of a dataset/i });
    await slider.focus();
    await page.keyboard.press("ArrowRight");
    await expect(slider).toHaveAttribute("aria-valuetext", "Snapshot 1");
    await page.keyboard.press("End");
    await expect(slider).toHaveAttribute("aria-valuetext", "Snapshot 2");
  });

  test("readiness has nine dimensions, a filter, and no score", async ({ page }) => {
    await page.goto("/");
    const section = page.locator("#readiness");
    await expect(section).toContainText("There is no overall score");
    await section.getByLabel("Filter dimensions").fill("integ");
    await expect(section.getByRole("status")).toContainText("1 of 9");
  });

  test("roles show the real permission counts", async ({ page }) => {
    await page.goto("/");
    const tabs = page.locator("#access").getByRole("tablist");
    await expect(tabs.getByRole("tab", { name: /Owner/ })).toContainText("25 of 25");
    await tabs.getByRole("tab", { name: /Developer/ }).click();
    await expect(tabs.getByRole("tab", { name: /Developer/ })).toContainText("16 of 25");
    await tabs.getByRole("tab", { name: /Auditor/ }).click();
    const access = page.locator("#access");
    await access.getByRole("button", { name: /^Audit/ }).click();
    await expect(access).toContainText("Can read the audit log");
  });

  test("the closing form needs a name and bucket, then takes you to sign in without calling the API", async ({ page }) => {
    const calls: string[] = [];
    page.on("request", (request) => {
      if (request.url().includes("/api/")) calls.push(request.url());
    });
    await page.goto("/");
    const dialog = page.locator("#begin");
    const create = dialog.getByRole("button", { name: "Create connection" });
    await expect(create).toBeDisabled();
    await dialog.getByLabel("Name").fill("Production bucket");
    await dialog.getByLabel("Bucket").fill("my-training-data");
    await create.click();
    await expect(page).toHaveURL(/\/login/);
    expect(calls.filter((url) => url.includes("/api/proxy"))).toEqual([]);
  });

  for (const [name, width, height] of [
    ["desktop", 1440, 900],
    ["laptop", 1280, 800],
    ["phone", 390, 844],
  ] as const) {
    test(`no horizontal scroll on a ${name}`, async ({ page }) => {
      await page.setViewportSize({ width, height });
      await page.goto("/", { waitUntil: "networkidle" });
      const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
      expect(overflow).toBeLessThanOrEqual(0);
    });
  }

  for (const scheme of ["light", "dark"] as const) {
    test(`no serious axe violations in ${scheme} theme`, async ({ page }) => {
      await page.emulateMedia({ colorScheme: scheme });
      await page.goto("/", { waitUntil: "networkidle" });
      const results = await new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa"]).analyze();
      const serious = results.violations
        .filter((v) => v.impact === "serious" || v.impact === "critical")
        .map((v) => `${v.id}: ${v.nodes.slice(0, 3).map((n) => n.target.join(" ")).join(" | ")}`);
      expect(serious).toEqual([]);
    });
  }
});

test.describe("homepage scroll motion", () => {
  test.use({ reducedMotion: "no-preference" });

    test("the custody rail follows the reader through the stops", async ({ page }) => {
    await page.goto("/", { waitUntil: "networkidle" });
    const rail = page.getByRole("navigation", { name: /chain of custody/i });
    await expect(rail.locator('[data-state="upcoming"]')).toHaveCount(15 - 1);
    await page.locator("#snapshot").scrollIntoViewIfNeeded();
    await expect(rail.locator('a[aria-label="Snapshot"]')).toHaveAttribute("data-state", "current");
    await expect(rail.locator('a[aria-label="Connect"]')).toHaveAttribute("data-state", "passed");
    await expect(rail.locator('a[aria-label="Access"]')).toHaveAttribute("data-state", "upcoming");
  });

  test("the snapshot slider is keyboard operable and names each stage", async ({ page }) => {
    await page.goto("/");
    const slider = page.getByRole("slider", { name: /stages of a dataset/i });
    await slider.focus();
    await page.keyboard.press("ArrowRight");
    await expect(slider).toHaveAttribute("aria-valuetext", "Snapshot 1");
    await page.keyboard.press("End");
    await expect(slider).toHaveAttribute("aria-valuetext", "Snapshot 2");
  });

  test("readiness has nine dimensions, a filter, and no score", async ({ page }) => {
    await page.goto("/");
    const section = page.locator("#readiness");
    await expect(section).toContainText("There is no overall score");
    await section.getByLabel("Filter dimensions").fill("integ");
    await expect(section.getByRole("status")).toContainText("1 of 9");
  });

  test("roles show the real permission counts", async ({ page }) => {
    await page.goto("/");
    const tabs = page.locator("#access").getByRole("tablist");
    await expect(tabs.getByRole("tab", { name: /Owner/ })).toContainText("25 of 25");
    await tabs.getByRole("tab", { name: /Developer/ }).click();
    await expect(tabs.getByRole("tab", { name: /Developer/ })).toContainText("16 of 25");
    await tabs.getByRole("tab", { name: /Auditor/ }).click();
    const access = page.locator("#access");
    await access.getByRole("button", { name: /^Audit/ }).click();
    await expect(access).toContainText("Can read the audit log");
  });

  test("the closing form needs a name and bucket, then takes you to sign in without calling the API", async ({ page }) => {
    const calls: string[] = [];
    page.on("request", (request) => {
      if (request.url().includes("/api/")) calls.push(request.url());
    });
    await page.goto("/");
    const dialog = page.locator("#begin");
    const create = dialog.getByRole("button", { name: "Create connection" });
    await expect(create).toBeDisabled();
    await dialog.getByLabel("Name").fill("Production bucket");
    await dialog.getByLabel("Bucket").fill("my-training-data");
    await create.click();
    await expect(page).toHaveURL(/\/login/);
    expect(calls.filter((url) => url.includes("/api/proxy"))).toEqual([]);
  });

  for (const [name, width, height] of [
    ["desktop", 1440, 900],
    ["laptop", 1280, 800],
    ["phone", 390, 844],
  ] as const) {
    test(`no horizontal scroll on a ${name}`, async ({ page }) => {
      await page.setViewportSize({ width, height });
      await page.goto("/", { waitUntil: "networkidle" });
      const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
      expect(overflow).toBeLessThanOrEqual(0);
    });
  }

  for (const scheme of ["light", "dark"] as const) {
    test(`no serious axe violations in ${scheme} theme`, async ({ page }) => {
      await page.emulateMedia({ colorScheme: scheme });
      await page.goto("/", { waitUntil: "networkidle" });
      const results = await new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa"]).analyze();
      const serious = results.violations
        .filter((v) => v.impact === "serious" || v.impact === "critical")
        .map((v) => `${v.id}: ${v.nodes.slice(0, 3).map((n) => n.target.join(" ")).join(" | ")}`);
      expect(serious).toEqual([]);
    });
  }
});

test.describe("homepage scroll motion", () => {
  test.use({ reducedMotion: "no-preference" });

    test("the lifecycle token moves with the scroll, and the cut-out art loads", async ({ page }) => {
      await page.goto("/", { waitUntil: "networkidle" });
      const token = page.locator("[data-flow-token]");
      const strip = page.locator("[data-flow-line]").locator("..");
      const tokenX = () => token.evaluate((el) => el.getBoundingClientRect().x);
      const scrollStrip = (viewportY: number) =>
        strip.evaluate((el, y) => window.scrollTo(0, window.scrollY + el.getBoundingClientRect().top - y), viewportY);

      await scrollStrip(800);
      await page.waitForTimeout(800);
      const before = await tokenX();
      await scrollStrip(300);
      await page.waitForTimeout(800);
      expect(await tokenX()).toBeGreaterThan(before + 100);

      const broken = await page.locator("img[src^='/landing/']").evaluateAll((imgs) =>
        imgs.filter((img) => !(img as HTMLImageElement).complete || (img as HTMLImageElement).naturalWidth === 0).length,
      );
      expect(broken).toBe(0);
    });
});
