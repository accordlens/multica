import { test } from "node:test";
import assert from "node:assert/strict";
import { testCounts } from "./workflow-checks.mjs";
import { codeWorkflow } from "./workflow-template.mjs";

test("required Go test counts distinguish a skipped suite and a build-only success", () => {
  const events = [{ Action: "pass", Package: "workflow" }, { Action: "pass", Test: "TestA" }, { Action: "skip", Test: "TestB" }, { Action: "fail", Test: "TestC" }];
  assert.deepEqual(testCounts(events.map(JSON.stringify).join("\n")), { passed: 1, failed: 1, skipped: 1 });
  assert.deepEqual(testCounts('{"Action":"pass","Package":"empty"}\nno test files'), { passed: 0, failed: 0, skipped: 0 });
});
test("the code template freezes gate code and requires an independent reviewer", () => {
  const author = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";
  const reviewer = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb";
  assert.throws(() => codeWorkflow(author, author), /distinct/);
  const template = codeWorkflow(author, reviewer);
  const gate = template.steps[1].script;
  assert.equal(gate.max_repairs, 1);
  assert.match(gate.files["scripts/workflow-checks.mjs"], /^[a-f0-9]{64}$/);
  assert.deepEqual(gate.required_checks, ["workflow-unit", "process-cleanup"]);
});
