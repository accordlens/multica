// Package accountusage projects sourced account limits independently of run costs.
// Snapshots are display evidence; they never authorize a retry or enforce a quota.
package accountusage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"regexp"
	"sort"
	"time"
	_ "time/tzdata"
)

const MetadataKey = "provider_account_usage_v1"
const MaxSnapshotBytes = 32768

var referencePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:-]{0,99}$`)

type Provenance struct {
	Kind        string     `json:"kind"`
	Surface     string     `json:"surface"`
	EvidenceRef string     `json:"evidence_ref"`
	RecordedBy  string     `json:"recorded_by,omitempty"`
	RecordedAt  *time.Time `json:"recorded_at,omitempty"`
}

type Window struct {
	Kind            string     `json:"kind"`
	DurationSeconds *int64     `json:"duration_seconds"`
	StartsAt        *time.Time `json:"starts_at"`
	EndsAt          *time.Time `json:"ends_at"`
}

type Limit struct {
	ID            string     `json:"id"`
	Kind          string     `json:"kind"`
	Unit          string     `json:"unit"`
	Limit         *float64   `json:"limit"`
	Used          *float64   `json:"used"`
	Remaining     *float64   `json:"remaining"`
	Window        Window     `json:"window"`
	ResetAt       *time.Time `json:"reset_at"`
	ResetTimezone *string    `json:"reset_timezone"`
}

type Snapshot struct {
	Version           int        `json:"version"`
	ObservationID     string     `json:"observation_id"`
	Provider          string     `json:"provider"`
	AccountRef        string     `json:"account_ref"`
	Scope             string     `json:"scope"`
	Availability      string     `json:"availability"`
	Limits            []Limit    `json:"limits"`
	Provenance        Provenance `json:"provenance"`
	ObservedAt        time.Time  `json:"observed_at"`
	StaleAfterSeconds int64      `json:"stale_after_seconds"`
}

// Record carries the existing runtime binding. Account references are scoped to
// their owner, so an unrelated owner's alias cannot merge or replace a pool.
type Record struct {
	RuntimeID string
	OwnerID   string
	Snapshot  *Snapshot
}

type Projection struct {
	State      string    `json:"state"`
	Reason     string    `json:"reason"`
	AsOf       time.Time `json:"as_of"`
	OwnerID    string    `json:"owner_id"`
	RuntimeIDs []string  `json:"runtime_ids"`
	Snapshot   *Snapshot `json:"snapshot"`
}

func Decode(data []byte) (Snapshot, error) {
	var s Snapshot
	if len(data) > MaxSnapshotBytes {
		return s, errors.New("account snapshot is too large")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return s, errors.New("invalid account snapshot JSON")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return s, errors.New("expected one account snapshot")
	}
	return s, nil
}

func (s Snapshot) Validate(now time.Time) error {
	if s.Version != 1 || s.Provider != "anthropic" || s.Scope != "subscription_account" {
		return errors.New("only version 1 Anthropic subscription snapshots are supported")
	}
	if !referencePattern.MatchString(s.AccountRef) || !referencePattern.MatchString(s.ObservationID) {
		return errors.New("account_ref and observation_id must be opaque references of at most 100 characters")
	}
	if s.Availability != "unknown" && s.Availability != "available" && s.Availability != "rate_limited" {
		return errors.New("invalid account availability")
	}
	if s.Provenance.Kind != "manual" || (s.Provenance.Surface != "claude_code_usage" && s.Provenance.Surface != "claude_settings_usage") || !referencePattern.MatchString(s.Provenance.EvidenceRef) {
		return errors.New("a manual Claude usage source and opaque evidence_ref are required")
	}
	if s.ObservedAt.IsZero() || s.ObservedAt.After(now) || s.StaleAfterSeconds < 1 || s.StaleAfterSeconds > 86400 {
		return errors.New("observed_at must not be in the future; stale_after_seconds must be 1..86400")
	}
	if len(s.Limits) == 0 || len(s.Limits) > 8 {
		return errors.New("one to eight sourced limits are required; use null for unknown values")
	}
	ids := map[string]bool{}
	for _, l := range s.Limits {
		if !referencePattern.MatchString(l.ID) || ids[l.ID] {
			return errors.New("limit ids must be distinct opaque references")
		}
		ids[l.ID] = true
		if l.Kind != "usage" || l.Unit != "percent" {
			return errors.New("subscription limits currently support usage percentages only")
		}
		for _, value := range []*float64{l.Limit, l.Used, l.Remaining} {
			if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 || *value > 100) {
				return errors.New("percentages must be null or between zero and 100")
			}
		}
		if l.Window.Kind != "unknown" && l.Window.Kind != "rolling" && l.Window.Kind != "fixed" {
			return errors.New("invalid limit window")
		}
		if l.Window.DurationSeconds != nil && *l.Window.DurationSeconds <= 0 {
			return errors.New("window duration must be positive")
		}
		if l.Window.StartsAt != nil && l.Window.EndsAt != nil && !l.Window.EndsAt.After(*l.Window.StartsAt) {
			return errors.New("window end must follow its start")
		}
		if (l.ResetAt == nil) != (l.ResetTimezone == nil) {
			return errors.New("reset_at and reset_timezone must both be known or both be null")
		}
		if l.ResetTimezone != nil {
			if *l.ResetTimezone == "" || *l.ResetTimezone == "Local" {
				return errors.New("reset_timezone must be an explicit IANA timezone")
			}
			if _, err := time.LoadLocation(*l.ResetTimezone); err != nil {
				return fmt.Errorf("invalid reset timezone")
			}
		}
	}
	return nil
}

// Replacement is idempotent for an unchanged observation. A reused operation ID
// with different evidence, or an older observation, must not overwrite a snapshot.
func Replacement(current, incoming Snapshot) (bool, error) {
	a, b := current, incoming
	a.Provenance.RecordedBy, b.Provenance.RecordedBy = "", ""
	a.Provenance.RecordedAt, b.Provenance.RecordedAt = nil, nil
	if current.ObservationID == incoming.ObservationID {
		if reflect.DeepEqual(a, b) {
			return false, nil
		}
		return false, errors.New("observation_id already records different evidence")
	}
	if !incoming.ObservedAt.After(current.ObservedAt) {
		return false, errors.New("a newer observed_at is required")
	}
	return true, nil
}

// Project selects one sourced account pool without adding any per-agent values.
// Passing the reset instant only makes evidence stale: it never refills a quota.
func Project(records []Record, runtimeID string, now time.Time) Projection {
	out := Projection{State: "unknown", Reason: "snapshot_missing", AsOf: now.UTC(), RuntimeIDs: []string{}}
	var target *Record
	for i := range records {
		if records[i].RuntimeID == runtimeID {
			target = &records[i]
			break
		}
	}
	if target == nil || target.Snapshot == nil {
		return out
	}
	if target.OwnerID == "" || target.Snapshot.Validate(now) != nil {
		out.Reason = "snapshot_invalid"
		return out
	}
	var latest *Snapshot
	conflict := false
	seen := map[string]bool{}
	for _, r := range records {
		s := r.Snapshot
		if s == nil || r.OwnerID != target.OwnerID || s.Provider != target.Snapshot.Provider || s.AccountRef != target.Snapshot.AccountRef || s.Validate(now) != nil {
			continue
		}
		if !seen[r.RuntimeID] {
			out.RuntimeIDs = append(out.RuntimeIDs, r.RuntimeID)
			seen[r.RuntimeID] = true
		}
		if latest == nil || s.ObservedAt.After(latest.ObservedAt) {
			latest, conflict = s, false
		} else if s.ObservedAt.Equal(latest.ObservedAt) {
			if changed, err := Replacement(*latest, *s); changed || err != nil {
				conflict = true
			}
		}
	}
	sort.Strings(out.RuntimeIDs)
	out.OwnerID = target.OwnerID
	if conflict {
		out.State, out.Reason = "unknown", "snapshot_conflict"
		return out
	}
	out.Snapshot = latest
	out.State, out.Reason = "fresh", "manual_snapshot"
	if !now.Before(latest.ObservedAt.Add(time.Duration(latest.StaleAfterSeconds) * time.Second)) {
		out.State, out.Reason = "stale", "observation_expired"
	}
	for _, l := range latest.Limits {
		if l.Window.EndsAt != nil && !now.Before(*l.Window.EndsAt) {
			out.State, out.Reason = "stale", "window_requires_observation"
		}
	}
	for _, l := range latest.Limits {
		if l.ResetAt != nil && !now.Before(*l.ResetAt) {
			out.State, out.Reason = "stale", "reset_requires_observation"
		}
	}
	return out
}
