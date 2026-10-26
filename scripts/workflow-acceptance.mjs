import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdirSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const modulePath = "github.com/multica-ai/multica/server";

export const databaseSuites = [
  {
    package: `${modulePath}/internal/service`,
    directory: "./internal/service",
    tests: [
      "TestScriptWorkflowDatabaseDependenciesAndRevisionEvidence",
      "TestScriptWorkflowDatabaseReopenPreservesCompletion",
      "TestScriptWorkflowDatabaseRejectsStaleEvidence",
      "TestScriptWorkflowDatabaseSuccessorFailureRollsBack",
      "TestScriptWorkflowPilotFailRepairPassIndependentReview",
      "TestScriptWorkflowDuplicateCompletionCreatesOneSuccessor",
      "TestScriptWorkflowRejectsNoProgressAndRepairLimit",
      "TestScriptWorkflowOfflineCancelAndStaleReportBlockWithoutReplay",
      "TestScriptWorkflowBudgetExpiresQueuedWorkOnExistingSweep",
    ],
  },
  {
    package: `${modulePath}/internal/handler`,
    directory: "./internal/handler",
    tests: [
      "TestIssueDependencyConcurrentCycleAndReplay",
      "TestIssueDependencyANDGateClaimsOnceAndReopens",
      "TestIssueDependencyHumanAcceptanceAndRemovalHistory",
      "TestIssueDependencyNamedRunDoesNotCompleteItsParent",
      "TestIssueDependencyInfrastructureIntegrationHasNoCycleOrImplicitAcceptance",
      "TestIssueDependencyTenantIsolation",
      "TestIssueDependencyLinearChainAndCancelledInput",
      "TestIssueContextLinksExplicitReferencesAndCorrection",
      "TestIssueContextLinkEmitsOnlyMetadataInvalidation",
      "TestIssueProvenanceNativeEvidenceAutomaticallyLinksCommitAndRetainsCorrection",
      "TestIssueProvenanceRevisionHistoryReplayAndACL",
      "TestIssueProvenancePRCanLinkMultipleTasksWithoutWebhook",
      "TestIssueProvenanceRetainsChatPrivacyAfterLivePointersDisappear",
      "TestScriptWorkflowStatusGateCannotBeBypassedWithAssignmentOrDone",
      "TestScriptGateDefinitionAndBindingCannotBeChangedByRepairAgent",
      "TestScriptWorkflowClaimPinsDefinitionAndObserverCannotAcquireGate",
      "TestScriptWorkflowForeignIssueAgentCannotSupplyHumanAcceptance",
    ],
  },
];

export function validateGoEvents(events, suite) {
  const problems = [];
  const passed = new Set();
  let packagePassed = false;
  for (const event of events) {
    if (event.Package !== suite.package) continue;
    if (event.Action === "skip" || event.Action === "fail") {
      problems.push(`${event.Test || suite.package}: ${event.Action}`);
    }
    if (event.Action === "pass") {
      if (event.Test) passed.add(event.Test);
      else packagePassed = true;
    }
  }
  if (!packagePassed) problems.push("package did not finish successfully");
  for (const name of suite.tests) {
    if (!passed.has(name)) problems.push(`${name}: required test did not pass`);
  }
  return { passed: problems.length === 0, problems };
}

function run(command, args, options = {}) {
  const child = spawnSync(command, args, {
    cwd: root, encoding: "utf8", windowsHide: true, maxBuffer: 64 * 1024 * 1024,
    ...options,
  });
  if (child.error) throw child.error;
  return child;
}

function main(args) {
  if (args[0] === "--plan") {
    console.log(JSON.stringify(databaseSuites, null, 2));
    return;
  }
  if (args.length !== 2 || args[0] !== "--out") {
    throw new Error("Usage: node scripts/workflow-acceptance.mjs --plan | --out RECEIPT_DIRECTORY");
  }
  if (!process.env.DATABASE_URL) {
    throw new Error("DATABASE_URL must name the admitted disposable test database; no localhost fallback is used");
  }
  const status = run("git", ["status", "--porcelain", "--untracked-files=all"]);
  if (status.status !== 0) throw new Error("Cannot verify source cleanliness");
  if (status.stdout.trim()) {
    throw new Error("Commit the acceptance tests first; database receipts require a clean source revision");
  }
  const source = run("git", ["rev-parse", "HEAD"]);
  if (source.status !== 0) throw new Error("Cannot identify source revision");
  const output = resolve(args[1]);
  mkdirSync(output, { recursive: true });
  const receipt = { source_sha: source.stdout.trim(), started_at: new Date().toISOString(), database: "PostgreSQL", platform: process.platform, node_version: process.version, suites: [] };
  for (const suite of databaseSuites) {
    const command = ["test", "-json", "-p", "1", "-count=1", "-timeout=180s", suite.directory, "-run", `^(${suite.tests.join("|")})$`];
    let child;
    try {
      child = run("go", command, { cwd: resolve(root, "server"), env: { ...process.env, GOMAXPROCS: "2" } });
    } catch (error) {
      child = { status: null, stdout: "", stderr: error.message };
    }
    let result;
    try {
      const events = child.stdout.split(/\r?\n/).filter(Boolean).map((line) => JSON.parse(line));
      result = validateGoEvents(events, suite);
    } catch {
      result = { passed: false, problems: ["invalid or truncated go test JSON stream"] };
    }
    if (child.status !== 0 || child.signal) result.problems.push(`go exited ${child.status ?? child.signal ?? "unavailable"}`);
    result.passed = result.problems.length === 0;
    const log = `${child.stdout}\n${child.stderr}`.replace(/postgres(?:ql)?:\/\/[^\s"'<>]+/gi, "[redacted database URL]");
    const logName = `${suite.directory.split("/").at(-1)}-go.jsonl`;
    writeFileSync(resolve(output, logName), log);
    receipt.suites.push({ package: suite.package, command: ["go", ...command], ...result, log: logName, log_sha256: createHash("sha256").update(log).digest("hex") });
    if (!result.passed) break;
  }
  receipt.finished_at = new Date().toISOString();
  receipt.passed = receipt.suites.length === databaseSuites.length && receipt.suites.every((suite) => suite.passed);
  writeFileSync(resolve(output, "workflow-database.json"), JSON.stringify(receipt, null, 2));
  console.log(JSON.stringify(receipt, null, 2));
  if (!receipt.passed) process.exitCode = 1;
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  try { main(process.argv.slice(2)); }
  catch (error) { console.error(error.message); process.exitCode = 1; }
}
