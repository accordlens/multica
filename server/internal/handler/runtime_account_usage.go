package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	obsmetrics "github.com/multica-ai/multica/server/internal/metrics"
	"github.com/multica-ai/multica/server/pkg/accountusage"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func runtimeAccountSnapshot(rt db.AgentRuntime) *accountusage.Snapshot {
	var metadata map[string]json.RawMessage
	if json.Unmarshal(rt.Metadata, &metadata) != nil {
		return nil
	}
	s, err := accountusage.Decode(metadata[accountusage.MetadataKey])
	if err != nil {
		return nil
	}
	return &s
}

func accountUsageRecord(rt db.AgentRuntime) accountusage.Record {
	ownerID := ""
	if rt.OwnerID.Valid {
		ownerID = uuidToString(rt.OwnerID)
	}
	return accountusage.Record{RuntimeID: uuidToString(rt.ID), OwnerID: ownerID, Snapshot: runtimeAccountSnapshot(rt)}
}

func (h *Handler) GetRuntimeAccountUsage(w http.ResponseWriter, r *http.Request) {
	rt, member, ok := h.requireRuntimeReadAccess(w, r, obsmetrics.RuntimeLookupSourceRuntimeAPI, chi.URLParam(r, "runtimeId"))
	if !ok {
		return
	}
	// Even workspace admins may only pool evidence from runtimes they can read.
	runtimes, err := h.Queries.ListVisibleAgentRuntimes(r.Context(), db.ListVisibleAgentRuntimesParams{WorkspaceID: rt.WorkspaceID, OwnerID: member.UserID})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read account snapshots")
		return
	}
	records := make([]accountusage.Record, 0, len(runtimes))
	for _, runtime := range runtimes {
		records = append(records, accountUsageRecord(runtime))
	}
	writeJSON(w, http.StatusOK, accountusage.Project(records, uuidToString(rt.ID), time.Now()))
}

func (h *Handler) RecordRuntimeAccountUsage(w http.ResponseWriter, r *http.Request) {
	if isMachineCredentialActor(r) {
		writeError(w, http.StatusForbidden, "account snapshots require a member session")
		return
	}
	rt, member, ok := h.requireRuntimeReadAccess(w, r, obsmetrics.RuntimeLookupSourceRuntimeAPI, chi.URLParam(r, "runtimeId"))
	if !ok {
		return
	}
	if !rt.OwnerID.Valid || rt.OwnerID != member.UserID {
		writeError(w, http.StatusForbidden, "only the runtime owner can record account usage")
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, accountusage.MaxSnapshotBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "account snapshot is too large or unreadable")
		return
	}
	snapshot, err := accountusage.Decode(data)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	now := time.Now().UTC()
	if err := snapshot.Validate(now); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if rt.Provider != "claude" {
		writeError(w, http.StatusBadRequest, "this snapshot requires a Claude runtime")
		return
	}
	snapshot.Provenance.RecordedBy = uuidToString(member.UserID)
	snapshot.Provenance.RecordedAt = &now
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to start snapshot transaction")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)
	locked, err := qtx.LockAgentRuntime(r.Context(), rt.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to lock runtime")
		return
	}
	if !locked.OwnerID.Valid || locked.OwnerID != member.UserID || locked.WorkspaceID != rt.WorkspaceID || locked.Provider != "claude" {
		writeError(w, http.StatusForbidden, "runtime ownership or provider changed; refresh before recording")
		return
	}
	if current := runtimeAccountSnapshot(locked); current != nil {
		changed, err := accountusage.Replacement(*current, snapshot)
		if err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		if !changed {
			writeJSON(w, http.StatusOK, accountusage.Project([]accountusage.Record{accountUsageRecord(locked)}, uuidToString(rt.ID), now))
			return
		}
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid account snapshot")
		return
	}
	updated, err := qtx.UpdateAgentRuntimeAccountUsage(r.Context(), db.UpdateAgentRuntimeAccountUsageParams{ID: rt.ID, OwnerID: member.UserID, Snapshot: encoded})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to record snapshot")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to commit snapshot")
		return
	}
	h.publish(protocol.EventDaemonRegister, uuidToString(rt.WorkspaceID), "member", uuidToString(member.UserID), map[string]any{"action": "account_snapshot", "runtime_id": uuidToString(rt.ID), "observation_id": snapshot.ObservationID})
	writeJSON(w, http.StatusOK, accountusage.Project([]accountusage.Record{accountUsageRecord(updated)}, uuidToString(rt.ID), now))
}
