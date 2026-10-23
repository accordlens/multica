package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/accountusage"
)

func TestRuntimeAccountUsageTablePreservesUnknownZeroAndTimezone(t *testing.T) {
	data, err := os.ReadFile("../../pkg/accountusage/testdata/claude-manual.json")
	if err != nil {
		t.Fatal(err)
	}
	s, err := accountusage.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	s.ObservedAt = time.Now().UTC().Add(-time.Minute)
	zero := 0.0
	s.Limits[0].Remaining = &zero
	p := accountusage.Project([]accountusage.Record{{RuntimeID: "runtime", OwnerID: "owner", Snapshot: &s}}, "runtime", time.Now())
	var out bytes.Buffer
	if err := printRuntimeAccountUsage(&out, p); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"0%", "UNKNOWN", "37%", "Europe/Warsaw", "manual / claude_code_usage", "never resumes"} {
		if !strings.Contains(out.String(), expected) {
			t.Fatalf("missing %q in %s", expected, out.String())
		}
	}
}

func TestRuntimeAccountUsageTableMissingSnapshotIsNotZero(t *testing.T) {
	var out bytes.Buffer
	p := accountusage.Project(nil, "runtime", time.Now())
	if err := printRuntimeAccountUsage(&out, p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "UNKNOWN") || strings.Contains(out.String(), "0%") {
		t.Fatal(out.String())
	}
}
