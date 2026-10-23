package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/accountusage"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func accountSnapshotFixture(t *testing.T) accountusage.Snapshot {
	t.Helper()
	data, err := os.ReadFile("../../pkg/accountusage/testdata/claude-manual.json")
	if err != nil {
		t.Fatal(err)
	}
	s, err := accountusage.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	s.ObservedAt = time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	reset := s.ObservedAt.Add(time.Hour)
	s.Limits[0].ResetAt = &reset
	return s
}

func TestRuntimeAccountSnapshotReplayDoesNotResumeOrDuplicateRuns(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	runtimeID := fx.Runtime(t, "usage-account-a", testutil.Cols{"provider": "claude"})
	otherRuntime := fx.Runtime(t, "usage-account-b", testutil.Cols{"provider": "claude"})
	a := fx.Agent(t, "shared-pool-agent-a", runtimeID)
	b := fx.Agent(t, "shared-pool-agent-b", runtimeID)
	other := fx.Agent(t, "other-pool-agent", otherRuntime)
	fx.Task(t, a, testutil.Cols{"runtime_id": runtimeID, "status": "failed", "failure_reason": "agent_error.provider_capacity_or_rate_limit"})
	fx.Task(t, b, testutil.Cols{"runtime_id": runtimeID, "status": "queued"})
	fx.Task(t, other, testutil.Cols{"runtime_id": otherRuntime, "status": "queued"})
	s := accountSnapshotFixture(t)
	s.Availability = "rate_limited"
	zero := 0.0
	s.Limits[0].Remaining = &zero
	path := "/api/runtimes/" + runtimeID + "/usage/account"
	put := func(snapshot accountusage.Snapshot, want int) accountusage.Projection {
		t.Helper()
		res := testutil.Call(t, testHandler.RecordRuntimeAccountUsage, withURLParams(newRequestAs(testUserID, http.MethodPut, path, snapshot), "runtimeId", runtimeID)).Want(want)
		var p accountusage.Projection
		if want == http.StatusOK {
			res.JSON(&p)
		}
		return p
	}
	first := put(s, http.StatusOK)
	replay := put(s, http.StatusOK)
	if first.Snapshot.Provenance.RecordedBy != testUserID || !first.Snapshot.Provenance.RecordedAt.Equal(*replay.Snapshot.Provenance.RecordedAt) {
		t.Fatal("replay rewrote provenance or failed to record its member source")
	}
	if replay.Snapshot.Availability != "rate_limited" || replay.Snapshot.Limits[0].Remaining == nil || *replay.Snapshot.Limits[0].Remaining != 0 {
		t.Fatal("recording cleared observed rate limit")
	}
	changed := s
	changed.Availability = "available"
	put(changed, http.StatusConflict)
	var total, failed, queued int
	err := testPool.QueryRow(context.Background(), `SELECT count(*), count(*) FILTER (WHERE status='failed'), count(*) FILTER (WHERE status='queued') FROM agent_task_queue WHERE agent_id::text = ANY($1::text[])`, []string{a, b, other}).Scan(&total, &failed, &queued)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || failed != 1 || queued != 2 {
		t.Fatalf("snapshot changed workflow runs: total=%d failed=%d queued=%d", total, failed, queued)
	}
	var unknown accountusage.Projection
	testutil.Call(t, testHandler.GetRuntimeAccountUsage, withURLParams(newRequestAs(testUserID, http.MethodGet, "/api/runtimes/"+otherRuntime+"/usage/account", nil), "runtimeId", otherRuntime)).Want(http.StatusOK).JSON(&unknown)
	if unknown.State != "unknown" || unknown.Snapshot != nil {
		t.Fatalf("a second account inherited the first pool: %+v", unknown)
	}
}

func TestRuntimeAccountSnapshotOwnerGateAndMetadataPrivacy(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	runtimeID, ownerID, memberID := runtimeVisibilityFixture(t)
	path := "/api/runtimes/" + runtimeID + "/usage/account"
	testutil.Call(t, testHandler.GetRuntimeAccountUsage, withURLParams(newRequestAs(memberID, http.MethodGet, path, nil), "runtimeId", runtimeID)).Want(http.StatusNotFound)
	request := withURLParams(newRequestAs(ownerID, http.MethodPut, path, nil), "runtimeId", runtimeID)
	request.Header.Set("X-Actor-Source", "task_token")
	testutil.Call(t, testHandler.RecordRuntimeAccountUsage, request).Want(http.StatusForbidden)
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	s := accountSnapshotFixture(t)
	metadata, err := json.Marshal(map[string]any{accountusage.MetadataKey: s, "cli_version": "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	publicID := fx.Runtime(t, "public-usage-pool", testutil.Cols{"provider": "claude", "visibility": "public", "metadata": metadata})
	// A member can read a shared pool but cannot attest its account identity.
	testutil.Call(t, testHandler.RecordRuntimeAccountUsage, withURLParams(newRequestAs(memberID, http.MethodPut, "/api/runtimes/"+publicID+"/usage/account", map[string]any{}), "runtimeId", publicID)).Want(http.StatusForbidden)
	var runtimes []AgentRuntimeResponse
	testutil.Call(t, testHandler.ListAgentRuntimes, newRequestAs(testUserID, http.MethodGet, "/api/runtimes", nil)).Want(http.StatusOK).JSON(&runtimes)
	for _, rt := range runtimes {
		if rt.ID == publicID {
			encoded, _ := json.Marshal(rt.Metadata)
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatal(err)
			}
			if _, leaked := fields[accountusage.MetadataKey]; leaked {
				t.Fatal("account evidence leaked through runtime metadata")
			}
			if _, preserved := fields["cli_version"]; !preserved {
				t.Fatal("unrelated runtime metadata disappeared")
			}
			return
		}
	}
	t.Fatal("expected public runtime in list")
}

func TestRuntimeAccountSnapshotSurvivesRegisterAndIsClearedOnOwnerChange(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	s := accountSnapshotFixture(t)
	metadata, err := json.Marshal(map[string]any{accountusage.MetadataKey: s, "offline_reason": "old-fixture-reason"})
	if err != nil {
		t.Fatal(err)
	}
	otherOwner := fx.Insert(t, "user", testutil.Cols{"name": "Usage fixture owner", "email": "usage-registration-" + testWorkspaceID + "@fixture.invalid"})
	runtimeID := fx.Runtime(t, "account-registration", testutil.Cols{"provider": "claude", "daemon_id": "usage129-registration-fixture", "metadata": metadata})
	rt, err := testHandler.Queries.GetAgentRuntime(ctx, parseUUID(runtimeID))
	if err != nil {
		t.Fatal(err)
	}
	params := db.UpsertAgentRuntimeParams{WorkspaceID: rt.WorkspaceID, DaemonID: rt.DaemonID, Name: rt.Name, RuntimeMode: rt.RuntimeMode, Provider: rt.Provider, Status: "online", Metadata: []byte(`{"cli_version":"next-fixture"}`), OwnerID: rt.OwnerID}
	if _, err := testHandler.Queries.UpsertAgentRuntime(ctx, params); err != nil {
		t.Fatal(err)
	}
	rt, err = testHandler.Queries.GetAgentRuntime(ctx, rt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := runtimeAccountSnapshot(rt); got == nil || got.ObservationID != s.ObservationID {
		t.Fatal("re-registration lost account evidence")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(rt.Metadata, &fields); err != nil {
		t.Fatal(err)
	}
	if _, stale := fields["offline_reason"]; stale {
		t.Fatal("registration retained daemon-owned offline reason")
	}
	params.OwnerID = parseUUID(otherOwner)
	if _, err := testHandler.Queries.UpsertAgentRuntime(ctx, params); err != nil {
		t.Fatal(err)
	}
	rt, err = testHandler.Queries.GetAgentRuntime(ctx, rt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if runtimeAccountSnapshot(rt) != nil {
		t.Fatal("old owner's account evidence survived an ownership change")
	}
}
