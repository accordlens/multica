import { accessSync, constants, readdirSync } from "node:fs";
import { execFileSync } from "node:child_process";
import { join, resolve } from "node:path";

const git = (...args) => execFileSync("git", args, { encoding: "utf8" }).trim();
const config = (key) => {
  try { return git("config", "--get", key); } catch { return ""; }
};
process.chdir(git("rev-parse", "--show-toplevel"));
const existing = config("core.hooksPath");
if (existing && existing !== ".githooks") {
  throw new Error(`core.hooksPath=${existing}; integrate .githooks/pre-push before installing.`);
}
if (!existing) {
  const directory = join(git("rev-parse", "--git-common-dir"), "hooks");
  for (const name of readdirSync(directory)) {
    if (name.endsWith(".sample") || name === "prepare-commit-msg") continue;
    try { accessSync(join(directory, name), constants.X_OK); } catch { continue; }
    throw new Error(`Existing hook ${name}; integrate it before installing .githooks.`);
  }
}
const linked = resolve(git("rev-parse", "--git-dir")) !== resolve(git("rev-parse", "--git-common-dir"));
if (linked && config("extensions.worktreeConfig") !== "true") {
  throw new Error("Enable extensions.worktreeConfig before installing hooks in a linked worktree.");
}
git("config", config("extensions.worktreeConfig") === "true" ? "--worktree" : "--local", "core.hooksPath", ".githooks");
console.log("Installed local CI pre-push gate for this checkout.");
