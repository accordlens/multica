import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { checks } from "./workflow-checks.mjs";

export function codeWorkflow(author, reviewer, checkout = "multica") {
  const uuid = /^[a-f0-9]{8}(-[a-f0-9]{4}){3}-[a-f0-9]{12}$/;
  if (!uuid.test(author) || !uuid.test(reviewer) || author === reviewer) throw new Error("Choose distinct author and reviewer agent UUIDs.");
  const files = Object.fromEntries(["scripts/workflow-checks.mjs"].map((path) => [path, createHash("sha256").update(readFileSync(path)).digest("hex")]));
  return { name: "Code change", description: "Commit → required checks → bounded repair → independent review → human acceptance", steps: [
    { status_key: "in_progress", handler: { type: "agent", id: author }, instructions: "Implement the requested change, commit it, and deliver the PR. The engine runs verification after your run completes.", next_status_key: "verify" },
    { new_status: { name: "Verify", category: "started" }, script: { id: "workflow-code-checks", version: "1", checkout, directory: ".", command: ["node", "scripts/workflow-checks.mjs"], required_env: ["CI"], timeout_seconds: 300, budget_seconds: 3600, max_repairs: 1, required_checks: checks.map(([name]) => name), files }, next_status_key: "in_review", back_status_key: "in_progress" },
    { status_key: "in_review", handler: { type: "agent", id: reviewer }, instructions: "Review the exact verified SHA and the attached report. Human acceptance remains required." },
    { status_key: "blocked" },
  ] };
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try { console.log(JSON.stringify(codeWorkflow(...process.argv.slice(2)), null, 2)); } catch (error) { console.error(error.message); process.exitCode = 1; }
}
