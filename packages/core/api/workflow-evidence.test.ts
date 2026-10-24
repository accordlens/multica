// @vitest-environment node
import { describe, expect, it } from "vitest";
import { AgentTaskSchema, IssueWorkflowSchema } from "./schemas";

const script = { id: "unit", version: "1", checkout: "repo", directory: ".", command: ["node", "gate.mjs"], required_env: [], timeout_seconds: 5, budget_seconds: 60, max_repairs: 1, required_checks: ["unit"], files: { "gate.mjs": "a".repeat(64) } };
const evidence = { run_id: "run", source: { sha: "a".repeat(40) }, workflow_version: "version", script_version: "1", outcome: "passed", exit_code: 0, started_at: "start", finished_at: "end", report: '{"checks":[]}', report_id: "report", report_hash: "hash" };

describe("workflow evidence API compatibility", () => {
  it("retains script definitions and exact report text", () => {
    const workflow = IssueWorkflowSchema.parse({ id: "wf", workspace_id: "ws", name: "Code", created_at:"created",updated_at:"updated",steps: [{ status_key: "verify", script }] });
    expect(workflow.steps[0]?.script).toEqual(script);
    const task = AgentTaskSchema.parse({ id: "run", workflow_evidence: evidence, workflow_execution: { kind: "script", root_task_id: "root", workflow_id: "wf", workflow_version: "version", step_key: "verify", source: evidence.source, script, repairs: 0, deadline: "deadline" } });
    expect(task.workflow_evidence?.report).toBe(evidence.report);
    expect(task.workflow_execution?.script).toEqual(script);
  });
  it("drops malformed optional display evidence without hiding the run", () => {
    const task = AgentTaskSchema.parse({ id: "run", workflow_evidence: { report: 42 }, workflow_execution: { kind: "future" } });
    expect(task.id).toBe("run");
    expect(task.workflow_evidence).toBeUndefined();
    expect(task.workflow_execution).toBeUndefined();
  });
  it("refuses a malformed gate definition instead of silently removing its checks", () => {
    expect(IssueWorkflowSchema.safeParse({ id: "wf", workspace_id: "ws", name: "Code", steps: [{ status_key: "verify", script: { command: "rm" } }] }).success).toBe(false);
  });
});
