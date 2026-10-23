package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type IssueProvenanceRequest struct {
	RunID      string `json:"run_id"`
	Kind       string `json:"kind"`
	URL        string `json:"url,omitempty"`
	CommitSHA  string `json:"commit_sha,omitempty"`
	ArtifactID string `json:"artifact_id,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
}

var provenancePRPath = regexp.MustCompile(`/(?:pull|pulls|merge_requests)/[1-9][0-9]*/?$`)

func (h *Handler) ListIssueProvenance(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadDependencyIssue(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	offset := 0
	if value := r.URL.Query().Get("offset"); value != "" {
		var err error
		offset, err = strconv.Atoi(value)
		if err != nil || offset < 0 || offset > 1000000 {
			writeError(w, 400, "invalid offset")
			return
		}
	}
	runs, err := h.Queries.ListIssueProvenanceRuns(r.Context(), db.ListIssueProvenanceRunsParams{IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, PageOffset: int32(offset)})
	if err != nil {
		writeError(w, 500, "could not load run provenance")
		return
	}
	artifacts, err := h.Queries.ListIssueProvenanceArtifacts(r.Context(), db.ListIssueProvenanceArtifactsParams{IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, PageOffset: int32(offset)})
	if err != nil {
		writeError(w, 500, "could not load artifact provenance")
		return
	}
	more := len(runs) > 100 || len(artifacts) > 100
	if len(runs) > 100 {
		runs = runs[:100]
	}
	if len(artifacts) > 100 {
		artifacts = artifacts[:100]
	}
	links := []IssueProvenanceLink{}
	results := []map[string]any{}
	for _, run := range runs {
		stored, valid := decodeRunLinks(run.Context)
		if !valid {
			writeError(w, 500, "could not decode run provenance")
			return
		}
		for _, link := range withNativeEvidenceCommit(stored, uuidToString(run.ID), provenanceEntryStep(run.Context), run.Result) {
			if link.RemovedAt == nil || r.URL.Query().Get("include_removed") == "true" {
				if link.Kind == "artifact" {
					// Recheck visibility, including a later deletion or detach.
					artifactID, parseErr := util.ParseUUID(link.ArtifactID)
					if parseErr != nil {
						continue
					}
					_, err := h.Queries.GetIssueProvenanceArtifact(r.Context(), db.GetIssueProvenanceArtifactParams{ID: artifactID, WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, RunID: run.ID})
					if err != nil {
						continue
					}
				}
				links = append(links, link)
			}
		}
		var result struct {
			Evidence json.RawMessage `json:"workflow_evidence"`
		}
		_ = json.Unmarshal(run.Result, &result)
		var snapshot struct {
			WorkflowID string          `json:"workflow_id"`
			Steps      json.RawMessage `json:"workflow_steps"`
		}
		_ = json.Unmarshal(run.Context, &snapshot)
		definitionHash := ""
		if len(snapshot.Steps) > 0 {
			var definition any
			if json.Unmarshal(snapshot.Steps, &definition) == nil {
				canonical, _ := json.Marshal(definition)
				sum := sha256.Sum256(canonical)
				definitionHash = hex.EncodeToString(sum[:])
			}
		}
		results = append(results, map[string]any{"run_id": uuidToString(run.ID), "step": provenanceEntryStep(run.Context), "status": run.Status, "created_at": run.CreatedAt.Time, "workflow_evidence": result.Evidence, "workflow_id": snapshot.WorkflowID, "definition_sha256": definitionHash})
	}
	files := []map[string]any{}
	for _, a := range artifacts {
		files = append(files, map[string]any{"id": uuidToString(a.ID), "filename": a.Filename, "run_id": uuidToString(a.TaskID), "created_at": a.CreatedAt.Time})
	}
	writeJSON(w, 200, map[string]any{"links": links, "runs": results, "artifacts": files, "has_more": more, "offset": offset})
}

func (h *Handler) CreateIssueProvenance(w http.ResponseWriter, r *http.Request) {
	h.mutateIssueProvenance(w, r, false)
}

func (h *Handler) RemoveIssueProvenance(w http.ResponseWriter, r *http.Request) {
	h.mutateIssueProvenance(w, r, true)
}

func (h *Handler) mutateIssueProvenance(w http.ResponseWriter, r *http.Request, remove bool) {
	issue, ok := h.loadDependencyIssue(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	var req IssueProvenanceRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&req) != nil {
		writeError(w, 400, "invalid provenance")
		return
	}
	runID, ok := parseUUIDOrBadRequest(w, req.RunID, "run id")
	if !ok {
		return
	}
	actorType, actorID := h.resolveActor(r, requestUserID(r), uuidToString(issue.WorkspaceID))
	if actorType == "agent" {
		own := h.agentRunOnIssue(r, actorType, issue)
		if own == nil || own.ID != runID {
			writeError(w, 403, "agents may report provenance only for their own run")
			return
		}
	}
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, 500, "could not save provenance")
		return
	}
	defer tx.Rollback(r.Context())
	q := h.Queries.WithTx(tx)
	run, err := q.LockIssueProvenanceRun(r.Context(), db.LockIssueProvenanceRunParams{RunID: runID, IssueID: issue.ID, WorkspaceID: issue.WorkspaceID})
	if err != nil {
		writeError(w, 404, "run not found")
		return
	}
	req.RunID = uuidToString(run.ID)
	links, valid := decodeRunLinks(run.Context)
	if !valid {
		writeError(w, 500, "could not decode provenance")
		return
	}
	var id string
	nativePRChanged := false
	if remove {
		id = chi.URLParam(r, "linkID")
		found := false
		for i := range links {
			if links[i].ID == id {
				found = true
				if links[i].RemovedAt != nil {
					writeJSON(w, 200, map[string]bool{"removed": true})
					return
				}
				now := time.Now().UTC()
				links[i].RemovedAt = &now
				links[i].RemovedBy = actorType + ":" + actorID
			}
		}
		if !found {
			if derived, ok := nativeEvidenceCommit(uuidToString(run.ID), provenanceEntryStep(run.Context), run.Result); ok && derived.ID == id {
				now := time.Now().UTC()
				derived.RemovedAt = &now
				derived.RemovedBy = actorType + ":" + actorID
				links = append(links, derived)
				found = true
			}
		}
		if !found {
			writeError(w, 404, "provenance link not found")
			return
		}
	} else {
		if run.Status != "running" && run.Status != "completed" && run.Status != "failed" && run.Status != "cancelled" {
			writeError(w, 409, "source run has not started")
			return
		}
		req.CommitSHA = strings.ToLower(req.CommitSHA)
		req.SHA256 = strings.ToLower(req.SHA256)
		switch req.Kind {
		case "commit":
			if !provenanceSHA.MatchString(req.CommitSHA) || req.URL != "" || req.ArtifactID != "" || req.SHA256 != "" {
				writeError(w, 400, "commit requires only an exact commit SHA")
				return
			}
		case "pull_request":
			u, err := url.Parse(req.URL)
			if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || !provenancePRPath.MatchString(u.Path) || u.RawQuery != "" || u.Fragment != "" || req.CommitSHA != "" || req.ArtifactID != "" || req.SHA256 != "" {
				writeError(w, 400, "pull_request requires an HTTPS pull request URL")
				return
			}
			u.Host = strings.ToLower(u.Host)
			if u.Port() == "443" {
				u.Host = u.Hostname()
			}
			if u.Host == "github.com" {
				u.Path = strings.ToLower(u.Path)
			}
			u.Path = strings.TrimRight(u.Path, "/")
			req.URL = u.String()
			for _, old := range links {
				if old.Kind == "pull_request" && old.URL == req.URL && old.RemovedAt != nil {
					writeJSON(w, 200, map[string]any{"id": old.ID, "created": false, "removed": true})
					return
				}
			}
			pr, found, lookupErr := h.findPullRequestForLink(r.Context(), issue.WorkspaceID, LinkIssuePullRequestRequest{URL: req.URL})
			if lookupErr != nil {
				writeError(w, 500, "could not resolve pull request")
				return
			}
			if found {
				if pr.github {
					excluded, checkErr := q.IsPullRequestExcludedFromIssue(r.Context(), db.IsPullRequestExcludedFromIssueParams{IssueID: issue.ID, PullRequestID: pr.ID})
					if checkErr != nil {
						writeError(w, 500, "could not check PR correction")
						return
					}
					if !excluded {
						var affected int64
						affected, err = q.LinkIssueToPullRequest(r.Context(), db.LinkIssueToPullRequestParams{IssueID: issue.ID, PullRequestID: pr.ID})
						nativePRChanged = affected > 0
					}
				} else {
					excluded, checkErr := q.IsPullRequestExcludedFromIssue(r.Context(), db.IsPullRequestExcludedFromIssueParams{IssueID: issue.ID, PullRequestID: pr.ID})
					if checkErr != nil {
						writeError(w, 500, "could not check PR correction")
						return
					}
					if !excluded {
						var affected int64
						affected, err = q.LinkIssueToVCSPullRequest(r.Context(), db.LinkIssueToVCSPullRequestParams{IssueID: issue.ID, PullRequestID: pr.ID})
						nativePRChanged = affected > 0
					}
				}
				if err != nil {
					writeError(w, 500, "could not link pull request")
					return
				}
			}
		case "artifact":
			id, valid := parseUUIDOrBadRequest(w, req.ArtifactID, "artifact id")
			if !valid {
				return
			}
			a, err := q.GetIssueProvenanceArtifact(r.Context(), db.GetIssueProvenanceArtifactParams{ID: id, WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, RunID: run.ID})
			if err != nil {
				writeError(w, 404, "artifact not found")
				return
			}
			req.ArtifactID = uuidToString(a.ID)
			if !provenanceHash.MatchString(req.SHA256) || req.URL != "" || req.CommitSHA != "" {
				writeError(w, 400, "artifact requires a SHA256 report hash")
				return
			}
		default:
			writeError(w, 400, "kind must be commit, pull_request or artifact")
			return
		}
		id = provenanceLinkID(req.Kind, req.URL, req.CommitSHA, req.ArtifactID, req.SHA256, "")
		for _, link := range links {
			if link.ID == id {
				if err := tx.Commit(r.Context()); err != nil {
					writeError(w, 500, "could not reconcile provenance")
					return
				}
				if nativePRChanged {
					h.publishIssueRelationChanges(issue, issue)
					h.publishIssuePullRequestsChanged(uuidToString(issue.WorkspaceID), issue.ID)
				}
				writeJSON(w, 200, map[string]any{"id": id, "created": false, "removed": link.RemovedAt != nil})
				return
			}
		}
		if len(links) >= 100 {
			writeError(w, 400, "a run can record at most 100 provenance links")
			return
		}
		source := "manual"
		if actorType == "agent" {
			source = "run_report"
		}
		links = append(links, IssueProvenanceLink{ID: id, Kind: req.Kind, RunID: req.RunID, Step: provenanceEntryStep(run.Context), URL: req.URL, CommitSHA: req.CommitSHA, ArtifactID: req.ArtifactID, SHA256: req.SHA256, Source: source, ActorType: actorType, ActorID: actorID, CreatedAt: time.Now().UTC()})
	}
	raw, _ := json.Marshal(links)
	err = q.SetIssueRunProvenanceLinks(r.Context(), db.SetIssueRunProvenanceLinksParams{RunID: run.ID, Links: raw})
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, 500, "could not save provenance")
		return
	}
	h.publishIssueRelationChanges(issue, issue)
	if nativePRChanged {
		h.publishIssuePullRequestsChanged(uuidToString(issue.WorkspaceID), issue.ID)
	}
	writeJSON(w, 200, map[string]any{"id": id, "created": !remove, "removed": remove})
}
