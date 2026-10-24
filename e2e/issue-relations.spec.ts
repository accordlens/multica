import "./env";
import { expect, test } from "@playwright/test";
import { createTestApi } from "./helpers";

test("dependencies retain the human gate through delivery, reopen and reconnect", async ({ page }, testInfo) => {
  test.setTimeout(120_000);
  const api = await createTestApi();
  try {
    const workspace = (await api.getWorkspaces())[0]!;
    const source = await api.createIssue("Infrastructure readiness", { status: "in_review" });
    const target = await api.createIssue("Configure HTTPS and authentication", { status: "todo" });
    await page.addInitScript((token) => {
      localStorage.setItem("multica_token", token!);
      localStorage.setItem("multica:chat:isOpen", "false");
    }, api.getToken());
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.goto(`/${workspace.slug}/issues/${target.id}`, { waitUntil: "domcontentloaded" });
    const section = page.getByRole("region", { name: "Relations", exact: true });
    await section.getByRole("button", { name: "Add relation", exact: true }).click();
    const form = section.getByRole("form", { name: "Add relation" });
    await form.getByLabel("Issue ID or key").fill(source.identifier);
    await form.getByLabel("Required result").click();
    await page.getByRole("option", { name: "Explicit human acceptance", exact: true }).click();
    await form.getByRole("button", { name: "Add relation", exact: true }).click();
    await expect(form).toBeHidden();
    await expect(section.getByText("Waiting for human acceptance", { exact: true })).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath("relations.png"), fullPage: true });
    await section.getByRole("button", { name: /^Accept revision/ }).click();
    await expect(section.getByText("Inputs satisfied", { exact: true })).toBeVisible();
    await api.updateIssue(source.id, { status: "in_progress" });
    await expect(section.getByText("Waiting for human acceptance", { exact: true })).toBeVisible();
    await page.reload({ waitUntil: "domcontentloaded" });
    await expect(section.getByRole("link", { name: new RegExp(source.identifier) })).toHaveCount(1);
    await section.getByRole("button", { name: `Remove relation to ${source.identifier}` }).click();
    await expect(section.getByRole("link", { name: new RegExp(source.identifier) })).toHaveCount(0);
  } finally { await api.cleanup(); }
});
