package main

import (
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/pkg/accountusage"
)

func printRuntimeAccountUsage(w io.Writer, p accountusage.Projection) error {
	if p.Snapshot == nil || p.State == "unknown" || p.Snapshot.Validate(time.Now()) != nil {
		_, err := fmt.Fprintf(w, "Provider account: UNKNOWN (%s)\nCheck Claude /usage or Settings > Usage and record a manual snapshot.\n", p.Reason)
		return err
	}
	s := p.Snapshot
	// Use the same pure projection for elapsed freshness as the API.
	current := accountusage.Project([]accountusage.Record{{RuntimeID: "current", OwnerID: p.OwnerID, Snapshot: s}}, "current", time.Now())
	if _, err := fmt.Fprintf(w, "Provider account: %s / %s\nFreshness: %s (%s)\nAvailability (observed): %s\nObserved: %s\nSource: %s / %s / %s\n", s.Provider, s.AccountRef, current.State, current.Reason, s.Availability, s.ObservedAt.Format(time.RFC3339), s.Provenance.Kind, s.Provenance.Surface, s.Provenance.EvidenceRef); err != nil {
		return err
	}
	rows := make([][]string, 0, len(s.Limits))
	for _, l := range s.Limits {
		reset := "UNKNOWN"
		if l.ResetAt != nil && l.ResetTimezone != nil {
			loc, _ := time.LoadLocation(*l.ResetTimezone)
			reset = l.ResetAt.In(loc).Format(time.RFC3339) + " [" + *l.ResetTimezone + "]"
		}
		window := l.Window.Kind
		if l.Window.DurationSeconds != nil {
			window += " " + strconv.FormatInt(*l.Window.DurationSeconds, 10) + "s"
		}
		if l.Window.StartsAt != nil || l.Window.EndsAt != nil {
			start, end := "UNKNOWN", "UNKNOWN"
			if l.Window.StartsAt != nil {
				start = l.Window.StartsAt.UTC().Format(time.RFC3339)
			}
			if l.Window.EndsAt != nil {
				end = l.Window.EndsAt.UTC().Format(time.RFC3339)
			}
			window += " [" + start + ".." + end + "]"
		}
		rows = append(rows, []string{l.ID, percentageOrUnknown(l.Limit), percentageOrUnknown(l.Used), percentageOrUnknown(l.Remaining), window, reset})
	}
	cli.PrintTable(w, []string{"LIMIT", "TOTAL", "USED", "REMAINING", "WINDOW", "RESET"}, rows)
	_, err := fmt.Fprintln(w, "Display snapshot only. Verify fresh provider evidence before retrying the existing run; recording a snapshot never resumes it.")
	return err
}

func percentageOrUnknown(value *float64) string {
	if value == nil {
		return "UNKNOWN"
	}
	return strconv.FormatFloat(*value, 'f', -1, 64) + "%"
}
