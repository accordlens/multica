package issueworkflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
)

// Script is an administrator-owned gate, saved with the workflow definition.
// Commands are argv, never shell text or instructions from a conversation.
type Script struct {
	ID             string   `json:"id"`
	Version        string   `json:"version"`
	Checkout       string   `json:"checkout"`
	Directory      string   `json:"directory"`
	Command        []string `json:"command"`
	RequiredEnv    []string `json:"required_env"`
	TimeoutSeconds int      `json:"timeout_seconds"`
	BudgetSeconds  int      `json:"budget_seconds"`
	MaxRepairs     int      `json:"max_repairs"`
	RequiredChecks []string `json:"required_checks"`
	// Files pins the gate implementation independently of the code being fixed.
	Files map[string]string `json:"files"`
}

var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var shaPattern = regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`)
var envPattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

func RelativePath(s string) bool {
	return s != "" && !strings.ContainsAny(s, "\\:\x00") && !path.IsAbs(s) && path.Clean(s) == s && s != ".." && !strings.HasPrefix(s, "../")
}

func (s Script) Validate() error {
	if s.ID == "" || len(s.ID) > 100 || s.Version == "" || len(s.Version) > 100 {
		return invalid("script id and version must be 1-100 bytes")
	}
	if !RelativePath(s.Checkout) || !RelativePath(s.Directory) {
		return invalid("script checkout and directory must be relative paths inside the run workdir")
	}
	if len(s.Command) == 0 || len(s.Command) > 64 || len(s.Files) == 0 || len(s.Files) > 50 {
		return invalid("script requires an argv command and pinned gate files")
	}
	for _, arg := range s.Command {
		if strings.ContainsRune(arg, 0) || len(arg) > 4096 {
			return invalid("invalid script argument")
		}
	}
	if s.Command[0] == "" || strings.ContainsAny(s.Command[0], "\\/:") {
		return invalid("script executable must be a tool name resolved on the runtime PATH")
	}
	if s.TimeoutSeconds < 1 || s.TimeoutSeconds > 3600 || s.BudgetSeconds < s.TimeoutSeconds || s.BudgetSeconds > 86400 || s.MaxRepairs < 0 || s.MaxRepairs > 5 {
		return invalid("script timeout, total budget or repair limit out of range")
	}
	if len(s.RequiredChecks) == 0 || len(s.RequiredChecks) > 100 {
		return invalid("script must name 1-100 required checks")
	}
	seen := map[string]bool{}
	for _, name := range s.RequiredChecks {
		if strings.TrimSpace(name) == "" || len(name) > 200 || seen[name] {
			return invalid("required checks must have unique nonempty names")
		}
		seen[name] = true
	}
	for filename, digest := range s.Files {
		if !RelativePath(filename) || !digestPattern.MatchString(digest) {
			return invalid("gate files require relative paths and SHA256 digests")
		}
	}
	for _, key := range s.RequiredEnv {
		// Credentials and agent runtime configuration never flow into gates.
		if !envPattern.MatchString(key) || key != "CI" && key != "LANG" && key != "TZ" {
			return invalid("script required_env supports CI, LANG and TZ only")
		}
	}
	return nil
}

func Hash(value any) string {
	raw, _ := json.Marshal(value)
	return HashBytes(raw)
}

func HashBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (d Definition) Version() string { raw, _ := EncodeSteps(d.Steps); return HashBytes(raw) }

func (d Definition) HasScripts() bool {
	for _, step := range d.Steps {
		if step.Script != nil {
			return true
		}
	}
	return false
}

// Execution lives in native task.context.hybrid_workflow. One completion CAS
// creates at most one successor in the same transaction. It is not a scheduler.
type Execution struct {
	Kind            string    `json:"kind"`
	RootTaskID      string    `json:"root_task_id"`
	SourceTaskID    string    `json:"source_task_id"`
	WorkflowID      string    `json:"workflow_id"`
	WorkflowVersion string    `json:"workflow_version"`
	StepKey         string    `json:"step_key"`
	AuthorID        string    `json:"author_id"`
	AuthorRuntimeID string    `json:"author_runtime_id"`
	WorkDir         string    `json:"work_dir"`
	Source          Source    `json:"source"`
	Script          Script    `json:"script"`
	Repairs         int       `json:"repairs"`
	Deadline        time.Time `json:"deadline"`
	Evidence        *Evidence `json:"evidence,omitempty"`
	BlockReason     string    `json:"block_reason,omitempty"`
	ReviewVerdict   string    `json:"review_verdict,omitempty"`
}

func ParseExecution(raw []byte) *Execution {
	var envelope struct {
		Execution *Execution `json:"hybrid_workflow"`
	}
	_ = json.Unmarshal(raw, &envelope)
	return envelope.Execution
}

type Source struct {
	SHA       string `json:"sha"`
	DirtyHash string `json:"dirty_hash,omitempty"`
}

func (s Source) Clean() bool { return shaPattern.MatchString(s.SHA) && s.DirtyHash == "" }

type Check struct {
	Name    string `json:"name"`
	Passed  int    `json:"passed"`
	Failed  int    `json:"failed"`
	Skipped int    `json:"skipped"`
}

// Report is emitted only to MULTICA_WORKFLOW_REPORT by the registered command.
// The run identity and revisions prevent a retained/empty report satisfying a gate.
type Report struct {
	InfrastructureError string  `json:"infrastructure_error,omitempty"`
	RunID               string  `json:"run_id"`
	SourceSHA           string  `json:"source_sha"`
	WorkflowVersion     string  `json:"workflow_version"`
	ScriptVersion       string  `json:"script_version"`
	Checks              []Check `json:"checks"`
}

type Evidence struct {
	RunID           string    `json:"run_id"`
	Source          Source    `json:"source"`
	WorkflowVersion string    `json:"workflow_version"`
	ScriptVersion   string    `json:"script_version,omitempty"`
	ScriptHash      string    `json:"script_hash,omitempty"`
	Outcome         string    `json:"outcome"`
	ExitCode        int       `json:"exit_code"`
	StartedAt       time.Time `json:"started_at"`
	FinishedAt      time.Time `json:"finished_at"`
	ReportID        string    `json:"report_id,omitempty"`
	ReportHash      string    `json:"report_hash,omitempty"`
	Report          string    `json:"report,omitempty"`
	Log             string    `json:"log,omitempty"`
	LogHash         string    `json:"log_hash,omitempty"`
	Reason          string    `json:"reason,omitempty"`
}

// ValidateReport returns assertion_failed only for a complete, fresh report.
// Missing, skipped or zero executed checks are never considered a test failure
// suitable for automatic code repair, and can never be a PASS.
func ValidateReport(raw []byte, runID string, source Source, version string, script Script) (string, error) {
	var report Report
	if len(raw) == 0 || len(raw) > 65536 || json.Unmarshal(raw, &report) != nil {
		return "invalid_report", fmt.Errorf("missing, oversized or malformed report")
	}
	if !source.Clean() || report.RunID != runID || report.SourceSHA != source.SHA || report.WorkflowVersion != version || report.ScriptVersion != script.Version {
		return "stale_report", fmt.Errorf("report does not match this run, revision and definition")
	}
	if report.InfrastructureError != "" {
		return "infrastructure_failed", fmt.Errorf("check infrastructure: %s", report.InfrastructureError)
	}
	checks := map[string]Check{}
	for _, check := range report.Checks {
		if _, duplicate := checks[check.Name]; duplicate || check.Passed < 0 || check.Failed < 0 || check.Skipped < 0 {
			return "invalid_report", fmt.Errorf("duplicate check or invalid counts")
		}
		checks[check.Name] = check
	}
	outcome := "passed"
	for _, required := range script.RequiredChecks {
		check, ok := checks[required]
		if !ok || check.Passed+check.Failed == 0 {
			return "no_tests", fmt.Errorf("required check %q did not execute", required)
		}
		if check.Skipped != 0 {
			return "skipped", fmt.Errorf("required check %q skipped tests", required)
		}
		if check.Failed != 0 {
			outcome = "assertion_failed"
		}
	}
	return outcome, nil
}

func (e Evidence) Validate(runID string, execution Execution, now time.Time) error {
	if e.RunID != runID || e.Source != execution.Source || !e.Source.Clean() || e.WorkflowVersion != execution.WorkflowVersion || e.ScriptVersion != execution.Script.Version || e.ScriptHash != Hash(execution.Script) {
		return fmt.Errorf("evidence identity or revision mismatch")
	}
	if e.StartedAt.IsZero() || e.FinishedAt.Before(e.StartedAt) || e.FinishedAt.After(now.Add(time.Minute)) || e.FinishedAt.After(execution.Deadline) {
		return fmt.Errorf("evidence outside execution budget")
	}
	if len(e.Log) > 65536 || e.LogHash != HashBytes([]byte(e.Log)) {
		return fmt.Errorf("invalid log hash or size")
	}
	if e.Outcome != "passed" && e.Outcome != "assertion_failed" {
		return fmt.Errorf("script blocked: %s: %s", e.Outcome, e.Reason)
	}
	if e.ReportID != runID || e.ReportHash != HashBytes([]byte(e.Report)) {
		return fmt.Errorf("invalid report identity or hash")
	}
	outcome, err := ValidateReport([]byte(e.Report), runID, e.Source, e.WorkflowVersion, execution.Script)
	if err != nil {
		return err
	}
	if outcome != e.Outcome || (outcome == "passed" && e.ExitCode != 0) || (outcome == "assertion_failed" && e.ExitCode == 0) {
		return fmt.Errorf("exit code and required checks disagree")
	}
	return nil
}
