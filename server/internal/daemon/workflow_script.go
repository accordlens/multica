package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/processtree"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
)

// boundedScriptLog drains all output but retains a bounded, UTF-8-safe prefix.
type boundedScriptLog struct {
	mu   sync.Mutex
	data []byte
}

func (b *boundedScriptLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if remaining := 60000 - len(b.data); remaining > 0 {
		b.data = append(b.data, p[:min(remaining, len(p))]...)
	}
	return len(p), nil
}
func (b *boundedScriptLog) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.ReplaceAll(strings.ToValidUTF8(string(b.data), "?"), "\x00", "")
}

func workflowEnvironment(report, runID, sha, version, scriptVersion string) []string {
	allowed := map[string]bool{"PATH": true, "SYSTEMROOT": true, "COMSPEC": true, "PATHEXT": true, "TMPDIR": true, "TMP": true, "TEMP": true, "LANG": true, "TZ": true}
	var env []string
	for _, entry := range os.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		if runtime.GOOS == "windows" {
			key = strings.ToUpper(key)
		}
		if allowed[key] {
			env = append(env, key+"="+value)
		}
	}
	return append(env, "CI=true", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"MULTICA_WORKFLOW_REPORT="+report, "MULTICA_WORKFLOW_RUN_ID="+runID,
		"MULTICA_WORKFLOW_SOURCE_SHA="+sha, "MULTICA_WORKFLOW_VERSION="+version,
		"MULTICA_WORKFLOW_SCRIPT_VERSION="+scriptVersion)
}

// workflowPath rejects Windows drives, traversal and symlinks escaping the
// execution checkout on every host (including when validating Windows input).
func workflowPath(root, relative string) (string, error) {
	if !issueworkflow.RelativePath(relative) {
		return "", fmt.Errorf("invalid relative workflow path")
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	target, err := filepath.EvalSymlinks(filepath.Join(canonicalRoot, filepath.FromSlash(relative)))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(canonicalRoot, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("workflow path escapes checkout")
	}
	return target, nil
}

func workflowSource(ctx context.Context, workdir string, script issueworkflow.Script) (issueworkflow.Source, string, error) {
	checkout, err := workflowPath(workdir, script.Checkout)
	if err != nil {
		return issueworkflow.Source{}, "", err
	}
	git := func(args ...string) ([]byte, error) {
		cmd := exec.Command("git", append([]string{"-c", "core.fsmonitor=false", "-c", "core.hooksPath=" + os.DevNull, "-C", checkout}, args...)...)
		cmd.Env = workflowEnvironment("", "", "", "", "")
		return processtree.Output(ctx, cmd, time.Second)
	}
	root, err := git("rev-parse", "--show-toplevel")
	if err != nil || !sameExistingDir(strings.TrimSpace(string(root)), checkout) {
		return issueworkflow.Source{}, "", fmt.Errorf("workflow checkout must be a Git repository root")
	}
	sha, err := git("rev-parse", "HEAD")
	if err != nil {
		return issueworkflow.Source{}, "", err
	}
	status, err := git("status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return issueworkflow.Source{}, "", err
	}
	source := issueworkflow.Source{SHA: strings.TrimSpace(string(sha))}
	flags, err := git("ls-files", "-v", "-z")
	if err != nil {
		return source, "", err
	}
	for _, entry := range strings.Split(string(flags), "\x00") {
		if len(entry) > 0 && entry[0] != 'H' {
			status = append(status, []byte("unverifiable-index-flags:"+entry)...)
		}
	}
	if len(status) != 0 {
		// The report carries an identity for dirty sources, but dirty sources
		// cannot satisfy this first pilot's acceptance gate.
		diff, diffErr := git("diff", "--no-ext-diff", "--no-textconv", "HEAD", "--binary")
		if diffErr != nil {
			return source, "", diffErr
		}
		source.DirtyHash = issueworkflow.HashBytes(append(status, diff...))
	}
	return source, checkout, nil
}

func pinWorkflowFiles(checkout string, script issueworkflow.Script) error {
	for relative, digest := range script.Files {
		filename, err := workflowPath(checkout, relative)
		if err != nil {
			return err
		}
		file, err := os.Open(filename)
		if err != nil {
			return err
		}
		data, readErr := io.ReadAll(io.LimitReader(file, 4*1024*1024+1))
		_ = file.Close()
		if readErr != nil || len(data) > 4*1024*1024 || issueworkflow.HashBytes(data) != digest {
			return fmt.Errorf("pinned gate file changed: %s", relative)
		}
	}
	return nil
}

func runWorkflowScript(ctx context.Context, runID string, execution issueworkflow.Execution) (evidence issueworkflow.Evidence, returnErr error) {
	evidence = issueworkflow.Evidence{RunID: runID, Source: execution.Source, WorkflowVersion: execution.WorkflowVersion,
		ScriptVersion: execution.Script.Version, ScriptHash: issueworkflow.Hash(execution.Script), StartedAt: time.Now().UTC(), ExitCode: -1, Outcome: "infrastructure_failed"}
	log := &boundedScriptLog{}
	var recordDirectory string
	replayed := false
	defer func() {
		if replayed {
			return
		}
		evidence.FinishedAt = time.Now().UTC()
		evidence.Log = log.String()
		evidence.LogHash = issueworkflow.HashBytes([]byte(evidence.Log))
		if returnErr != nil {
			evidence.Reason = returnErr.Error()
		}
		if recordDirectory != "" {
			raw, err := json.Marshal(evidence)
			if err == nil {
				err = os.WriteFile(filepath.Join(recordDirectory, "evidence.pending"), raw, 0600)
			}
			if err == nil {
				err = os.Rename(filepath.Join(recordDirectory, "evidence.pending"), filepath.Join(recordDirectory, "evidence.json"))
			}
			if err != nil {
				evidence.Outcome = "infrastructure_failed"
				evidence.Reason = "could not retain script receipt"
				returnErr = err
			}
		}
	}()
	if err := execution.Script.Validate(); err != nil {
		return evidence, err
	}
	deadline := minTime(execution.Deadline, time.Now().Add(time.Duration(execution.Script.TimeoutSeconds)*time.Second))
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	source, checkout, err := workflowSource(ctx, execution.WorkDir, execution.Script)
	if err != nil {
		return evidence, err
	}
	evidence.Source = source
	if source != execution.Source || !source.Clean() {
		evidence.Outcome = "stale_source"
		return evidence, fmt.Errorf("checkout changed since agent completion")
	}
	if err := pinWorkflowFiles(checkout, execution.Script); err != nil {
		return evidence, err
	}
	gitDirCmd := exec.Command("git", "-C", checkout, "rev-parse", "--absolute-git-dir")
	gitDirCmd.Env = workflowEnvironment("", "", "", "", "")
	gitDir, err := processtree.Output(ctx, gitDirCmd, time.Second)
	if err != nil {
		return evidence, err
	}
	if !validWorkflowRunID(runID) {
		return evidence, fmt.Errorf("invalid run id")
	}
	owned := filepath.Join(strings.TrimSpace(string(gitDir)), "multica-workflow", runID)
	if err := os.MkdirAll(filepath.Dir(owned), 0700); err != nil {
		return evidence, err
	}
	if err := os.Mkdir(owned, 0700); err != nil {
		if os.IsExist(err) {
			// A complete retained result may be delivered again, never executed
			// again. A running/uncertain attempt is reconciled by task expiry.
			if raw, readErr := os.ReadFile(filepath.Join(owned, "evidence.json")); readErr == nil && json.Unmarshal(raw, &evidence) == nil {
				replayed = true
				return evidence, nil
			}
			return evidence, errStartClaimRejected
		}
		return evidence, err
	}
	recordDirectory = owned
	// Tests run against committed objects, never a concurrently writable
	// author's tree or ignored developer files. The replica is run-owned.
	snapshot := filepath.Join(owned, "checkout")
	defer func() {
		if err := os.RemoveAll(snapshot); err != nil {
			evidence.Outcome = "infrastructure_failed"
			returnErr = fmt.Errorf("checkout cleanup failed: %w", err)
		}
	}()
	for _, argv := range [][]string{{"clone", "--no-hardlinks", "--no-checkout", checkout, snapshot}, {"-C", snapshot, "-c", "core.hooksPath=" + os.DevNull, "checkout", "--detach", source.SHA}} {
		clone := exec.Command("git", argv...)
		clone.Env = workflowEnvironment("", "", "", "", "")
		if _, err := processtree.CombinedOutput(ctx, clone, time.Second); err != nil {
			return evidence, fmt.Errorf("prepare committed checkout: %w", err)
		}
	}
	if err := pinWorkflowFiles(snapshot, execution.Script); err != nil {
		return evidence, err
	}
	directory, err := workflowPath(snapshot, execution.Script.Directory)
	if err != nil {
		return evidence, err
	}
	home := filepath.Join(owned, "home")
	if err := os.MkdirAll(filepath.Join(home, "tmp"), 0700); err != nil {
		return evidence, err
	}
	defer func() {
		if err := os.RemoveAll(home); err != nil {
			evidence.Outcome = "infrastructure_failed"
			returnErr = fmt.Errorf("script resource cleanup failed: %w", err)
		}
	}()
	reportPath := filepath.Join(owned, "report.json")
	cmd := exec.Command(execution.Script.Command[0], execution.Script.Command[1:]...)
	cmd.Dir = directory
	cmd.Env = workflowEnvironment(reportPath, runID, source.SHA, execution.WorkflowVersion, execution.Script.Version)
	cmd.Env = append(cmd.Env, "HOME="+home, "USERPROFILE="+home, "XDG_CACHE_HOME="+filepath.Join(home, "cache"), "GOCACHE="+filepath.Join(home, "cache", "go-build"), "GOMODCACHE="+filepath.Join(home, "go-mod"), "TMPDIR="+filepath.Join(home, "tmp"), "TMP="+filepath.Join(home, "tmp"), "TEMP="+filepath.Join(home, "tmp"))
	for _, required := range execution.Script.RequiredEnv {
		found := false
		for _, entry := range cmd.Env {
			if strings.HasPrefix(entry, required+"=") && len(entry) > len(required)+1 {
				found = true
			}
		}
		if !found {
			return evidence, fmt.Errorf("required environment variable %s is unavailable", required)
		}
	}
	cmd.Stdout = log
	cmd.Stderr = log
	err = processtree.Run(ctx, cmd, time.Second)
	if cmd.ProcessState != nil {
		evidence.ExitCode = cmd.ProcessState.ExitCode()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		evidence.Outcome = "timeout"
		return evidence, err
	}
	if errors.Is(err, context.Canceled) {
		evidence.Outcome = "cancelled"
		return evidence, err
	}
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return evidence, err
		}
	}
	finalSource, _, sourceErr := workflowSource(ctx, execution.WorkDir, execution.Script)
	if sourceErr != nil || finalSource != source {
		evidence.Source = finalSource
		evidence.Outcome = "stale_source"
		return evidence, fmt.Errorf("source changed during script execution")
	}
	snapshotScript := execution.Script
	snapshotScript.Checkout = "."
	checkedSource, _, checkedErr := workflowSource(ctx, snapshot, snapshotScript)
	if checkedErr != nil || checkedSource != source {
		evidence.Outcome = "stale_source"
		return evidence, fmt.Errorf("script modified the committed checkout")
	}
	if pinErr := pinWorkflowFiles(snapshot, execution.Script); pinErr != nil {
		return evidence, pinErr
	}
	info, err := os.Lstat(reportPath)
	if err != nil || !info.Mode().IsRegular() {
		evidence.Outcome = "invalid_report"
		return evidence, fmt.Errorf("report must be a new regular file")
	}
	file, err := os.Open(reportPath)
	if err != nil {
		evidence.Outcome = "invalid_report"
		return evidence, err
	}
	raw, err := io.ReadAll(io.LimitReader(file, 65537))
	_ = file.Close()
	if err != nil {
		return evidence, err
	}
	evidence.Report = string(raw)
	evidence.ReportID = runID
	evidence.ReportHash = issueworkflow.HashBytes(raw)
	evidence.Outcome, err = issueworkflow.ValidateReport(raw, runID, source, execution.WorkflowVersion, execution.Script)
	if err != nil {
		return evidence, err
	}
	if (evidence.Outcome == "passed" && evidence.ExitCode != 0) || (evidence.Outcome == "assertion_failed" && evidence.ExitCode == 0) {
		evidence.Outcome = "infrastructure_failed"
		return evidence, fmt.Errorf("exit code disagrees with required checks")
	}
	return evidence, nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
func validWorkflowRunID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if r != '-' {
				return false
			}
		} else if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func (d *Daemon) runScriptTask(ctx context.Context, task Task, taskLog *slog.Logger) (TaskResult, error) {
	if _, err := d.client.StartTask(ctx, task); err != nil {
		return TaskResult{}, err
	}
	_ = d.client.ReportProgress(ctx, task.ID, "Running workflow script", 1, 1)
	evidence, err := superviseWorkflowScript(ctx, task.ID, *task.WorkflowExecution)
	if errors.Is(err, errStartClaimRejected) {
		return TaskResult{}, err
	}
	if err != nil {
		execution := task.WorkflowExecution
		evidence = issueworkflow.Evidence{RunID: task.ID, Source: execution.Source, WorkflowVersion: execution.WorkflowVersion, ScriptVersion: execution.Script.Version, ScriptHash: issueworkflow.Hash(execution.Script), Outcome: "infrastructure_failed", ExitCode: -1, StartedAt: time.Now().UTC(), FinishedAt: time.Now().UTC(), LogHash: issueworkflow.HashBytes(nil), Reason: err.Error()}
	}
	// Failed assertions are a completed script operation, distinct from
	// failed infrastructure. The server evaluates and routes its receipt.
	return TaskResult{Status: "completed", Comment: fmt.Sprintf("Workflow script %s: %s", task.WorkflowExecution.Script.ID, evidence.Outcome), WorkDir: task.WorkflowExecution.WorkDir, WorkflowEvidence: &evidence}, nil
}
