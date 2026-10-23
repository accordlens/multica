package handler

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func nativeSourceResult(t *testing.T, runID, sha, dirty, outcome string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"workflow_evidence": map[string]any{
		"run_id": runID, "source": map[string]string{"sha": sha, "dirty_hash": dirty},
		"outcome": outcome, "finished_at": "2026-10-23T10:00:00Z",
	}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestNativeSourceReplayAndRevisionHistory(t *testing.T) {
	const runID = "00000000-0000-0000-0000-000000000001"
	sha, dirty := strings.Repeat("a", 40), strings.Repeat("b", 64)
	raw := nativeSourceResult(t, runID, strings.ToUpper(sha), dirty, "assertion_failed")
	links := withNativeEvidenceCommit(nil, runID, "test", raw)
	if len(links) != 1 || links[0].RunID != runID || links[0].CommitSHA != sha || links[0].DirtyHash != dirty || links[0].Step != "test" || links[0].Source != "workflow_evidence" {
		t.Fatalf("incorrect source identity: %+v", links)
	}
	first := links[0]
	for range 3 {
		links = withNativeEvidenceCommit(links, runID, "test", raw)
	}
	if len(links) != 1 || !reflect.DeepEqual(links[0], first) {
		t.Fatal("reconnect duplicated or rewrote the source")
	}
	// Outcomes describe evidence; they neither change source identity nor grant PASS.
	for _, outcome := range []string{"passed", "cancelled", "timed_out", "skipped", ""} {
		links = withNativeEvidenceCommit(links, runID, "test", nativeSourceResult(t, runID, sha, dirty, outcome))
	}
	if len(links) != 1 {
		t.Fatal("outcome changed a source pointer")
	}
	for _, changed := range [][]byte{
		nativeSourceResult(t, runID, strings.Repeat("c", 40), dirty, "passed"),
		nativeSourceResult(t, runID, sha, strings.Repeat("d", 64), "passed"),
	} {
		links = withNativeEvidenceCommit(links, runID, "test", changed)
	}
	if len(links) != 3 || !reflect.DeepEqual(links[0], first) {
		t.Fatal("new revision replaced historical source evidence")
	}
}

func TestNativeSourceCorrectionSurvivesReconnect(t *testing.T) {
	const runID = "00000000-0000-0000-0000-000000000001"
	raw := nativeSourceResult(t, runID, strings.Repeat("a", 40), "", "passed")
	links := withNativeEvidenceCommit(nil, runID, "test", raw)
	removed := time.Date(2026, 10, 23, 10, 1, 0, 0, time.UTC)
	links[0].RemovedAt, links[0].RemovedBy = &removed, "member:corrector"
	context, err := json.Marshal(map[string]any{"provenance_links": links, "hybrid_workflow": map[string]string{"version": "frozen"}})
	if err != nil {
		t.Fatal(err)
	}
	stored, valid := decodeRunLinks(context)
	if !valid {
		t.Fatal("could not decode retained correction")
	}
	for range 3 {
		stored = withNativeEvidenceCommit(stored, runID, "test", raw)
	}
	if len(stored) != 1 || stored[0].RemovedAt == nil || !stored[0].RemovedAt.Equal(removed) || stored[0].RemovedBy != "member:corrector" {
		t.Fatalf("replay lost the correction: %+v", stored)
	}
}

func TestNativeSourceRejectsUnboundOrMalformedEvidence(t *testing.T) {
	const runID = "00000000-0000-0000-0000-000000000001"
	valid := nativeSourceResult(t, runID, strings.Repeat("a", 40), "", "passed")
	for name, raw := range map[string][]byte{
		"empty":               nil,
		"malformed":           []byte(`{"workflow_evidence":`),
		"missing":             []byte(`{}`),
		"null":                []byte(`{"workflow_evidence":null}`),
		"other run":           nativeSourceResult(t, "other", strings.Repeat("a", 40), "", "passed"),
		"short SHA":           nativeSourceResult(t, runID, "abcdef", "", "passed"),
		"bad SHA":             nativeSourceResult(t, runID, strings.Repeat("g", 40), "", "passed"),
		"oversize dirty hash": nativeSourceResult(t, runID, strings.Repeat("a", 40), strings.Repeat("b", 257), "passed"),
		"missing timestamp":   []byte(strings.ReplaceAll(string(valid), `"finished_at":"2026-10-23T10:00:00Z",`, "")),
	} {
		t.Run(name, func(t *testing.T) {
			if link, ok := nativeEvidenceCommit(runID, "test", raw); ok {
				t.Fatalf("accepted unbound source: %+v", link)
			}
		})
	}
}

func TestDecodeRunLinksFailsOnMalformedStoredData(t *testing.T) {
	for _, raw := range []string{`{`, `{"provenance_links":{}}`, `{"provenance_links":[{"created_at":"invalid"}]}`} {
		if _, valid := decodeRunLinks([]byte(raw)); valid {
			t.Fatalf("accepted malformed stored links: %s", raw)
		}
	}
	for _, raw := range []string{"", `{}`, `{"provenance_links":null}`} {
		links, valid := decodeRunLinks([]byte(raw))
		if !valid || links == nil || len(links) != 0 {
			t.Fatalf("empty source must project an empty list: %q", raw)
		}
	}
}
