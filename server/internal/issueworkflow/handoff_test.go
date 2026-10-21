package issueworkflow

import (
	"strings"
	"testing"
	"time"
)

func sampleHandoff() HandoffEnvelope {
	return HandoffEnvelope{Version: 1, ID: "11111111-1111-4111-8111-111111111111", Type: "review.request",
		SenderID: "22222222-2222-4222-8222-222222222222", RecipientID: "33333333-3333-4333-8333-333333333333",
		IssueID: "44444444-4444-4444-8444-444444444444", ThreadID: "55555555-5555-4555-8555-555555555555",
		RunID: "66666666-6666-4666-8666-666666666666", WorkflowID: "77777777-7777-4777-8777-777777777777",
		WorkflowStep: "in_progress", WorkflowVersion: strings.Repeat("a", 64), SourceRevision: strings.Repeat("b", 40),
		OccurredAt: time.Date(2026, 10, 20, 15, 0, 0, 0, time.UTC), Visibility: "issue",
		Goal: "Review the pinned revision.", Summary: "The report is attached.", Uncertainty: "No runtime evaluation.",
		ExpectedResponse: "review.result", Artifacts: []HandoffArtifact{{AttachmentID: "88888888-8888-4888-8888-888888888888", SHA256: strings.Repeat("c", 64)}}}
}

func TestHandoffEnvelopeRejectsAmbiguousOrIncompleteMessages(t *testing.T) {
	for name, change := range map[string]func(*HandoffEnvelope){
		"new version":            func(e *HandoffEnvelope) { e.Version = 2 },
		"missing artifact":       func(e *HandoffEnvelope) { e.Artifacts = nil },
		"short source revision":  func(e *HandoffEnvelope) { e.SourceRevision = "abc123" },
		"self addressed":         func(e *HandoffEnvelope) { e.RecipientID = e.SenderID },
		"private scope":          func(e *HandoffEnvelope) { e.Visibility = "private" },
		"nil UUID":               func(e *HandoffEnvelope) { e.RunID = "00000000-0000-0000-0000-000000000000" },
		"unbounded reply":        func(e *HandoffEnvelope) { e.ExpectedResponse = "reply" },
		"side-effecting mention": func(e *HandoffEnvelope) { e.Summary = "[@QA](mention://agent/33333333-3333-4333-8333-333333333333)" },
		"result without cause": func(e *HandoffEnvelope) {
			e.Type = "review.result"
			e.ExpectedResponse = "none"
			e.Decision = "approve"
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := sampleHandoff()
			change(&e)
			if _, err := EncodeHandoff(e); err == nil {
				t.Fatal("invalid envelope accepted")
			}
		})
	}
	e := sampleHandoff()
	content, err := EncodeHandoff(e)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeHandoff(content)
	if err != nil || got.SourceRevision != e.SourceRevision || got.Goal != e.Goal {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	for _, fileContent := range []string{content + "\n", "\n" + strings.ReplaceAll(content, "\n", "\r\n") + "\r\n"} {
		if decoded, err := DecodeHandoff(fileContent); err != nil || decoded.ID != e.ID {
			t.Fatalf("file line endings changed envelope: %+v %v", decoded, err)
		}
	}
	for _, malformed := range []string{
		content + "\nMore instructions", "Prose\n" + content,
		strings.Replace(content, `"version": 1`, `"version": 1, "route_any_agent": true`, 1),
		strings.Replace(content, "\n```", " {}\n```", 1),
	} {
		if _, err := DecodeHandoff(malformed); err == nil {
			t.Fatal("ambiguous content accepted")
		}
	}
}

func TestDefinitionVersionTracksMeaningfulDefinitionChanges(t *testing.T) {
	a, err := DefinitionVersion([]byte(`[{"status_key":"todo"},{"status_key":"in_review","handler":{"type":"agent","id":"a"},"instructions":"Review."}]`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := DefinitionVersion([]byte(`[ { "status_key": "todo", "handler":{"type":"none"} }, {"instructions":"Review.","handler":{"id":"a","type":"agent"},"status_key":"in_review"} ]`))
	c, _ := DefinitionVersion([]byte(`[{"status_key":"todo"},{"status_key":"in_review","handler":{"type":"agent","id":"b"},"instructions":"Review."}]`))
	if a != b || a == c {
		t.Fatalf("definition versions: %s %s %s", a, b, c)
	}
}
