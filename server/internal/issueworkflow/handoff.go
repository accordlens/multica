package issueworkflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
)

const HandoffFence = "```json title=\"Multica handoff v1\"\n"

// HandoffEnvelope is data in an ordinary issue comment, not an instruction to
// advance the workflow. ID is the producer's stable event UUID. Resource IDs
// refer to existing rows; run_id is agent_task_queue.id.
type HandoffEnvelope struct {
	Version          int               `json:"version"`
	ID               string            `json:"id"`
	Type             string            `json:"type"`
	SenderID         string            `json:"sender_id"`
	IssueID          string            `json:"issue_id"`
	ThreadID         string            `json:"thread_id"`
	RunID            string            `json:"run_id"`
	WorkflowID       string            `json:"workflow_id"`
	WorkflowVersion  string            `json:"workflow_version"`
	WorkflowStep     string            `json:"workflow_step"`
	SourceRevision   string            `json:"source_revision"`
	RecipientID      string            `json:"recipient_id"`
	CausationID      string            `json:"causation_id,omitempty"`
	OccurredAt       time.Time         `json:"occurred_at"`
	Visibility       string            `json:"visibility"`
	Goal             string            `json:"goal"`
	Summary          string            `json:"summary"`
	Uncertainty      string            `json:"uncertainty"`
	ExpectedResponse string            `json:"expected_response"`
	Decision         string            `json:"decision,omitempty"`
	Artifacts        []HandoffArtifact `json:"artifacts"`
}

// Only source-owned attachment IDs travel in routing evidence. A digest is an
// author's content reference, not proof of a successful test or review.
type HandoffArtifact struct {
	AttachmentID string `json:"attachment_id"`
	SHA256       string `json:"sha256"`
}

func validHandoffUUID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id != uuid.Nil && id.String() == s
}

func IsSourceRevision(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && s == strings.ToLower(s)
}

func IsDefinitionVersion(s string) bool {
	return len(s) == 64 && IsSourceRevision(s)
}

func (e HandoffEnvelope) Validate() error {
	if e.Version != 1 || !validHandoffUUID(e.ID) || !validHandoffUUID(e.SenderID) ||
		!validHandoffUUID(e.IssueID) || !validHandoffUUID(e.ThreadID) || !validHandoffUUID(e.RunID) ||
		!validHandoffUUID(e.WorkflowID) || !validHandoffUUID(e.RecipientID) ||
		(e.CausationID != "" && !validHandoffUUID(e.CausationID)) {
		return errors.New("handoff requires version 1 and canonical UUIDs")
	}
	if e.SenderID == e.RecipientID || e.WorkflowStep == "" || !IsDefinitionVersion(e.WorkflowVersion) ||
		!IsSourceRevision(e.SourceRevision) || e.OccurredAt.IsZero() || e.Visibility != "issue" {
		return errors.New("handoff requires distinct roles, workflow/source revisions, time and issue visibility")
	}
	if e.Goal == "" || e.Summary == "" || e.Uncertainty == "" || len(e.Artifacts) == 0 || len(e.Artifacts) > 8 {
		return errors.New("handoff requires goal, summary, uncertainty and 1–8 artifact references")
	}
	if strings.Contains(e.Goal+e.Summary+e.Uncertainty+e.Decision+e.WorkflowStep, "mention://") {
		return errors.New("handoff prose cannot contain side-effecting mention links")
	}
	switch e.Type {
	case "review.request":
		if e.ExpectedResponse != "review.result" || e.Decision != "" {
			return errors.New("review.request expects review.result without a decision")
		}
	case "review.result":
		if e.CausationID == "" || e.ExpectedResponse != "none" ||
			(e.Decision != "approve" && e.Decision != "changes_requested" && e.Decision != "unknown") {
			return errors.New("review.result requires its request ID, decision and no automatic reply")
		}
	case "decision.changed":
		if e.ExpectedResponse != "none" {
			return errors.New("decision.changed expects no automatic reply")
		}
	default:
		return errors.New("unknown handoff type")
	}
	seen := map[string]bool{}
	for _, a := range e.Artifacts {
		if !validHandoffUUID(a.AttachmentID) || !IsDefinitionVersion(a.SHA256) || seen[a.AttachmentID] {
			return errors.New("handoff artifacts require unique attachment IDs and SHA-256 references")
		}
		seen[a.AttachmentID] = true
	}
	return nil
}

func EncodeHandoff(e HandoffEnvelope) (string, error) {
	if err := e.Validate(); err != nil {
		return "", err
	}
	raw, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return "", err
	}
	if len(raw) > 12000 {
		return "", errors.New("handoff exceeds 12KB")
	}
	return HandoffFence + string(raw) + "\n```", nil
}

func DecodeHandoff(content string) (HandoffEnvelope, error) {
	var e HandoffEnvelope
	if len(content) > 12100 {
		return e, errors.New("handoff exceeds 12KB")
	}
	content = strings.TrimSpace(strings.ReplaceAll(content, "\r\n", "\n"))
	if !strings.HasPrefix(content, HandoffFence) || !strings.HasSuffix(content, "\n```") {
		return e, errors.New("expected one complete Multica handoff v1 code block")
	}
	raw := strings.TrimSuffix(strings.TrimPrefix(content, HandoffFence), "\n```")
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		return e, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return e, errors.New("handoff contains trailing data")
	}
	return e, e.Validate()
}

// DefinitionVersion fingerprints the normalized stored steps, including their
// handlers, instructions and next/back edges. JSON formatting is immaterial.
func DefinitionVersion(raw []byte) (string, error) {
	steps, err := DecodeSteps(raw)
	if err != nil {
		return "", err
	}
	canonical, err := json.Marshal(steps)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(canonical)
	return hex.EncodeToString(hash[:]), nil
}
