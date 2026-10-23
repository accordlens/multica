package accountusage

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

func fixture(t *testing.T) Snapshot {
	t.Helper()
	data, err := os.ReadFile("testdata/claude-manual.json")
	if err != nil {
		t.Fatal(err)
	}
	s, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(s.ObservedAt); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSharedPoolIsSelectedOnceAndAccountsStaySeparate(t *testing.T) {
	s := fixture(t)
	newer := fixture(t)
	newer.ObservedAt = newer.ObservedAt.Add(time.Minute)
	newer.ObservationID = "next-observation"
	used := 40.0
	newer.Limits[0].Used = &used
	other := fixture(t)
	other.AccountRef = "pool-b"
	records := []Record{{"runtime-a", "owner", &s}, {"runtime-b", "owner", &newer}, {"runtime-c", "owner", &other}, {"runtime-d", "other-owner", &s}}
	// Two agents bound to runtime-a read the same account projection. A second
	// runtime using that account adds a binding, never another limit allowance.
	for range 2 {
		p := Project(records, "runtime-a", newer.ObservedAt)
		if p.State != "fresh" || *p.Snapshot.Limits[0].Used != 40 || strings.Join(p.RuntimeIDs, ",") != "runtime-a,runtime-b" {
			t.Fatalf("one shared pool expected, got %+v", p)
		}
	}
	for _, runtimeID := range []string{"runtime-c", "runtime-d"} {
		p := Project(records, runtimeID, newer.ObservedAt)
		if len(p.RuntimeIDs) != 1 || p.RuntimeIDs[0] != runtimeID {
			t.Fatalf("account/owner scopes merged: %+v", p)
		}
	}
}

func TestUnknownZeroAndFreshnessStayIndependent(t *testing.T) {
	s := fixture(t)
	zero := 0.0
	s.Limits[0].Remaining = &zero
	s.Availability = "rate_limited"
	records := []Record{{"runtime", "owner", &s}}
	p := Project(records, "runtime", s.ObservedAt)
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"remaining":0`) || !strings.Contains(string(data), `"remaining":null`) {
		t.Fatalf("zero and unknown collapsed: %s", data)
	}
	p = Project(records, "runtime", s.ObservedAt.Add(900*time.Second))
	if p.State != "stale" || p.Reason != "observation_expired" || p.Snapshot.Availability != "rate_limited" || *p.Snapshot.Limits[0].Remaining != 0 {
		t.Fatalf("stale evidence changed observed availability: %+v", p)
	}
	p = Project(records, "runtime", s.Limits[0].ResetAt.Add(time.Second))
	if p.State != "stale" || p.Reason != "reset_requires_observation" || *p.Snapshot.Limits[0].Remaining != 0 {
		t.Fatalf("reset must not refill a pool or resume work: %+v", p)
	}
	if got := Project(nil, "runtime", s.ObservedAt); got.State != "unknown" || got.Snapshot != nil {
		t.Fatalf("missing is not zero: %+v", got)
	}
}

func TestTimezoneAndDifferentWindowsPreserveSourceInstants(t *testing.T) {
	s := fixture(t)
	reset, err := time.Parse(time.RFC3339, "2026-10-25T02:30:00+01:00")
	if err != nil {
		t.Fatal(err)
	}
	s.Limits[0].ResetAt = &reset
	if err := s.Validate(s.ObservedAt); err != nil {
		t.Fatal(err)
	}
	loc, err := time.LoadLocation(*s.Limits[0].ResetTimezone)
	if err != nil {
		t.Fatal(err)
	}
	if got := reset.In(loc).Format(time.RFC3339); got != "2026-10-25T02:30:00+01:00" {
		t.Fatalf("DST reset shifted: %s", got)
	}
	if s.Limits[0].Window.Kind == s.Limits[1].Window.Kind || *s.Limits[0].Window.DurationSeconds == *s.Limits[1].Window.DurationSeconds {
		t.Fatal("windows collapsed")
	}
	zone := "Mars/Olympus"
	s.Limits[0].ResetTimezone = &zone
	if s.Validate(s.ObservedAt) == nil {
		t.Fatal("invalid timezone accepted")
	}
}

func TestKnownWindowEndExpiresEvidenceWithoutInventingReset(t *testing.T) {
	s := fixture(t)
	s.Limits[0].ResetAt, s.Limits[0].ResetTimezone = nil, nil
	end := s.ObservedAt.Add(time.Minute)
	s.Limits[0].Window.EndsAt = &end
	p := Project([]Record{{"runtime", "owner", &s}}, "runtime", end)
	if p.State != "stale" || p.Reason != "window_requires_observation" || p.Snapshot.Limits[0].ResetAt != nil {
		t.Fatalf("expired window invented a reset: %+v", p)
	}
}

func TestObservationReplayAndConflictsDoNotBecomeNewEvidence(t *testing.T) {
	s := fixture(t)
	if changed, err := Replacement(s, s); changed || err != nil {
		t.Fatalf("replay changed evidence: %v %v", changed, err)
	}
	changed := fixture(t)
	changed.Availability = "rate_limited"
	if _, err := Replacement(s, changed); err == nil {
		t.Fatal("operation ID reused for different evidence")
	}
	changed.ObservationID = "another-observation"
	if _, err := Replacement(s, changed); err == nil {
		t.Fatal("equal timestamp replaced evidence")
	}
	p := Project([]Record{{"a", "owner", &s}, {"b", "owner", &changed}}, "a", s.ObservedAt)
	if p.State != "unknown" || p.Reason != "snapshot_conflict" || p.Snapshot != nil {
		t.Fatalf("conflict must be unknown: %+v", p)
	}
	changed.ObservedAt = changed.ObservedAt.Add(time.Second)
	if replace, err := Replacement(s, changed); !replace || err != nil {
		t.Fatalf("new observation rejected: %v", err)
	}
}

func TestFakeProviderRateLimitUsesExistingFailureReason(t *testing.T) {
	if got := taskfailure.Classify(`API Error: 429 {"type":"error","error":{"type":"rate_limit_error","message":"rate limit exceeded"}}`); got != taskfailure.ReasonAgentProviderCapacityOrRateLimit {
		t.Fatalf("fake provider response classified as %q", got)
	}
	s := fixture(t)
	s.Availability = "rate_limited"
	records := []Record{{"limited", "owner", &s}}
	for range 2 {
		p := Project(records, "limited", s.ObservedAt)
		if p.Snapshot.Availability != "rate_limited" {
			t.Fatal("replay cleared a pause")
		}
	}
	// The projection has no scheduler/task dependency and emits no resume action.
	if p := Project(records, "different-account", s.ObservedAt); p.State != "unknown" {
		t.Fatalf("other route affected: %+v", p)
	}
}

func TestRejectUnsourcedAndMalformedSnapshots(t *testing.T) {
	for _, data := range []string{`null`, `{}`, `{"version":1,"extra":true}`, `{} {}`, strings.Repeat(" ", MaxSnapshotBytes+1)} {
		s, err := Decode([]byte(data))
		if err == nil && s.Validate(time.Now()) == nil {
			t.Fatalf("invalid input accepted: %.40s", data)
		}
	}
	s := fixture(t)
	s.Provenance.Kind = "model_estimate"
	if s.Validate(s.ObservedAt) == nil {
		t.Fatal("unsourced snapshot accepted")
	}
	s = fixture(t)
	if s.Validate(s.ObservedAt.Add(-time.Second)) == nil {
		t.Fatal("future observation accepted")
	}
}
