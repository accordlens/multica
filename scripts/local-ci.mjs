import { execFileSync, spawn } from "node:child_process";
import { closeSync, existsSync, mkdirSync, mkdtempSync, openSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { createServer } from "node:net";
import { homedir, platform } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const shellGate = (name) => ["bash", "scripts/local-ci-checks.sh", name];
const turboGate = (task, filter) => ["pnpm", "exec", "turbo", task, `--filter=${filter}`, "--force", "--concurrency=2", "--env-mode=loose"];
export const gates = [
  ["scripts", shellGate("scripts")],
  ["frontend-typecheck", turboGate("typecheck", "!@multica/mobile")],
  ["frontend-lint", turboGate("lint", "!@multica/mobile")],
  ["frontend-test", turboGate("test", "!@multica/mobile")],
  ["frontend-build", turboGate("build", "!@multica/mobile")],
  ["mobile-typecheck", turboGate("typecheck", "@multica/mobile")],
  ["mobile-lint", turboGate("lint", "@multica/mobile")],
  ["mobile-test", turboGate("test", "@multica/mobile")],
  ["sqlc", shellGate("sqlc")],
  ["backend-build", shellGate("backend-build")],
  ["backend-test", shellGate("backend-test")],
  ["vulnerabilities", shellGate("vulnerabilities")],
  ["e2e", ["pnpm", "exec", "playwright", "test", "--reporter=line"]],
  ["selfhost", shellGate("selfhost")],
];
const marker = "<!-- multica-local-ci -->";
const children = new Set();
let interrupted = false;
function stop(child) {
  if (!child.pid) return;
  try { process.kill(-child.pid, "SIGKILL"); } catch { /* Already exited. */ }
}
const now = () => new Date().toISOString();
const command = (cwd, tool, args, options = {}) => (execFileSync(tool, args, {
  cwd, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"], ...options,
}) ?? "").trim();
const git = (cwd, ...args) => command(cwd, "git", args);
const head = (cwd) => git(cwd, "rev-parse", "HEAD");
export function requireClean(cwd, sha = head(cwd)) {
  const changes = git(cwd, "status", "--porcelain", "--untracked-files=all");
  if (head(cwd) !== sha || changes) {
    throw new Error(`Checkout changed. Commit or stash all changes, then run pnpm local:ci again.\n${changes}`);
  }
}
function reportPath(cwd) {
  return resolve(cwd, git(cwd, "rev-parse", "--git-path", "local-ci/latest.json"));
}
export function requirePass(report, sha) {
  if (report.schema !== 1 || report.sha !== sha || report.state !== "success" || !report.finishedAt ||
      report.gates?.length !== gates.length || gates.some(([name, argv], index) => {
        const result = report.gates[index];
        return result.name !== name || result.state !== "success" || result.exitCode !== 0 ||
          JSON.stringify(result.command) !== JSON.stringify(argv);
      })) {
    throw new Error("No complete passing local CI for this commit. Run pnpm local:ci before pushing.");
  }
}
export function verifyPush(cwd, input) {
  const sha = head(cwd);
  let checked = false;
  for (const line of input.trim().split("\n").filter(Boolean)) {
    const fields = line.trim().split(/\s+/);
    if (fields.length !== 4) throw new Error("Invalid pre-push ref input.");
    const [, localSha] = fields;
    if (/^0+$/.test(localSha)) continue;
    if (git(cwd, "rev-parse", `${localSha}^{commit}`) !== sha) {
      throw new Error("Push only the checked-out commit; check out and verify each branch/tag separately.");
    }
    checked = true;
  }
  if (!checked) return;
  if (existsSync(join(dirname(reportPath(cwd)), "running.lock"))) throw new Error("Local CI is still running; push refused.");
  requireClean(cwd, sha);
  let report;
  try { report = JSON.parse(readFileSync(reportPath(cwd), "utf8")); }
  catch { throw new Error("No local CI result. Run pnpm local:ci before pushing."); }
  requirePass(report, sha);
  console.log(`Local CI PASS for ${sha.slice(0, 12)} (reusing ${report.finishedAt}).`);
}

// Test children receive no runtime credentials, provider keys, or developer .env.
export function testEnvironment(source = process.env) {
  const allowed = /^(PATH|HOME|USER|LOGNAME|SHELL|TMPDIR|TMP|TEMP|LANG|LC_.*|SystemRoot|COMSPEC|PATHEXT|USERPROFILE|APPDATA|LOCALAPPDATA|DOCKER_HOST|DOCKER_CONTEXT|DOCKER_CONFIG|SSL_CERT_FILE|SSL_CERT_DIR|NODE_EXTRA_CA_CERTS)$/;
  const env = Object.fromEntries(Object.entries(source).filter(([key]) => allowed.test(key)));
  return { ...env, CI: "true", GOMAXPROCS: "4", GOFLAGS: "-p=4", GIT_CONFIG_NOSYSTEM: "1", GIT_CONFIG_GLOBAL: "/dev/null" };
}
async function execute(cwd, argv, env, logfile) {
  const fd = openSync(logfile, "a");
  try {
    return await new Promise((done) => {
      const child = spawn(argv[0], argv.slice(1), { cwd, env, detached: true, stdio: ["ignore", fd, fd] });
      children.add(child);
      const timeout = setTimeout(() => stop(child), 45 * 60 * 1000);
      child.once("close", () => { clearTimeout(timeout); children.delete(child); });
      child.on("error", (error) => { writeFileSync(logfile, `${error.message}\n`, { flag: "a" }); done(1); });
      child.on("exit", (code) => { stop(child); done(code ?? 1); });
    });
  } finally { closeSync(fd); }
}
const delay = (ms) => new Promise((done) => setTimeout(done, ms));
async function freePort() {
  const server = createServer();
  await new Promise((done, reject) => server.listen(0, "127.0.0.1", done).on("error", reject));
  const port = server.address().port;
  await new Promise((done) => server.close(done));
  return port;
}
async function waitFor(check, description) {
  for (let attempt = 0; attempt < 180; attempt++) {
    if (interrupted) throw new Error("Local CI interrupted.");
    try { if (await check()) return; } catch { /* Service is still starting. */ }
    await delay(1000);
  }
  throw new Error(`Timed out waiting for ${description}.`);
}
async function e2e(cwd, env, logfile) {
  const services = [];
  const fd = openSync(`${logfile}.services`, "a");
  try {
    for (const argv of [["server/bin/server"], ["pnpm", "--filter", "@multica/web", "exec", "next", "start", "--port", env.FRONTEND_PORT]]) {
      const child = spawn(argv[0], argv.slice(1), { cwd, env, detached: true, stdio: ["ignore", fd, fd] });
      child.on("error", () => {});
      services.push(child);
      children.add(child);
    }
    for (const url of [`http://127.0.0.1:${env.PORT}/readyz`, env.PLAYWRIGHT_BASE_URL]) {
      await waitFor(async () => {
        if (services.some((child) => child.exitCode !== null || !child.pid)) throw new Error("Service exited");
        return (await fetch(url, { signal: AbortSignal.timeout(1000) })).ok;
      }, url);
    }
    return await execute(cwd, gates.find(([name]) => name === "e2e")[1], env, logfile);
  } finally {
    // Only process groups created here are signalled; never reuse a local app.
    for (const child of services) {
      if (!child.pid) continue;
      try { process.kill(-child.pid, "SIGTERM"); } catch { /* Already exited. */ }
    }
    await delay(1000);
    for (const child of services) {
      stop(child);
      children.delete(child);
    }
    closeSync(fd);
  }
}

export async function run(cwd) {
  if (platform() === "win32") throw new Error("Run local CI in WSL2 with Docker integration.");
  const sha = head(cwd);
  requireClean(cwd, sha);
  const latest = reportPath(cwd);
  mkdirSync(dirname(latest), { recursive: true });
  const lock = join(dirname(latest), "running.lock");
  try { mkdirSync(lock); } catch { throw new Error("Local CI is already running; inspect local-ci/running.lock before retrying."); }
  writeFileSync(join(lock, "owner.json"), JSON.stringify({ pid: process.pid, sha, startedAt: now() }));
  const directory = join(dirname(latest), `${Date.now()}-${sha.slice(0, 12)}`);
  mkdirSync(directory, { recursive: true });
  const workspaceBase = join(homedir(), ".cache", "multica-local-ci");
  mkdirSync(workspaceBase, { recursive: true });
  const workspace = mkdtempSync(join(workspaceBase, "run-"));
  const source = join(workspace, "source");
  const report = { schema: 1, sha, startedAt: now(), state: "pending", gates: [] };
  const save = () => {
    writeFileSync(join(directory, "report.json"), `${JSON.stringify(report, null, 2)}\n`);
    writeFileSync(latest, `${JSON.stringify(report, null, 2)}\n`);
  };
  save(); // A failed or interrupted retry must invalidate a previous PASS.
  const env = testEnvironment();
  env.TMPDIR = join(workspace, "tmp");
  mkdirSync(env.TMPDIR);
  const containers = [];
  let network;
  const goContainerFile = join(directory, "go-container.cid");
  const docker = (...args) => command(cwd, "docker", args, { env });
  const setupLog = join(directory, "setup.log");
  interrupted = false;
  const interrupt = () => { interrupted = true; for (const child of children) stop(child); };
  process.on("SIGINT", interrupt);
  process.on("SIGTERM", interrupt);
  try {
    const origin = git(cwd, "remote", "get-url", "origin");
    for (const [tool, args] of [["node", ["--version"]], ["pnpm", ["--version"]], ["go", ["version"]], ["docker", ["version", "--format", "{{.Server.Version}}"]], ["pwsh", ["--version"]], ["helm", ["version", "--short"]]]) {
      report.toolchain ??= {};
      report.toolchain[tool] = command(tool === "go" ? join(cwd, "server") : cwd, tool, args, { env });
    }
    git(cwd, "clone", "--no-hardlinks", "--no-checkout", cwd, source);
    git(source, "checkout", "--detach", sha);
    git(source, "remote", "set-url", "origin", origin);
    network = docker("network", "create", `multica-local-ci-${process.pid}-${Date.now()}`);
    for (const [kind, image, port, extra] of [
      ["pg", "pgvector/pgvector:pg17", "5432", ["-e", "POSTGRES_USER=multica", "-e", "POSTGRES_PASSWORD=local-ci", "-e", "POSTGRES_DB=multica_ci"]],
      ["redis", "redis:7-alpine", "6379", []],
    ]) {
      const name = `multica-local-ci-${process.pid}-${Date.now()}-${kind}`;
      const id = docker("run", "--rm", "--detach", "--name", name, "--network", network, "--network-alias", kind, "-p", `127.0.0.1::${port}`, ...extra, image);
      containers.push(id);
      const hostPort = docker("port", name, `${port}/tcp`).split(":").at(-1);
      if (kind === "pg") {
        env.DATABASE_URL = `postgres://multica:local-ci@127.0.0.1:${hostPort}/multica_ci?sslmode=disable`;
        await waitFor(() => { docker("exec", name, "pg_isready", "-U", "multica", "-d", "multica_ci"); return true; }, "CI PostgreSQL");
      } else {
        env.REDIS_TEST_URL = `redis://127.0.0.1:${hostPort}/1`;
        await waitFor(() => docker("exec", name, "redis-cli", "ping") === "PONG", "CI Redis");
      }
    }
    env.PORT = String(await freePort());
    do { env.FRONTEND_PORT = String(await freePort()); } while (env.FRONTEND_PORT === env.PORT);
    env.FRONTEND_ORIGIN = `http://localhost:${env.FRONTEND_PORT}`;
    Object.assign(env, {
      REMOTE_API_URL: `http://localhost:${env.PORT}`, NEXT_PUBLIC_API_URL: "", NEXT_PUBLIC_WS_URL: `ws://localhost:${env.PORT}/ws`,
      MULTICA_PUBLIC_URL: `http://localhost:${env.PORT}`, MULTICA_APP_URL: env.FRONTEND_ORIGIN,
      PLAYWRIGHT_BASE_URL: env.FRONTEND_ORIGIN, LOCAL_UPLOAD_BASE_URL: `http://localhost:${env.PORT}`,
      JWT_SECRET: "local-ci-synthetic-jwt-secret-at-least-32-characters", APP_ENV: "development", DO_NOT_TRACK: "true",
      LOCAL_UPLOAD_DIR: join(source, "data", "uploads"),
    });
    if (await execute(source, ["pnpm", "install", "--frozen-lockfile"], env, setupLog)) throw new Error("Dependency install failed; see setup.log.");
    if (await execute(source, ["pnpm", "exec", "playwright", "install", "chromium"], env, setupLog)) throw new Error("Chromium install failed; see setup.log.");
    for (const [name, argv] of gates) {
      if (interrupted) throw new Error("Local CI interrupted.");
      const entry = { name, command: argv, startedAt: now(), state: "pending" };
      report.gates.push(entry);
      save();
      console.log(`RUN ${name}`);
      const logfile = join(directory, `${name}.log`);
      const gateEnv = ["scripts", "selfhost"].includes(name) ? { ...testEnvironment(), TMPDIR: env.TMPDIR } : env;
      try {
        if (name === "backend-test") {
          const image = "multica-local-ci-go:1.26.9";
          entry.exitCode = await execute(source, ["docker", "build", "--file", "Dockerfile.local-ci-go", "--tag", image, "."], gateEnv, logfile);
          if (entry.exitCode === 0) {
            report.toolchain.goTestImage = docker("image", "inspect", "--format", "{{.Id}}", image);
            entry.exitCode = await execute(source, ["docker", "run", "--init", "--cidfile", goContainerFile, "--network", network,
              "--volume", `${source}:/src`, "--volume", "multica-local-ci-gomod:/go/pkg/mod",
              "--volume", "multica-local-ci-gobuild:/tmp/go-build",
              "--env", "DATABASE_URL=postgres://multica:local-ci@pg:5432/multica_ci?sslmode=disable",
              "--env", "REDIS_TEST_URL=redis://redis:6379/1", "--env", "GOMAXPROCS=4", report.toolchain.goTestImage, ...argv], gateEnv, logfile);
          }
        } else entry.exitCode = name === "e2e" ? await e2e(source, gateEnv, logfile) : await execute(source, argv, gateEnv, logfile);
      }
      catch (error) { entry.exitCode = 1; writeFileSync(logfile, `${error.message}\n`, { flag: "a" }); }
      if (name === "frontend-build" && entry.exitCode === 0) {
        // Next rewrites generated route-type shims during production builds.
        // Normalize only those shims, only in this disposable checkout.
        git(source, "restore", "--worktree", "--", "apps/web/next-env.d.ts", "apps/docs/next-env.d.ts");
      }
      const changes = git(source, "status", "--porcelain", "--untracked-files=all");
      if (changes) writeFileSync(logfile, `\nCheckout changes after ${name}:\n${changes}\n`, { flag: "a" });
      entry.state = entry.exitCode === 0 ? "success" : "failure";
      entry.finishedAt = now();
      save();
      console.log(`${entry.state === "success" ? "PASS" : "FAIL"} ${name}`);
    }
    requireClean(source, sha);
    requireClean(cwd, sha);
    report.state = report.gates.every((gate) => gate.state === "success") ? "success" : "failure";
  } catch (error) {
    report.state = "failure";
    report.error = error.message;
    console.error(error.message);
  } finally {
    if (existsSync(goContainerFile)) containers.push(readFileSync(goContainerFile, "utf8").trim());
    for (const name of containers.reverse()) {
      try { docker("rm", "--force", name); } catch { report.state = "failure"; report.error = "CI container cleanup failed."; }
    }
    if (network) {
      try { docker("network", "rm", network); } catch { report.state = "failure"; report.error = "CI network cleanup failed."; }
    }
    rmSync(workspace, { recursive: true, force: true });
    report.finishedAt = now();
    save();
    rmSync(lock, { recursive: true, force: true });
    process.removeListener("SIGINT", interrupt);
    process.removeListener("SIGTERM", interrupt);
  }
  console.log(`Local CI ${report.state.toUpperCase()}; logs: ${directory}`);
  return report.state === "success" ? 0 : 1;
}

export function markdown(report) {
  return `${marker}\nLocal CI **${report.state.toUpperCase()}** for \`${report.sha}\`\n\n` +
    gates.map(([name]) => {
      const gate = report.gates.find((result) => result.name === name);
      return `- ${gate?.state === "success" ? "PASS" : gate ? "FAIL" : "NOT RUN"} \`${name}\``;
    }).join("\n") +
    `\n\nCompleted: ${report.finishedAt}. Executed locally on ${report.toolchain?.node ?? "unknown Node"}.` +
    "\n\nNative Windows/macOS matrix, real-provider smoke, desktop packaging and performance benchmarks remain separate checks. Full logs stay local; this comment contains no environment values.\n";
}
export function publish(cwd, prNumber) {
  const sha = head(cwd);
  requireClean(cwd, sha);
  const report = JSON.parse(readFileSync(reportPath(cwd), "utf8"));
  if (report.sha !== sha || !["success", "failure"].includes(report.state) || !report.finishedAt) throw new Error("No completed result for HEAD.");
  if (report.state === "success") requirePass(report, sha);
  const gh = (...args) => command(cwd, "gh", args);
  const repository = JSON.parse(gh("repo", "view", git(cwd, "remote", "get-url", "origin"), "--json", "nameWithOwner")).nameWithOwner;
  const pr = JSON.parse(gh("pr", "view", ...(prNumber ? [prNumber] : []), "--repo", repository, "--json", "number,headRefOid,url"));
  if (pr.headRefOid !== sha) throw new Error("Push this exact commit to the PR before publishing its CI result.");
  const login = JSON.parse(gh("api", "user")).login;
  const comments = JSON.parse(gh("api", "--paginate", "--slurp", `repos/${repository}/issues/${pr.number}/comments?per_page=100`)).flat();
  const previous = comments.findLast((comment) => comment.user.login === login && comment.body.includes(marker));
  const bodyfile = join(dirname(reportPath(cwd)), "comment.json");
  writeFileSync(bodyfile, JSON.stringify({ body: markdown(report) }));
  const posted = JSON.parse(gh("api", "--method", previous ? "PATCH" : "POST", previous ? `repos/${repository}/issues/comments/${previous.id}` : `repos/${repository}/issues/${pr.number}/comments`, "--input", bodyfile));
  for (const [context, state] of [...gates.map(([name]) => [`local-ci/${name}`, report.gates.find((gate) => gate.name === name)?.state]), ["local-ci", report.state]]) {
    const payload = join(dirname(reportPath(cwd)), "status.json");
    writeFileSync(payload, JSON.stringify({ context, state: state === "success" ? "success" : "failure", target_url: posted.html_url, description: "Local verification of the exact PR commit" }));
    gh("api", "--method", "POST", `repos/${repository}/statuses/${sha}`, "--input", payload);
  }
  console.log(`Published local CI ${report.state}: ${posted.html_url}`);
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    const cwd = git(process.cwd(), "rev-parse", "--show-toplevel");
    const [action = "run", argument] = process.argv.slice(2);
    if (action === "run") process.exitCode = await run(cwd);
    else if (action === "verify-push") verifyPush(cwd, readFileSync(0, "utf8"));
    else if (action === "publish") publish(cwd, argument);
    else if (action === "push") {
      verifyPush(cwd, `HEAD ${head(cwd)} refs/heads/current ${"0".repeat(40)}`);
      command(cwd, "git", ["push", "--set-upstream", "origin", ...(argument ? [`HEAD:refs/heads/${argument}`] : ["HEAD"])], { stdio: "inherit" });
      publish(cwd, argument);
    } else throw new Error("Usage: local-ci.mjs run|verify-push|publish [PR]|push [branch]");
  } catch (error) { console.error(error.message); process.exitCode = 1; }
}
