import assert from "node:assert/strict";
import { test } from "node:test";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { validateGoEvents } from "./workflow-acceptance.mjs";

const suite = { package: "workflow/service", tests: ["TestAND", "TestRollback"] };
const event = (Action, Test) => ({ Action, Test, Package: suite.package });
const passed = [event("pass", "TestAND"), event("pass", "TestRollback"), event("pass")];

test("an unavailable DB exiting zero in TestMain cannot provide acceptance", () => {
  assert.equal(validateGoEvents([event("output"), event("pass")], suite).passed, false);
  assert.equal(validateGoEvents([], suite).passed, false);
});

test("skipped DB tests and skipped subtests invalidate a successful package", () => {
  for (const name of ["TestRollback", "TestAND/concurrent_claim"]) {
    assert.equal(validateGoEvents([...passed, event("skip", name)], suite).passed, false);
  }
});

test("failures, missing filters and foreign package names cannot satisfy the required list", () => {
  assert.equal(validateGoEvents([...passed, event("fail", "TestAND")], suite).passed, false);
  assert.equal(validateGoEvents(passed.filter((row) => row.Test !== "TestRollback"), suite).passed, false);
  assert.equal(validateGoEvents(passed.map((row) => ({ ...row, Package: "workflow/offline" })), suite).passed, false);
});

test("all named tests plus a completed package produce acceptance", () => {
  assert.deepEqual(validateGoEvents(passed, suite), { passed: true, problems: [] });
  assert.equal(validateGoEvents(passed.slice(0, -1), suite).passed, false);
});

test("the actual runner refuses an absent disposable DATABASE_URL before starting Go", () => {
  const runner = spawnSync(process.execPath, [fileURLToPath(new URL("./workflow-acceptance.mjs", import.meta.url)), "--out", "unused"], {
    encoding: "utf8", windowsHide: true, env: { ...process.env, DATABASE_URL: "" },
  });
  assert.equal(runner.status, 1);
  assert.match(runner.stderr, /DATABASE_URL must name/);
  assert.equal(runner.stdout, "");
});
