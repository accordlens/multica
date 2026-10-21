import "./env";

import { expect, test } from "@playwright/test";
import pg from "pg";
import { createTestApi } from "./helpers";

test("a pinned review subscription stays visible in the existing task", async ({ page }) => {
  test.setTimeout(120_000);
  const api = await createTestApi();
  const db = new pg.Client(process.env.DATABASE_URL);
  await db.connect();
  let runtimeId: string | undefined;
  let workflowId: string | undefined;
  const agents: string[] = [];
  try {
    const workspace = (await api.getWorkspaces())[0]!;
    const user = await db.query<{ id: string }>(`SELECT id FROM "user" WHERE email=$1`, [api.getEmail()]);
    const userId = user.rows[0]!.id;
    const runtime = await db.query<{ id: string }>(
      `INSERT INTO agent_runtime(workspace_id,name,runtime_mode,provider,status,device_info,metadata,owner_id,last_seen_at)
       VALUES($1,'Handoff fixture','cloud','e2e_wakeups','online','Synthetic fixture','{}',$2,now()) RETURNING id`,
      [workspace.id, userId],
    );
    runtimeId = runtime.rows[0]!.id;
    for (const role of ["Dev", "QA"]) {
      const agent = await db.query<{ id: string }>(
        `INSERT INTO agent(workspace_id,name,description,instructions,runtime_mode,runtime_config,runtime_id,visibility,permission_mode,max_concurrent_tasks,owner_id)
         VALUES($1,$2,'','','cloud','{}',$3,'private','private',1,$4) RETURNING id`,
        [workspace.id, `Handoff ${role}`, runtimeId, userId],
      );
      agents.push(agent.rows[0]!.id);
    }
    const steps = [
      { status_key: "todo" },
      { status_key: "in_progress", handler: { type: "agent", id: agents[0] } },
      { status_key: "in_review", handler: { type: "agent", id: agents[1] } },
      { status_key: "done" },
      { status_key: "cancelled" },
    ];
    const workflow = await db.query<{ id: string }>(
      `INSERT INTO issue_workflow(workspace_id,name,initial_status_key,steps) VALUES($1,'Review pilot','todo',$2) RETURNING id`,
      [workspace.id, JSON.stringify(steps)],
    );
    workflowId = workflow.rows[0]!.id;
    const project = await api.createProject("Local review pilot");
    const issue = await api.createIssue("Review one pinned revision", { status: "in_progress", project_id: project.id });
    // Adopt the workflow administratively, without entering a column that
    // intentionally assigns Dev and starts the existing workflow run.
    await db.query(`UPDATE project SET workflow_id=$2 WHERE id=$1`, [project.id, workflowId]);
    const root = await api.createComment(issue.id,
      "**Participants:** Handoff Dev and Handoff QA.\n\n**Expected response:** one review.result for revision aaaaaaaaaaaa.\n\n**Decision:** pending. This is a synthetic local pilot.");
    const headers = { Authorization: `Bearer ${api.getToken()}`, "Content-Type": "application/json", "X-Workspace-Slug": workspace.slug };
    const base = process.env.NEXT_PUBLIC_API_URL || `http://localhost:${process.env.PORT || "8080"}`;
    const definition = await fetch(`${base}/api/issue-workflows/${workflowId}`, { headers });
    expect(definition.status).toBe(200);
    const version = (await definition.json()).definition_version;
    for (const [sender, recipient] of [[agents[0], agents[1]], [agents[1], agents[0]]]) {
      const saved = await fetch(`${base}/api/issues/${issue.id}/wakeups`, {
        method: "POST", headers, body: JSON.stringify({
          agent_id: recipient, instruction: "Review only the pinned revision. Return one review.result in the source thread.",
          kind: "event", mode: "once", expires_in_seconds: 3600,
          condition: { type: "review_handoff", handoff: {
            thread_id: root.id, sender_id: sender, workflow_id: workflowId, workflow_version: version, source_revision: "a".repeat(40),
          } },
        }),
      });
      expect(saved.status).toBe(201);
    }

    await page.addInitScript((token) => {
      localStorage.setItem("multica_token", token!);
      localStorage.setItem("multica:chat:isOpen", "false");
    }, api.getToken());
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.goto(`/${workspace.slug}/issues/${issue.id}`, { waitUntil: "domcontentloaded" });
    const subscriptions = page.getByRole("button", { name: /When review is requested for aaaaaaaaaaaa/ });
    const subscription = subscriptions.filter({ hasText: "Handoff QA" });
    await expect(subscription).toBeVisible({ timeout: 45_000 });
    await expect(subscription).toContainText("Handoff QA");
    await expect(subscriptions.filter({ hasText: "Handoff Dev" })).toBeVisible();
    await expect(page.getByText("Expected response: one review.result for revision aaaaaaaaaaaa.", { exact: true })).toBeVisible();
    await api.createComment(issue.id, "Decision unchanged: waiting for the pinned review report.");
    await expect(page.locator("p").filter({ hasText: "Decision unchanged: waiting for the pinned review report." })).toBeVisible();
    await page.screenshot({ path: "test-results/review-handoff.png", fullPage: true });
    const runs = await db.query<{ n: number }>(`SELECT count(*)::int AS n FROM agent_task_queue WHERE issue_id=$1`, [issue.id]);
    expect(runs.rows[0]!.n).toBe(0);
  } finally {
    await api.cleanup();
    if (workflowId) await db.query(`DELETE FROM issue_workflow WHERE id=$1`, [workflowId]);
    for (const agent of agents) await db.query(`DELETE FROM agent WHERE id=$1`, [agent]);
    if (runtimeId) await db.query(`DELETE FROM agent_runtime WHERE id=$1`, [runtimeId]);
    await db.end();
  }
});
