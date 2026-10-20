import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import { chmodSync, copyFileSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { gates, markdown, requirePass, run, testEnvironment, verifyPush } from "./local-ci.mjs";

const root = dirname(dirname(fileURLToPath(import.meta.url)));
const env = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_NOSYSTEM: "1", GIT_CONFIG_COUNT: "0" };
for (const name of execFileSync("git", ["rev-parse", "--local-env-vars"], { encoding: "utf8" }).trim().split("\n")) delete env[name];
const git = (cwd, ...args) => execFileSync("git", args, { cwd, env, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }).trim();
function fixture(t) {
  const directory = mkdtempSync(join(tmpdir(), "local-ci-test-"));
  t.after(() => rmSync(directory, { recursive: true, force: true }));
  const repo = join(directory, "repo");
  mkdirSync(repo);
  git(repo, "init", "-b", "main");
  git(repo, "config", "user.name", "CI Fixture");
  git(repo, "config", "user.email", "ci@example.invalid");
  git(repo, "config", "commit.gpgsign", "false");
  for (const file of ["scripts/local-ci.mjs", "scripts/install-git-hooks.mjs", ".githooks/pre-push", ".githooks/prepare-commit-msg"]) {
    mkdirSync(dirname(join(repo, file)), { recursive: true });
    copyFileSync(join(root, file), join(repo, file));
    if (file.startsWith(".githooks/")) chmodSync(join(repo, file), 0o755);
  }
  git(repo, "add", ".");
  git(repo, "commit", "-m", "fixture");
  const sha = git(repo, "rev-parse", "HEAD");
  const report = { schema: 1, sha, state: "success", finishedAt: "2026-10-20T00:00:00Z", gates: gates.map(([name, command]) => ({ name, command, state: "success", exitCode: 0 })) };
  const receipt = join(repo, ".git/local-ci/latest.json");
  mkdirSync(dirname(receipt), { recursive: true });
  const save = (value = report) => writeFileSync(receipt, JSON.stringify(value));
  return { repo, directory, sha, report, receipt, save, input: `refs/heads/main ${sha} refs/heads/main ${"0".repeat(40)}\n` };
}

test("real push rejects missing evidence, accepts an earlier PASS, rejects the next commit", (t) => {
  const f = fixture(t);
  const remote = join(f.directory, "remote.git");
  git(f.repo, "init", "--bare", remote);
  git(f.repo, "remote", "add", "origin", remote);
  execFileSync(process.execPath, ["scripts/install-git-hooks.mjs"], { cwd: f.repo, env });
  const push = () => spawnSync("git", ["push", "origin", "HEAD:refs/heads/checked"], { cwd: f.repo, env, encoding: "utf8" });
  assert.notEqual(push().status, 0);
  f.save();
  assert.equal(push().status, 0);
  assert.equal(git(f.repo, "ls-remote", "origin", "refs/heads/checked").split("\t")[0], f.sha);
  git(f.repo, "commit", "--allow-empty", "-m", "next commit");
  assert.notEqual(push().status, 0);
  assert.equal(JSON.parse(readFileSync(f.receipt)).finishedAt, f.report.finishedAt);
});

test("dirty tracked, staged and untracked source blocks reuse", (t) => {
  const f = fixture(t);
  f.save();
  writeFileSync(join(f.repo, "untracked.txt"), "new source");
  assert.throws(() => verifyPush(f.repo, f.input), /Checkout changed/);
  git(f.repo, "add", "untracked.txt");
  assert.throws(() => verifyPush(f.repo, f.input), /Checkout changed/);
  git(f.repo, "reset", "--", "untracked.txt");
  rmSync(join(f.repo, "untracked.txt"));
  writeFileSync(join(f.repo, "scripts/local-ci.mjs"), "changed source");
  assert.throws(() => verifyPush(f.repo, f.input), /Checkout changed/);
});

test("every required gate must pass with its current command", (t) => {
  const f = fixture(t);
  for (const mutate of [
    (report) => report.gates.pop(),
    (report) => report.gates.reverse(),
    (report) => report.gates[0].command.push("--skip"),
    (report) => report.gates[0].exitCode = 1,
    ...["failure", "pending", "skipped"].map((state) => (report) => report.gates[0].state = state),
    (report) => report.state = "failure",
    (report) => report.sha = "0".repeat(40),
  ]) {
    const invalid = structuredClone(f.report);
    mutate(invalid);
    assert.throws(() => requirePass(invalid, f.sha), /No complete passing/);
  }
  f.save({ ...f.report, gates: f.report.gates.slice(1) });
  assert.throws(() => verifyPush(f.repo, f.input), /No complete passing/);
  writeFileSync(f.receipt, "invalid json");
  assert.throws(() => verifyPush(f.repo, f.input), /No local CI result/);
});

test("multiple refs cannot hide another branch; annotated tags are peeled", (t) => {
  const f = fixture(t);
  f.save();
  git(f.repo, "-c", "tag.gpgsign=false", "tag", "-a", "v0.0.1", "-m", "tag");
  verifyPush(f.repo, `refs/tags/v0.0.1 ${git(f.repo, "rev-parse", "v0.0.1")} refs/tags/v0.0.1 ${"0".repeat(40)}`);
  git(f.repo, "commit", "--allow-empty", "-m", "new commit");
  const next = git(f.repo, "rev-parse", "HEAD");
  f.save({ ...f.report, sha: next });
  assert.throws(() => verifyPush(f.repo, `HEAD ${next} refs/heads/main ${f.sha}\n${f.input}`), /checked-out commit/);
  assert.throws(() => verifyPush(f.repo, "malformed"), /Invalid pre-push/);
});

test("deletions and up-to-date pushes need no receipt", (t) => {
  const f = fixture(t);
  verifyPush(f.repo, "");
  verifyPush(f.repo, `(delete) ${"0".repeat(40)} refs/heads/old ${f.sha}`);
});

test("installer preserves attribution and refuses unrelated custom hooks", (t) => {
  const f = fixture(t);
  writeFileSync(join(f.repo, ".git/hooks/prepare-commit-msg"), '#!/bin/sh\nprintf "\\nFixture attribution\\n" >> "$1"\n', { mode: 0o755 });
  execFileSync(process.execPath, ["scripts/install-git-hooks.mjs"], { cwd: f.repo, env });
  git(f.repo, "commit", "--allow-empty", "-m", "attributed");
  assert.match(git(f.repo, "log", "-1", "--format=%B"), /Fixture attribution/);
  git(f.repo, "config", "core.hooksPath", "custom-hooks");
  assert.notEqual(spawnSync(process.execPath, ["scripts/install-git-hooks.mjs"], { cwd: f.repo, env }).status, 0);
  assert.equal(git(f.repo, "config", "--get", "core.hooksPath"), "custom-hooks");
});

test("hook configuration and receipts are isolated between worktrees", (t) => {
  const f = fixture(t);
  git(f.repo, "config", "extensions.worktreeConfig", "true");
  const other = join(f.directory, "other");
  git(f.repo, "worktree", "add", "-b", "other", other);
  execFileSync(process.execPath, ["scripts/install-git-hooks.mjs"], { cwd: other, env });
  assert.equal(spawnSync("git", ["config", "--get", "core.hooksPath"], { cwd: f.repo, env }).status, 1);
  f.save();
  assert.throws(() => verifyPush(other, f.input), /No local CI result/);
});

test("test children receive no runtime, application or provider secrets", () => {
  const result = testEnvironment({ PATH: "/bin", HOME: "/example", MULTICA_TOKEN: "secret", DATABASE_URL: "production", OPENAI_API_KEY: "secret", GH_TOKEN: "secret", GIT_DIR: "/wrong", REDIS_URL: "production" });
  for (const key of ["MULTICA_TOKEN", "DATABASE_URL", "OPENAI_API_KEY", "GH_TOKEN", "GIT_DIR", "REDIS_URL"]) assert.equal(result[key], undefined);
  assert.equal(result.HOME, "/example");
});

test("failed retries revoke an earlier PASS before gates start", async (t) => {
  const f = fixture(t);
  f.save();
  // This fixture has no origin, and must never reach dependency installation.
  assert.equal(await run(f.repo), 1);
  assert.equal(JSON.parse(readFileSync(f.receipt)).state, "failure");
  assert.throws(() => verifyPush(f.repo, f.input), /No complete passing/);
});

test("public summaries pin SHA without local logs or environment", (t) => {
  const f = fixture(t);
  const text = markdown(f.report);
  assert.match(text, new RegExp(f.sha));
  for (const [name] of gates) assert.ok(text.includes(`PASS \`${name}\``));
  assert.ok(!text.includes(f.repo));
});

test("verification Actions only run manually", () => {
  for (const name of ["ci", "mobile-verify", "openclaw-config-smoke", "desktop-smoke", "ui-performance"]) {
    const triggers = readFileSync(join(root, `.github/workflows/${name}.yml`), "utf8").split(/^on:\s*$/m)[1].split(/^\S/m)[0];
    assert.match(triggers, /workflow_dispatch:/);
    assert.doesNotMatch(triggers, /^  (push|pull_request|schedule|workflow_run):/m);
  }
});
