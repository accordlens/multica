package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/issueworkflow"
)

func workflowTestGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := workflowGit(append([]string{"-C", root}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s: %v", args, out, err)
	}
	return strings.TrimSpace(string(out))
}

const workflowTestGate = `import {readFileSync,writeFileSync} from 'node:fs';
const p=process.env; const mode=readFileSync('mode.txt','utf8').trim();
if(mode==='hang'){writeFileSync(p.MULTICA_WORKFLOW_REPORT+'.started','started');setInterval(()=>{},1000);}else{
 const checks=mode==='empty'?[]:[{name:'unit',passed:mode==='fail'?0:1,failed:mode==='fail'?1:0,skipped:mode==='skip'?1:0}];
 writeFileSync(p.MULTICA_WORKFLOW_REPORT,JSON.stringify({run_id:mode==='stale'?'old':p.MULTICA_WORKFLOW_RUN_ID,source_sha:p.MULTICA_WORKFLOW_SOURCE_SHA,workflow_version:p.MULTICA_WORKFLOW_VERSION,script_version:p.MULTICA_WORKFLOW_SCRIPT_VERSION,checks}));
 console.log('gate executed');process.exitCode=mode==='fail'?1:0;
}`

func workflowTestExecution(t *testing.T, mode string) issueworkflow.Execution {
	t.Helper()
	workdir := t.TempDir()
	t.Cleanup(func() {
		if err := removeWorkflowResources(workdir); err != nil {
			t.Error(err)
		}
	})
	repo := filepath.Join(workdir, "repo with spaces")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	for filename, body := range map[string]string{"gate.mjs": workflowTestGate, "mode.txt": mode} {
		if err := os.WriteFile(filepath.Join(repo, filename), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	workflowTestGit(t, repo, "init", "-q")
	workflowTestGit(t, repo, "add", ".")
	workflowTestGit(t, repo, "-c", "user.name=Workflow Test", "-c", "user.email=workflow@multica.test", "commit", "-qm", "fixture")
	return issueworkflow.Execution{Kind: "script", WorkflowVersion: strings.Repeat("a", 64), WorkDir: workdir, Source: issueworkflow.Source{SHA: workflowTestGit(t, repo, "rev-parse", "HEAD")}, Deadline: time.Now().Add(time.Minute), Script: issueworkflow.Script{ID: "test", Version: "1", Checkout: "repo with spaces", Directory: ".", Command: []string{"node", "gate.mjs"}, TimeoutSeconds: 5, BudgetSeconds: 60, MaxRepairs: 1, RequiredChecks: []string{"unit"}, Files: map[string]string{"gate.mjs": issueworkflow.HashBytes([]byte(workflowTestGate))}}}
}

func TestWorkflowScriptExecutesReportsAndRetainsReceiptWithoutReplay(t *testing.T) {
	for mode, want := range map[string]string{"pass": "passed", "fail": "assertion_failed", "skip": "skipped", "empty": "no_tests", "stale": "stale_report"} {
		t.Run(mode, func(t *testing.T) {
			e := workflowTestExecution(t, mode)
			id := uuid.NewString()
			got, err := runWorkflowScript(context.Background(), id, e)
			if got.Outcome != want {
				t.Fatalf("outcome %s want %s: %v", got.Outcome, want, err)
			}
			if mode == "pass" || mode == "fail" {
				if err != nil {
					t.Fatal(err)
				}
				if err := got.Validate(id, e, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			if got.ReportHash == "" || got.LogHash == "" || got.FinishedAt.IsZero() {
				t.Fatalf("missing receipt: %+v", got)
			}
			cached, err := runWorkflowScript(context.Background(), id, e)
			if err != nil || issueworkflow.Hash(cached) != issueworkflow.Hash(got) {
				t.Fatalf("callback replay reran or changed receipt: %v / %s", err, cached.Outcome)
			}
		})
	}
}

func TestWorkflowScriptLongCheckoutAndCleanup(t *testing.T) {
	e := workflowTestExecution(t, "pass")
	// Keep the source within Git for Windows' startup path limit. Its
	// receipt directory exceeds MAX_PATH; the disposable replica must not.
	longRoot := filepath.Join(e.WorkDir, strings.Repeat("p", max(1, 230-len(e.WorkDir)-len(e.Script.Checkout)-2)))
	if err := os.MkdirAll(longRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(e.WorkDir, e.Script.Checkout), filepath.Join(longRoot, e.Script.Checkout)); err != nil {
		t.Fatal(err)
	}
	e.WorkDir = longRoot
	e.Script.TimeoutSeconds = 15
	id := uuid.NewString()
	evidence, err := runWorkflowScript(context.Background(), id, e)
	if err != nil || evidence.Outcome != "passed" {
		t.Fatalf("long checkout: %+v / %v", evidence, err)
	}
	directory := filepath.Join(longRoot, e.Script.Checkout, ".git", "multica-workflow", id)
	raw, err := os.ReadFile(filepath.Join(directory, "resources.json"))
	if err != nil {
		t.Fatal(err)
	}
	var handle struct{ Directory string }
	if err := json.Unmarshal(raw, &handle); err != nil || handle.Directory == "" {
		t.Fatalf("missing resource cleanup handle: %s / %v", raw, err)
	}
	if _, err := os.Stat(handle.Directory); !os.IsNotExist(err) {
		t.Fatalf("owned resources remained after completion: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "evidence.json")); err != nil {
		t.Fatalf("cleanup removed the retained receipt: %v", err)
	}
}

func TestWorkflowScriptRejectsDirtyRevisionChangedGateAndSymlinkEscape(t *testing.T) {
	for _, kind := range []string{"dirty", "changed source", "gate", "escape"} {
		t.Run(kind, func(t *testing.T) {
			e := workflowTestExecution(t, "pass")
			repo := filepath.Join(e.WorkDir, e.Script.Checkout)
			switch kind {
			case "dirty":
				_ = os.WriteFile(filepath.Join(repo, "mode.txt"), []byte("fail"), 0600)
			case "changed source":
				_ = os.WriteFile(filepath.Join(repo, "mode.txt"), []byte("fail"), 0600)
				workflowTestGit(t, repo, "add", ".")
				workflowTestGit(t, repo, "-c", "user.name=T", "-c", "user.email=t@multica.test", "commit", "-qm", "changed")
			case "gate":
				e.Script.Files["gate.mjs"] = strings.Repeat("0", 64)
			case "escape":
				e.Script.Checkout = "../escape"
			}
			got, err := runWorkflowScript(context.Background(), uuid.NewString(), e)
			if err == nil || got.Outcome == "passed" {
				t.Fatalf("accepted %s: %+v", kind, got)
			}
		})
	}
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err == nil {
		if _, err := workflowPath(root, "escape"); err == nil {
			t.Fatal("symlink escaped checkout")
		}
	}
}

func TestWorkflowScriptTimeoutAndCancellation(t *testing.T) {
	for _, kind := range []string{"timeout", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			e := workflowTestExecution(t, "hang")
			e.Script.TimeoutSeconds = 1
			ctx := context.Background()
			cancel := func() {}
			if kind == "cancelled" {
				ctx, cancel = context.WithCancel(ctx)
				time.AfterFunc(150*time.Millisecond, cancel)
			}
			defer cancel()
			got, err := runWorkflowScript(ctx, uuid.NewString(), e)
			if err == nil || got.Outcome != kind {
				t.Fatalf("%s: %+v / %v", kind, got, err)
			}
		})
	}
}

func TestWorkflowScriptHelperProcess(t *testing.T) {
	if len(os.Args) < 2 || os.Args[len(os.Args)-1] != WorkflowScriptHelperArg {
		return
	}
	if err := RunWorkflowScriptHelper(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestWorkflowOwnerProcess(t *testing.T) {
	if len(os.Args) < 2 || os.Args[len(os.Args)-1] != "workflow-owner" {
		return
	}
	var request workflowScriptRequest
	if json.NewDecoder(os.Stdin).Decode(&request) != nil {
		os.Exit(2)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestWorkflowScriptHelperProcess$", "--", WorkflowScriptHelperArg)
	cmd.Env = workflowEnvironment("", "", "", "", "")
	_, err := superviseWorkflowCommand(context.Background(), cmd, request.RunID, request.Execution)
	if err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestWorkflowDaemonCrashClosesLeaseAndReapsScript(t *testing.T) {
	e := workflowTestExecution(t, "hang")
	id := uuid.NewString()
	raw, _ := json.Marshal(workflowScriptRequest{RunID: id, Execution: e})
	owner := exec.Command(os.Args[0], "-test.run=^TestWorkflowOwnerProcess$", "--", "workflow-owner")
	owner.Env = workflowEnvironment("", "", "", "", "")
	owner.Stdin = strings.NewReader(string(raw))
	configureWorkflowSupervisor(owner)
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Process.Kill(); _ = owner.Wait() }()
	directory := filepath.Join(e.WorkDir, e.Script.Checkout, ".git", "multica-workflow", id)
	started := filepath.Join(directory, "report.json.started")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("script never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := owner.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(7 * time.Second)
	for {
		if raw, err := os.ReadFile(filepath.Join(directory, "evidence.json")); err == nil {
			var evidence issueworkflow.Evidence
			if json.Unmarshal(raw, &evidence) != nil || evidence.Outcome != "cancelled" {
				t.Fatalf("crash cleanup: %s", raw)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon death left script without confirmed cleanup")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWorkflowSupervisorLeaseEOFAndLostResponse(t *testing.T) {
	e := workflowTestExecution(t, "pass")
	id := uuid.NewString()
	cmd := exec.Command(os.Args[0], "-test.run=^TestWorkflowScriptHelperProcess$", "--", WorkflowScriptHelperArg)
	cmd.Env = workflowEnvironment("", "", "", "", "")
	got, err := superviseWorkflowCommand(context.Background(), cmd, id, e)
	if err != nil || got.Outcome != "passed" {
		t.Fatalf("supervised: %+v / %v", got, err)
	}
	cmd = exec.Command(os.Args[0], "-test.run=^TestWorkflowScriptHelperProcess$", "--", WorkflowScriptHelperArg)
	cmd.Env = workflowEnvironment("", "", "", "", "")
	replay, err := superviseWorkflowCommand(context.Background(), cmd, id, e)
	if err != nil || issueworkflow.Hash(replay) != issueworkflow.Hash(got) {
		t.Fatalf("lost response replay: %+v / %v", replay, err)
	}
	e = workflowTestExecution(t, "hang")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	cmd = exec.Command(os.Args[0], "-test.run=^TestWorkflowScriptHelperProcess$", "--", WorkflowScriptHelperArg)
	cmd.Env = workflowEnvironment("", "", "", "", "")
	cancelled, err := superviseWorkflowCommand(ctx, cmd, uuid.NewString(), e)
	if err != nil || cancelled.Outcome != "cancelled" {
		t.Fatalf("lease EOF did not cancel script: %+v / %v", cancelled, err)
	}
}

func TestWorkflowEnvironmentDoesNotInheritCredentials(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "do-not-forward")
	t.Setenv("MULTICA_API_KEY", "do-not-forward")
	t.Setenv("GITHUB_TOKEN", "do-not-forward")
	for _, entry := range workflowEnvironment("report", "run", "sha", "version", "script") {
		if strings.Contains(entry, "do-not-forward") || strings.HasPrefix(entry, "HOME=") {
			t.Fatalf("credential source inherited: %s", entry)
		}
	}
}
