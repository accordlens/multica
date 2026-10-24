package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

// These pointers describe sources, never test or acceptance authority.
type IssueProvenanceLink struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	RunID      string     `json:"run_id"`
	Step       string     `json:"step"`
	URL        string     `json:"url,omitempty"`
	CommitSHA  string     `json:"commit_sha,omitempty"`
	DirtyHash  string     `json:"dirty_hash,omitempty"`
	ArtifactID string     `json:"artifact_id,omitempty"`
	SHA256     string     `json:"sha256,omitempty"`
	Source     string     `json:"source"`
	ActorType  string     `json:"actor_type"`
	ActorID    string     `json:"actor_id"`
	CreatedAt  time.Time  `json:"created_at"`
	RemovedAt  *time.Time `json:"removed_at,omitempty"`
	RemovedBy  string     `json:"removed_by,omitempty"`
}

var provenanceSHA = regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`)

func decodeRunLinks(raw []byte) ([]IssueProvenanceLink, bool) {
	var context struct {
		Links []IssueProvenanceLink `json:"provenance_links"`
	}
	if len(raw) > 0 && json.Unmarshal(raw, &context) != nil {
		return nil, false
	}
	if context.Links == nil {
		context.Links = []IssueProvenanceLink{}
	}
	return context.Links, true
}

func provenanceLinkID(kind, url, commit, artifact, hash, dirty string) string {
	value := kind + "\n" + url + "\n" + commit + "\n" + artifact + "\n" + hash
	if dirty != "" {
		value += "\n" + dirty
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// ACCO-94's native evidence is already a durable, run-bound source. Outcome is
// deliberately irrelevant here: a source pointer never establishes PASS.
func nativeEvidenceCommit(runID, step string, raw []byte) (IssueProvenanceLink, bool) {
	var envelope struct {
		Evidence struct {
			RunID  string `json:"run_id"`
			Source struct {
				SHA       string `json:"sha"`
				DirtyHash string `json:"dirty_hash"`
			} `json:"source"`
			FinishedAt time.Time `json:"finished_at"`
		} `json:"workflow_evidence"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return IssueProvenanceLink{}, false
	}
	e := envelope.Evidence
	sha := strings.ToLower(e.Source.SHA)
	if e.RunID != runID || !provenanceSHA.MatchString(sha) || e.FinishedAt.IsZero() || len(e.Source.DirtyHash) > 256 {
		return IssueProvenanceLink{}, false
	}
	return IssueProvenanceLink{ID: provenanceLinkID("commit", "", sha, "", "", e.Source.DirtyHash), Kind: "commit", RunID: runID, Step: step, CommitSHA: sha, DirtyHash: e.Source.DirtyHash, Source: "workflow_evidence", ActorType: "system", CreatedAt: e.FinishedAt}, true
}

// Retained tombstones take precedence over derived evidence on every replay.
func withNativeEvidenceCommit(stored []IssueProvenanceLink, runID, step string, result []byte) []IssueProvenanceLink {
	derived, ok := nativeEvidenceCommit(runID, step, result)
	if !ok {
		return stored
	}
	for _, link := range stored {
		if link.ID == derived.ID {
			return stored
		}
	}
	return append(stored, derived)
}
