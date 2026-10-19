import { test, expect } from "@playwright/test";
import { readFile } from "node:fs/promises";
import { nativeReleaseCollections, nativeScenarios } from "./fixtures/native-release-data";

function parseCSV(text: string): string[][] {
  const rows: string[][] = [];
  let row: string[] = [];
  let cell = "";
  let quoted = false;
  for (let index = 0; index < text.length; index++) {
    const char = text[index];
    if (char === '"') {
      if (quoted && text[index + 1] === '"') {
        cell += '"';
        index++;
      } else quoted = !quoted;
    } else if (!quoted && (char === "," || char === "\n")) {
      row.push(cell);
      cell = "";
      if (char === "\n") {
        rows.push(row);
        row = [];
      }
    } else cell += char;
  }
  expect(quoted).toBe(false);
  row.push(cell);
  rows.push(row);
  return rows;
}

for (const scenario of nativeScenarios) {
  test(`producer native ${scenario} remains a non-passing collection with exact attempt evidence`, async ({ page }) => {
    const collections = await nativeReleaseCollections(scenario);
    const attempts = collections[0]?.decision.native?.attempts;
    expect(attempts?.length).toBe(scenario === "retry" ? 4 : scenario.endsWith("budget") ? 1 : 2);
    if (!attempts?.length) throw new Error("Producer fixture must contain native attempt evidence.");
    await page.route(/\/api\/release-collections$/, (route) => route.fulfill({ json: collections }));
    await page.goto("/#/release");
    await expect(page.getByText("Selected policy: NOT PASSED", { exact: true })).toBeVisible();
    const evidence = page.getByRole("region", { name: "Native collection evidence", exact: true });
    await expect(evidence.getByText("copilot_sdk_single_turn_text / 1.0", { exact: true })).toBeVisible();
    await expect(evidence.getByText("Declared client tool policy: deny all. This is not server isolation.", { exact: true })).toBeVisible();
    await expect(evidence.getByText(/unknown required usage blocks new starts.*Asynchronous overshoot is possible/)).toBeVisible();
    await expect(evidence.getByText("unsupported", { exact: true })).toBeVisible();
    await expect(evidence.getByText("unavailable", { exact: true })).toHaveCount(2);
    const rows = evidence.locator("tbody tr");
    await expect(rows).toHaveCount(attempts.length);
    for (const [index, attempt] of attempts.entries()) {
      const row = rows.nth(index);
      await expect(row.getByRole("rowheader")).toContainText(
        `${attempt.key.arm} / ${attempt.key.cluster_id} / ${attempt.key.task_id} / trial ${attempt.key.trial_ordinal} / attempt ${attempt.key.attempt_ordinal}`,
      );
      await expect(row.getByRole("rowheader")).toContainText(`Eval: ${attempt.key.eval_id}`);
      await expect(row.getByText(`Status: ${attempt.status}`, { exact: true })).toBeVisible();
      await expect(row.getByText(`Category: ${attempt.category}`, { exact: true })).toBeVisible();
      await expect(row.getByText(`Output: ${attempt.output_availability}`, { exact: true })).toBeVisible();
      await expect(row.getByText(`Usage: ${attempt.usage_complete ? "complete" : "incomplete / unavailable"}`, { exact: true })).toBeVisible();
      await expect(row.getByText(`Received event models: ${attempt.event_models?.length ? attempt.event_models.join(", ") : "unavailable"}`, { exact: true })).toBeVisible();
      await expect(row.getByText(`Accounting models: ${attempt.accounting_models?.length ? attempt.accounting_models.join(", ") : "unavailable"}`, { exact: true })).toBeVisible();
      await expect(row.locator("summary").filter({ hasText: /^Actual output/ })).toHaveCount(attempt.output === undefined ? 0 : 1);
      if (attempt.error) await expect(row.getByText(`Error: ${attempt.error}`, { exact: true })).toBeVisible();
    }
    if (scenario === "retry") {
      await expect(page.getByText("First-attempt passes: 0; retry-policy passes: 1; recovered: 1.", { exact: true })).toHaveCount(2);
      await expect(page.getByText("First-attempt rate: 0.0%; retry-policy rate: 100.0%.", { exact: true })).toHaveCount(2);
      await expect(page.getByText(/golden first attempt failed \(recovery cannot erase it\)/)).toHaveCount(2);
    }
    if (scenario === "unknown-usage") {
      await expect(page.getByText(/ai_credits : unavailable \(unknown\)/)).toHaveCount(2);
      await expect(page.getByText(/ai_credits : 0/)).toHaveCount(0);
    }
    if (scenario === "unknown-budget") {
      const baseline = page.getByRole("heading", { name: "baseline", exact: true }).locator("..");
      const candidate = page.getByRole("heading", { name: "candidate", exact: true }).locator("..");
      await expect(baseline.getByText(/ai_credits : unavailable \(unknown\)/)).toBeVisible();
      await expect(baseline.getByText(/ai_credits : 0/)).toHaveCount(0);
      await expect(candidate.getByText("ai_credits : 0 (final_complete_attributable)", { exact: true })).toBeVisible();
    }
  });
}

test("native output and lifecycle details support keyboard access and mobile scrolling", async ({ page }) => {
  const collections = await nativeReleaseCollections("retry");
  await page.setViewportSize({ width: 390, height: 844 });
  await page.route(/\/api\/release-collections$/, (route) => route.fulfill({ json: collections }));
  await page.goto("/#/release");
  const observations = page.getByRole("region", { name: "Native attempt observations", exact: true });
  await observations.focus();
  await expect(observations).toBeFocused();
  const row = observations.locator("tbody tr").nth(1);
  const output = row.locator("summary").filter({ hasText: /^Actual output/ });
  await output.focus();
  await page.keyboard.press("Enter");
  await expect(row.locator("pre")).toHaveText("hello");
  const lifecycle = row.locator("summary").filter({ hasText: /^Lifecycle/ });
  await lifecycle.focus();
  await page.keyboard.press("Enter");
  await expect(row.locator("dt")).toHaveText(["construction", "execute", "grade", "initialize", "shutdown"]);
  await expect(row.locator("dd")).toHaveText(["completed", "completed", "completed", "completed", "completed"]);
  await page.keyboard.press("Enter");
  await expect(row.locator("dd").first()).not.toBeVisible();
});

test("native failed lifecycle retains its reason and unattempted prerequisites", async ({ page }) => {
  await page.route(/\/api\/release-collections$/, async (route) =>
    route.fulfill({ json: await nativeReleaseCollections("initialize") }));
  await page.goto("/#/release");
  const row = page.getByRole("region", { name: "Native attempt observations", exact: true }).locator("tbody tr").first();
  await row.locator("summary").filter({ hasText: /^Lifecycle/ }).click();
  await expect(row.locator("dt").filter({ hasText: /^initialize$/ }).locator("..").locator("dd")).toHaveText(
    "failed: copilot diagnostic: initialize/start_failed",
  );
  await expect(row.locator("dt").filter({ hasText: /^execute$/ }).locator("..").locator("dd")).toHaveText(
    "not_attempted: Prior lifecycle prerequisite did not complete.",
  );
});

for (const scenario of ["retry", "unknown-usage"] as const) {
  test(`native ${scenario} JSON and CSV downloads preserve row identities, outputs and unavailable usage`, async ({ page }) => {
    const collections = await nativeReleaseCollections(scenario);
    const collection = collections[0];
    if (!collection?.decision.native?.attempts?.length) throw new Error("Producer fixture must contain native attempts.");
    await page.route(/\/api\/release-collections$/, (route) => route.fulfill({ json: collections }));
    await page.goto("/#/release");
    const jsonEvent = page.waitForEvent("download");
    await page.getByRole("button", { name: "Export decision JSON", exact: true }).click();
    const json = await jsonEvent;
    expect(json.suggestedFilename()).toBe("waza-release-decision.json");
    const jsonPath = await json.path();
    expect(jsonPath).not.toBeNull();
    expect(JSON.parse(await readFile(jsonPath!, "utf8"))).toEqual(collection);
    const csvEvent = page.waitForEvent("download");
    await page.getByRole("button", { name: "Export native attempts CSV", exact: true }).click();
    const csv = await csvEvent;
    expect(csv.suggestedFilename()).toBe("waza-native-attempts.csv");
    const csvPath = await csv.path();
    expect(csvPath).not.toBeNull();
    const [headers, ...rows] = parseCSV(await readFile(csvPath!, "utf8"));
    expect(headers).toBeDefined();
    if (!headers) throw new Error("CSV header missing.");
    expect(rows).toHaveLength(collection.decision.native.attempts.length);
    for (const [index, attempt] of collection.decision.native.attempts.entries()) {
      const row = rows[index];
      expect(row).toHaveLength(headers.length);
      const value = (name: string) => {
        const column = headers.indexOf(name);
        expect(column).toBeGreaterThanOrEqual(0);
        return row?.[column];
      };
      expect(value("Arm")).toBe(attempt.key.arm);
      expect(value("Cluster")).toBe(attempt.key.cluster_id);
      expect(value("Task")).toBe(attempt.key.task_id);
      expect(value("Trial")).toBe(String(attempt.key.trial_ordinal));
      expect(value("Eval")).toBe(attempt.key.eval_id);
      expect(value("Attempt")).toBe(String(attempt.key.attempt_ordinal));
      expect(value("Status")).toBe(attempt.status);
      expect(value("Category")).toBe(attempt.category);
      expect(value("Error")).toBe(attempt.error);
      expect(value("Output availability")).toBe(attempt.output_availability);
      expect(value("Output present")).toBe(String(attempt.output !== undefined));
      expect(value("Output")).toBe(attempt.output ?? "");
      expect(value("Usage complete")).toBe(String(attempt.usage_complete));
      expect(JSON.parse(value("Arm usage JSON") ?? "")).toEqual(collection.decision.usage[attempt.key.arm]);
      expect(JSON.parse(value("Lifecycle JSON") ?? "")).toEqual(attempt.lifecycle);
      expect(JSON.parse(value("Event models JSON") ?? "")).toEqual(attempt.event_models);
      expect(JSON.parse(value("Accounting models JSON") ?? "")).toEqual(attempt.accounting_models);
      expect(value("Client tool policy")).toBe(collection.decision.native.client_tool_policy);
      expect(value("Budget contract")).toBe(collection.decision.native.budget_contract);
    }
  });
}

for (const malformed of ["version", "profile", "usage_complete", "models", "output_missing", "output_unavailable",
  "lifecycle", "ordinal", "duplicate"]) {
  test(`native admission rejects mutated producer ${malformed}`, async ({ page }) => {
    const collections = await nativeReleaseCollections("passed");
    const collection = collections[0];
    const native = collection?.decision.native;
    const first = native?.attempts?.[0];
    if (!collection || !native || !first) throw new Error("Producer fixture must contain native attempts.");
    const attempt: Record<string, unknown> = { ...first };
    const badNative: Record<string, unknown> = { ...native, attempts: [attempt] };
    if (malformed === "version") badNative.version = "2.0";
    if (malformed === "profile") badNative.profile = "unknown_profile";
    if (malformed === "usage_complete") attempt.usage_complete = "true";
    if (malformed === "models") attempt.event_models = [1];
    if (malformed === "output_missing") delete attempt.output;
    if (malformed === "output_unavailable") attempt.output_availability = "unavailable";
    if (malformed === "lifecycle") attempt.lifecycle = { initialize: { state: false } };
    if (malformed === "ordinal") attempt.key = { ...first.key, attempt_ordinal: 1.5 };
    if (malformed === "duplicate") badNative.attempts = [first, first];
    await page.route(/\/api\/release-collections$/, (route) => route.fulfill({
      json: [{ ...collection, decision: { ...collection.decision, native: badNative } }],
    }));
    await page.goto("/#/release");
    await expect(page.getByRole("alert")).toHaveText("Invalid release decision view; not passed.");
    await expect(page.getByRole("table")).toHaveCount(0);
  });
}
