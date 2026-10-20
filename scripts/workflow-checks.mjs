import { spawnSync } from "node:child_process";
import { writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

// Project policy: the workflow pilot changes the workflow engine and process
// ownership. Full repository CI remains required separately before PR push.
export const checks = [
  ["workflow-unit", ["go", "-C", "server", "test", "-json", "-count=1", "./internal/issueworkflow"]],
  ["process-cleanup", ["go", "-C", "server", "test", "-json", "-count=1", "./internal/daemon/processtree"]],
];

export function testCounts(output) {
  const result = { passed: 0, failed: 0, skipped: 0 };
  for (const line of output.split("\n")) {
    let event;
    try { event = JSON.parse(line); } catch { continue; }
    if (!event.Test) continue;
    if (event.Action === "pass") result.passed++;
    if (event.Action === "fail") result.failed++;
    if (event.Action === "skip") result.skipped++;
  }
  return result;
}

export function run(env = process.env) {
  const required = ["MULTICA_WORKFLOW_REPORT", "MULTICA_WORKFLOW_RUN_ID", "MULTICA_WORKFLOW_SOURCE_SHA", "MULTICA_WORKFLOW_VERSION", "MULTICA_WORKFLOW_SCRIPT_VERSION"];
  if (required.some((key) => !env[key])) throw new Error("Invoke this registered gate through the workflow runtime.");
  const report = { run_id: env.MULTICA_WORKFLOW_RUN_ID, source_sha: env.MULTICA_WORKFLOW_SOURCE_SHA, workflow_version: env.MULTICA_WORKFLOW_VERSION, script_version: env.MULTICA_WORKFLOW_SCRIPT_VERSION, checks: [] };
  let exit = 0;
  for (const [name, command] of checks) {
    const result = spawnSync(command[0], command.slice(1), { env, encoding: "utf8", maxBuffer: 16 * 1024 * 1024, timeout: 120000 });
    const counts = testCounts(result.stdout ?? "");
    process.stdout.write(result.stdout ?? "");
    process.stderr.write(result.stderr ?? result.error?.message ?? "");
    report.checks.push({ name, ...counts });
    if (result.status !== 0 || counts.failed || counts.skipped || !counts.passed) exit = 1;
    // A build/tool/transport failure may accompany passing test events. Keep
    // that distinct from an assertion by making the required check incomplete.
    if (result.status !== 0 && !counts.failed) {
      report.infrastructure_error = result.error?.message ?? `Check ${name} exited before a test assertion failed`;
    }
  }
  writeFileSync(env.MULTICA_WORKFLOW_REPORT, JSON.stringify(report));
  return exit;
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try { process.exitCode = run(); } catch (error) { console.error(error.message); process.exitCode = 1; }
}
