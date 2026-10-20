package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIssueRelationCLIApprovalCarriesInspectedEvidence(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("MULTICA_AGENT_ID", "")
	t.Setenv("MULTICA_TASK_ID", "")
	version := strings.Repeat("a", 64)
	var observed struct {
		Revision int64  `json:"revision"`
		Version  string `json:"evidence_version"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/issues/"+testIssueUUID && r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": testIssueUUID, "identifier": "MUL-1", "title": "review input", "status": "in_review"})
			return
		}
		if r.URL.Path == "/api/issues/"+testIssueUUID+"/relations/edge/accept" && r.Method == http.MethodPost {
			if err := json.NewDecoder(r.Body).Decode(&observed); err != nil {
				t.Errorf("invalid approval body: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]bool{"accepted": true})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	t.Setenv("MULTICA_SERVER_URL", server.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	t.Setenv("MULTICA_TOKEN", "synthetic-token")
	cmd, _, err := newIssueRelationCmd().Find([]string{"accept"})
	if err != nil {
		t.Fatal(err)
	}
	_ = cmd.Flags().Set("output", "json")
	_ = cmd.Flags().Set("revision", "7")
	if _, err := captureStdout(t, func() error { return runIssueRelation(cmd, []string{testIssueUUID, "edge"}) }); err == nil {
		t.Fatal("accepted without an inspected evidence version")
	}
	_ = cmd.Flags().Set("evidence-version", version)
	output, err := captureStdout(t, func() error { return runIssueRelation(cmd, []string{testIssueUUID, "edge"}) })
	if err != nil {
		t.Fatal(err)
	}
	if observed.Revision != 7 || observed.Version != version {
		t.Fatalf("approval scope=%+v", observed)
	}
	if !strings.Contains(output, `"accepted": true`) {
		t.Fatalf("missing acknowledgement: %s", output)
	}
}
