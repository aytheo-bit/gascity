package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// parkedDemandBead is a row the breaker has quarantined, with a ledger that
// matches its own fingerprint — the shape noteDemandSeatMinted leaves behind.
func parkedDemandBead(id string, strikes int) beads.Bead {
	b := beads.Bead{ID: id, Title: "unclaimable work", Type: "task", Status: "open", Metadata: map[string]string{
		beadmeta.RoutedToMetadataKey:                   agreementTemplate,
		beadmeta.DemandLoopQuarantinedMetadataKey:      "true",
		beadmeta.DemandLoopQuarantineReasonMetadataKey: demandLoopQuarantineReason,
	}}
	b.Metadata[beadmeta.DemandLoopStrikesMetadataKey] = demandLoopLedger{
		Strikes:     strikes,
		LastMint:    demandLoopNow,
		Fingerprint: demandLoopFingerprint(b),
	}.encode()
	return b
}

func demandLoopCheckFor(t *testing.T, rows ...beads.Bead) *demandLoopQuarantineCheck {
	t.Helper()
	store := beads.NewMemStoreFrom(0, rows, nil)
	return newDemandLoopQuarantineCheck(&config.City{}, t.TempDir(), func(string) (beads.Store, error) { return store, nil })
}

// TestDemandLoopQuarantineCheckNamesTheOffendingRow is the third ask of OPS-78.
// On the night of 2026-09-17 the condition was fully present in
// .gc/events.jsonl and still took ninety minutes and a manual grep to find, so
// "an operator can see it" means the row is NAMED, with enough on the line to
// act on without opening anything else.
func TestDemandLoopQuarantineCheckNamesTheOffendingRow(t *testing.T) {
	healthy := beads.Bead{ID: "T-fine", Title: "ordinary work", Type: "task", Status: "open",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: agreementTemplate}}
	check := demandLoopCheckFor(t, parkedDemandBead("T-loop", demandLoopStrikeLimit), healthy)

	result := check.Run(&doctor.CheckContext{})
	if result.Status == doctor.StatusOK {
		t.Fatalf("check reported OK with a parked row present: %+v", result)
	}
	joined := strings.Join(result.Details, "\n")
	for _, want := range []string{"T-loop", "unclaimable work", agreementTemplate, demandLoopQuarantineReason} {
		if !strings.Contains(joined, want) {
			t.Errorf("details do not mention %q:\n%s", want, joined)
		}
	}
	// Control: an ordinary routed row is not a finding. Without this the check
	// could warn about everything and still "name" the parked row.
	if strings.Contains(joined, "T-fine") {
		t.Errorf("details include an unparked row:\n%s", joined)
	}
}

// TestDemandLoopQuarantineCheckIgnoresStaleMarkers is the review's finding 5.
// A check that reports rows which are being counted perfectly well teaches an
// operator to stop reading it — and the two stale shapes both arise in normal
// operation, so this is not a corner case.
func TestDemandLoopQuarantineCheckIgnoresStaleMarkers(t *testing.T) {
	// Edited after it was parked: the ledger no longer matches the fingerprint,
	// so the row is already being counted again.
	edited := parkedDemandBead("T-edited", demandLoopStrikeLimit)
	edited.Metadata[beadmeta.RoutedToMetadataKey] = "rig/somewhere-else"

	// Closed: whatever it was doing is over, and history is not a finding.
	closed := parkedDemandBead("T-closed", demandLoopStrikeLimit)
	closed.Status = "closed"

	// Marker present but the ledger is unreadable — nothing to report a strike
	// count from, and a marker alone is not evidence.
	garbled := parkedDemandBead("T-garbled", demandLoopStrikeLimit)
	garbled.Metadata[beadmeta.DemandLoopStrikesMetadataKey] = "not-a-ledger"

	check := demandLoopCheckFor(t, edited, closed, garbled)
	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("stale markers produced a finding: %+v\n%s", result, strings.Join(result.Details, "\n"))
	}
}

// TestDemandLoopQuarantineCheckReportsARowInsideItsRetryGap: suppression is a
// moment and the finding is a state. A row that mints one probe seat every ten
// minutes must not be visible only to an operator who runs the check on the
// right tick.
func TestDemandLoopQuarantineCheckReportsARowInsideItsRetryGap(t *testing.T) {
	row := parkedDemandBead("T-probing", demandLoopStrikeLimit)
	past := demandLoopNow.Add(-2 * demandLoopQuarantineRetry)
	row.Metadata[beadmeta.DemandLoopStrikesMetadataKey] = demandLoopLedger{
		Strikes: demandLoopStrikeLimit, LastMint: past, Fingerprint: demandLoopFingerprint(row),
	}.encode()
	if demandLoopQuarantineActive(row, demandLoopNow) {
		t.Fatal("fixture is wrong: the row should be inside its retry gap, not suppressed")
	}

	result := demandLoopCheckFor(t, row).Run(&doctor.CheckContext{})
	if result.Status == doctor.StatusOK {
		t.Fatalf("a row in its retry gap is invisible to the operator: %+v", result)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), "T-probing") {
		t.Error("details do not name the row")
	}
}

// TestDemandLoopQuarantineCheckFixReArmsTheRow pins the un-park path. A
// quarantine an operator cannot lift after fixing the real cause is a silent
// terminal drop for work somebody is waiting on.
func TestDemandLoopQuarantineCheckFixReArmsTheRow(t *testing.T) {
	cfg := agreementConfig()
	templates := map[string]struct{}{agreementTemplate: {}}
	row := parkedDemandBead("T-loop", demandLoopStrikeLimit)
	store := beads.NewMemStoreFrom(0, []beads.Bead{row}, nil)
	check := newDemandLoopQuarantineCheck(&config.City{}, t.TempDir(), func(string) (beads.Store, error) { return store, nil })

	if _, counted := demandServableForTemplatesAt(cfg, row, templates, demandLoopNow); counted {
		t.Fatal("fixture is wrong: the parked row should not be counted before --fix")
	}
	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix: %v", err)
	}

	fixed, err := store.Get("T-loop")
	if err != nil {
		t.Fatalf("re-reading: %v", err)
	}
	if isDemandLoopQuarantined(fixed) {
		t.Error("the marker survived --fix")
	}
	if raw := strings.TrimSpace(fixed.Metadata[beadmeta.DemandLoopStrikesMetadataKey]); raw != "" {
		t.Errorf("the ledger survived --fix (%q), so the row would re-park after one mint", raw)
	}
	if _, counted := demandServableForTemplatesAt(cfg, fixed, templates, demandLoopNow); !counted {
		t.Error("--fix cleared the marker but the row is still not counted as demand")
	}
	if result := check.Run(&doctor.CheckContext{}); result.Status != doctor.StatusOK {
		t.Errorf("check still reports a finding after --fix: %+v", result)
	}
}

// TestDemandLoopQuarantineCheckReportsAnUnreadableScope: a store it cannot read
// is not evidence of a healthy city, and reporting OK there would hide exactly
// the condition the check exists for.
func TestDemandLoopQuarantineCheckReportsAnUnreadableScope(t *testing.T) {
	check := newDemandLoopQuarantineCheck(&config.City{}, t.TempDir(), func(string) (beads.Store, error) {
		return nil, errUnreadableDemandLoopScope
	})
	result := check.Run(&doctor.CheckContext{})
	if result.Status == doctor.StatusOK {
		t.Fatalf("an unreadable store reported OK: %+v", result)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), "skipped") {
		t.Error("details do not say the scope was skipped")
	}
}

var errUnreadableDemandLoopScope = &demandLoopTestError{"store is down"}

type demandLoopTestError struct{ msg string }

func (e *demandLoopTestError) Error() string { return e.msg }
