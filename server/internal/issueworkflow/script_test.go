package issueworkflow

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testScript() Script {
	return Script{ID: "code", Version: "1", Checkout: "repo", Directory: ".", Command: []string{"node", "gate.mjs"}, RequiredEnv: []string{"CI"}, TimeoutSeconds: 5, BudgetSeconds: 60, MaxRepairs: 1, RequiredChecks: []string{"unit"}, Files: map[string]string{"gate.mjs": strings.Repeat("a", 64)}}
}

func TestScriptDefinitionRoundTripAndVersion(t *testing.T) {
	script := testScript()
	steps := Arrange([]Step{{StatusKey: "implement", Handler: Handler{Type: HandlerAgent, ID: testAgentID}, NextStatusKey: "verify"}, {StatusKey: "verify", Handler: Handler{Type: HandlerNone}, Script: &script, NextStatusKey: "code_review", BackStatusKey: "implement"}, {StatusKey: "code_review", Handler: Handler{Type: HandlerAgent, ID: "0f0e0d0c-0b0a-4909-8807-060504030202"}}})
	catalog := testCatalog()
	catalog["verify"] = CatalogEntry{Key: "verify", Category: "started"}
	if err := Validate("Code", "", steps, false, catalog); err != nil {
		t.Fatal(err)
	}
	raw, err := EncodeSteps(steps)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeSteps(raw)
	if err != nil || !reflect.DeepEqual(steps, decoded) {
		t.Fatalf("script lost: %s / %v", raw, err)
	}
	def := Definition{Steps: decoded}
	version := def.Version()
	decoded[2].Script.MaxRepairs = 2
	if def.Version() == version {
		t.Fatal("gate change reused definition version")
	}
	decoded[2].Script = &script
	decoded[3].Handler.ID = testAgentID
	if err := Validate("Code", "", decoded, false, catalog); err == nil {
		t.Fatal("author accepted as reviewer")
	}
}

func TestScriptRejectsUnsafeAndUnboundedDefinitions(t *testing.T) {
	cases := map[string]func(*Script){"shell path": func(s *Script) { s.Command[0] = "/bin/sh" }, "Windows drive": func(s *Script) { s.Checkout = `C:\repo` }, "UNC": func(s *Script) { s.Directory = `\\host\repo` }, "traversal": func(s *Script) { s.Directory = "../repo" }, "secret": func(s *Script) { s.RequiredEnv = []string{"OPENAI_API_KEY"} }, "no checks": func(s *Script) { s.RequiredChecks = nil }, "no pins": func(s *Script) { s.Files = nil }, "unbounded repairs": func(s *Script) { s.MaxRepairs = 99 }, "unbounded time": func(s *Script) { s.TimeoutSeconds = 0 }, "duplicate check": func(s *Script) { s.RequiredChecks = []string{"unit", "unit"} }}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := testScript()
			mutate(&s)
			if s.Validate() == nil {
				t.Fatal("unsafe definition accepted")
			}
		})
	}
}

func TestRequiredReportsCannotBeSkippedEmptyOrStale(t *testing.T) {
	script := testScript()
	source := Source{SHA: strings.Repeat("a", 40)}
	base := Report{RunID: "run", SourceSHA: source.SHA, WorkflowVersion: "definition", ScriptVersion: script.Version, Checks: []Check{{Name: "unit", Passed: 2}}}
	cases := map[string]struct {
		mutate func(*Report)
		want   string
	}{
		"pass": {func(*Report) {}, "passed"}, "assertion": {func(r *Report) { r.Checks[0].Failed = 1 }, "assertion_failed"},
		"old run": {func(r *Report) { r.RunID = "old" }, "stale_report"}, "old SHA": {func(r *Report) { r.SourceSHA = strings.Repeat("b", 40) }, "stale_report"}, "old definition": {func(r *Report) { r.WorkflowVersion = "old" }, "stale_report"}, "old script": {func(r *Report) { r.ScriptVersion = "old" }, "stale_report"},
		"empty": {func(r *Report) { r.Checks = nil }, "no_tests"}, "zero": {func(r *Report) { r.Checks[0].Passed = 0 }, "no_tests"}, "skipped": {func(r *Report) { r.Checks[0].Skipped = 1 }, "skipped"}, "duplicate": {func(r *Report) { r.Checks = append(r.Checks, r.Checks[0]) }, "invalid_report"}, "negative": {func(r *Report) { r.Checks[0].Failed = -1 }, "invalid_report"},
	}
	cases["infrastructure"] = struct {
		mutate func(*Report)
		want   string
	}{func(r *Report) { r.InfrastructureError = "go tool unavailable" }, "infrastructure_failed"}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := base
			r.Checks = append([]Check{}, base.Checks...)
			tc.mutate(&r)
			raw, _ := json.Marshal(r)
			got, _ := ValidateReport(raw, "run", source, "definition", script)
			if got != tc.want {
				t.Fatalf("%s, want %s", got, tc.want)
			}
		})
	}
	if got, _ := ValidateReport(nil, "run", source, "definition", script); got != "invalid_report" {
		t.Fatal(got)
	}
}

func TestEvidenceRejectsForgedPassAndReceiptMutation(t *testing.T) {
	script := testScript()
	source := Source{SHA: strings.Repeat("a", 40)}
	now := time.Now().UTC()
	ctx := Execution{Source: source, Script: script, WorkflowVersion: "definition", Deadline: now.Add(time.Minute)}
	report, _ := json.Marshal(Report{RunID: "run", SourceSHA: source.SHA, WorkflowVersion: ctx.WorkflowVersion, ScriptVersion: script.Version, Checks: []Check{{Name: "unit", Passed: 1}}})
	base := Evidence{RunID: "run", Source: source, WorkflowVersion: ctx.WorkflowVersion, ScriptVersion: script.Version, ScriptHash: Hash(script), Outcome: "passed", StartedAt: now.Add(-time.Second), FinishedAt: now, ReportID: "run", ReportHash: HashBytes(report), Report: string(report), LogHash: HashBytes(nil)}
	if err := base.Validate("run", ctx, now); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*Evidence){"new SHA": func(e *Evidence) { e.Source.SHA = strings.Repeat("b", 40) }, "dirty": func(e *Evidence) { e.Source.DirtyHash = "dirty" }, "report mutation": func(e *Evidence) { e.Report = `{}` }, "log mutation": func(e *Evidence) { e.Log = "changed" }, "false exit": func(e *Evidence) { e.ExitCode = 1 }, "late": func(e *Evidence) { e.FinishedAt = ctx.Deadline.Add(time.Second) }, "wrong command": func(e *Evidence) { e.ScriptHash = "old" }}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := base
			mutate(&e)
			if e.Validate("run", ctx, now) == nil {
				t.Fatal("invalid evidence accepted")
			}
		})
	}
}
