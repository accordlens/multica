import { test, expect } from "@playwright/test";
import { createTestApi } from "./helpers";

test("script run details show the stored revision, report and repair blocker", async ({ page }, testInfo) => {
  const api = await createTestApi();
  try {
    const workspace = (await api.getWorkspaces())[0]!;
    const issue = await api.createIssue("Verify the answer before independent review", { status: "blocked" });
    const receipt = await api.seedWorkflowScriptRun(issue.id);
    const token = api.getToken();
    if (!token) throw new Error("Test login did not return a token");
    await page.addInitScript((token) => {
      localStorage.setItem("multica_token", token);
      localStorage.setItem("multica:chat:isOpen", "false");
    }, token);
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.goto(`/${workspace.slug}/issues/${issue.id}`, { waitUntil: "domcontentloaded" });
    await page.getByRole("button", { name: "1 run", exact: true }).click();
    const dialog = page.getByRole("dialog");
    await dialog.getByRole("button", { name: "Run details", exact: true }).click();
    const evidence = dialog.getByRole("region", { name: "Workflow evidence" });
    await expect(evidence).toBeVisible();
    await expect(evidence).toContainText(receipt.sourceSHA);
    await expect(evidence).toContainText(receipt.reportHash);
    await expect(evidence).toContainText("repair attempt limit exhausted");
    await expect(evidence).toContainText("assertion_failed");
    await evidence.getByText("Report", { exact: true }).last().click();
    await expect(evidence.locator("pre").first()).toContainText('"failed":1');
    await page.screenshot({ path: testInfo.outputPath("workflow-script-evidence.png"), fullPage: true });
  } finally { await api.cleanup(); }
});
