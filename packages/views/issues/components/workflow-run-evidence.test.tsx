// @vitest-environment jsdom
import { cleanup, screen } from "@testing-library/react";
import { afterEach, expect, it } from "vitest";
import type { AgentTask } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";
import { WorkflowRunEvidence } from "./workflow-run-evidence";
import { canRetryRun } from "./retry-run-button";

afterEach(cleanup);

it("shows the current report, revision and durable blocker without a model usage claim", () => {
  const task: AgentTask = {
    agent_id: "author", runtime_id: "local", issue_id: "issue", priority: 0, dispatched_at: null, started_at: null, completed_at: null, created_at: "", result: null, error: null,
    id: "run", status: "completed",
    workflow_execution: { kind: "script", root_task_id: "root", workflow_id: "workflow", workflow_version: "definition-hash", step_key: "verify", source: { sha: "new-sha" }, script: { id: "unit", version: "1", checkout: "repo", directory: ".", command: ["node"], required_env: [], timeout_seconds: 5, budget_seconds: 60, max_repairs: 1, required_checks: ["unit"], files: {} }, repairs: 1, deadline: "deadline", block_reason: "repair attempt limit exhausted" },
    workflow_evidence: { run_id: "run", source: { sha: "new-sha" }, workflow_version: "definition-hash", outcome: "assertion_failed", exit_code: 1, started_at: "start", finished_at: "end", report_id: "report-id", report_hash: "report-hash", report: '{"failed":1}', log: "assertion failed" },
  };
  renderWithI18n(<WorkflowRunEvidence task={task} />);
  expect(screen.getByRole("region", { name: "Workflow evidence" })).toBeTruthy();
  expect(screen.getByText("new-sha")).toBeTruthy();
  expect(screen.getByText("repair attempt limit exhausted")).toBeTruthy();
  expect(screen.getByText("1/1")).toBeTruthy();
  expect(screen.getByText('{"failed":1}')).toBeTruthy();
  expect(canRetryRun({ ...task, status: "failed" })).toBe(false);
});
