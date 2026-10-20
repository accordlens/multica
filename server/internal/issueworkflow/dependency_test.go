package issueworkflow

import (
	"reflect"
	"testing"
)

func TestDependencyCycle(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edges []DependencyEdge
		add   DependencyEdge
		want  []string
	}{
		{"self", nil, DependencyEdge{"A", "A"}, []string{"A", "A"}},
		{"two", []DependencyEdge{{"A", "B"}}, DependencyEdge{"B", "A"}, []string{"B", "A", "B"}},
		{"chain", []DependencyEdge{{"C", "B"}, {"B", "A"}}, DependencyEdge{"A", "C"}, []string{"A", "C", "B", "A"}},
		{"diamond", []DependencyEdge{{"C", "A"}}, DependencyEdge{"C", "B"}, nil},
		{"legacy cycle terminates", []DependencyEdge{{"A", "B"}, {"B", "A"}}, DependencyEdge{"C", "A"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DependencyCycle(tc.edges, tc.add); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("cycle = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDependencyCondition(t *testing.T) {
	for _, kind := range []string{"done", "in_review", "accepted"} {
		if err := (DependencyCondition{Kind: kind}).Validate(); err != nil {
			t.Fatal(err)
		}
		if err := (DependencyCondition{Kind: kind, RunID: "00000000-0000-0000-0000-000000000001"}).Validate(); err == nil {
			t.Fatal("accepted ambiguous condition")
		}
	}
	if err := (DependencyCondition{Kind: "run_completed", RunID: "bad"}).Validate(); err == nil {
		t.Fatal("accepted invalid run")
	}
	if err := (DependencyCondition{Kind: "passed"}).Validate(); err == nil {
		t.Fatal("agent completion is not a test gate")
	}
}
