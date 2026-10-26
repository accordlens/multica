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

test("two required inputs agree across API, reciprocal views and page reload", async ({ page }, testInfo) => {
  const api = await createTestApi();
  try {
    const workspace = await api.ensureWorkspace();
    const a = await api.createIssue("Required infrastructure result A", { status: "todo" });
    const b = await api.createIssue("Required authentication result B", { status: "todo" });
    const target = await api.createIssue("Integrate both required results", { status: "todo" });
    const context = await api.createIssue("Context without an execution gate", { status: "todo" });
    for (const source of [a, b]) {
      const input = { issue_id: source.id, condition: { kind: "done" } };
      const first = await api.createIssueRelation(target.id, input);
      const replay = await api.createIssueRelation(target.id, input);
      expect(replay.id).toBe(first.id);
    }
    await api.createIssueRelation(target.id, { issue_id: context.id, type: "related" });
    const initial = await api.getIssueRelations(target.id);
    expect(initial.ready).toBe(false);
    expect(initial.relations).toHaveLength(3);
    await page.addInitScript((token) => {
      localStorage.setItem("multica_token", token!);
      localStorage.setItem("multica:chat:isOpen", "false");
    }, api.getToken());
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.goto(`/${workspace.slug}/issues/${target.id}`, { waitUntil: "domcontentloaded" });
    const section = page.getByRole("region", { name: "Relations", exact: true });
    for (const source of [a, b, context]) {
      await expect(section.getByRole("link", { name: `${source.identifier} ${source.title}`, exact: true })).toHaveCount(1);
    }
    await expect(section.getByText("Inputs satisfied", { exact: true })).toHaveCount(0);
    await page.screenshot({ path: testInfo.outputPath("both-inputs-waiting.png"), fullPage: true });
    await api.updateIssue(a.id, { status: "done" });
    await api.updateIssue(b.id, { status: "in_review" });
    await expect(section.getByText("Condition met", { exact: true })).toHaveCount(1);
    expect((await api.getIssueRelations(target.id)).ready).toBe(false);
    await expect(section.getByText("Inputs satisfied", { exact: true })).toHaveCount(0);
    await api.updateIssue(b.id, { status: "done" });
    await expect(section.getByText("Inputs satisfied", { exact: true })).toBeVisible();
    expect((await api.getIssueRelations(target.id)).ready).toBe(true);
    await page.screenshot({ path: testInfo.outputPath("both-inputs-satisfied.png"), fullPage: true });
    const reciprocal = await api.getIssueRelations(a.id);
    expect(reciprocal.relations).toHaveLength(1);
    expect(reciprocal.relations[0]).toMatchObject({ type: "blocks", issue: { id: target.id } });
    await page.goto(`/${workspace.slug}/issues/${a.id}`, { waitUntil: "domcontentloaded" });
    await expect(section.getByText("Blocks", { exact: true })).toBeVisible();
    await expect(section.getByRole("link", { name: `${target.identifier} ${target.title}`, exact: true })).toBeVisible();
    await api.updateIssue(b.id, { status: "cancelled" });
    await page.goto(`/${workspace.slug}/issues/${target.id}`, { waitUntil: "domcontentloaded" });
    await expect(section.getByText("Waiting for issue completion", { exact: true })).toBeVisible();
    expect((await api.getIssueRelations(target.id)).ready).toBe(false);
    await page.reload({ waitUntil: "domcontentloaded" });
    await expect(section.getByRole("link", { name: `${b.identifier} ${b.title}`, exact: true })).toHaveCount(1);
    await expect(section.getByText("Inputs satisfied", { exact: true })).toHaveCount(0);
    await page.screenshot({ path: testInfo.outputPath("cancelled-input-after-reload.png"), fullPage: true });
  } finally { await api.cleanup(); }
});
