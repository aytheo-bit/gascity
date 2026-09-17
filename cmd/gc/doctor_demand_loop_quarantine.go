package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// demandLoopQuarantineCheck names the rows the demand/claim loop breaker has
// stopped counting as demand.
//
// This check IS the third ask of OPS-78. On the night of 2026-09-17 the
// condition was fully visible in `.gc/events.jsonl` — ninety
// session.demand_claim_divergence records in forty minutes, naming the four
// rows — and it still took ninety minutes and a manual read of a JSONL file to
// find. A condition that is only legible to someone who already suspects it and
// knows which file to grep is not an operator surface.
//
// Unlike the route-recovery quarantine it is modelled on, this one reports a
// row whose demand is actually being SUPPRESSED, so the finding carries a
// consequence: until the row is repaired or the retry window lets a probe
// through, no seat is being minted for it. That is the intended behaviour and
// the reason the check exists to say so out loud.
type demandLoopQuarantineCheck struct {
	cfg      *config.City
	cityPath string
	newStore func(string) (beads.Store, error)
}

func newDemandLoopQuarantineCheck(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error)) *demandLoopQuarantineCheck {
	return &demandLoopQuarantineCheck{cfg: cfg, cityPath: cityPath, newStore: newStore}
}

func (c *demandLoopQuarantineCheck) Name() string { return "demand-loop-quarantine" }

// CanFix returns true: --fix clears the ledger and re-arms the row. It is the
// explicit "I have looked at this and it should be tried again" gesture, for
// the case where the cause cleared somewhere the fingerprint cannot see.
func (c *demandLoopQuarantineCheck) CanFix() bool { return true }

func (c *demandLoopQuarantineCheck) WarmupEligible() bool { return false }

// quarantinedDemandRow is one parked row and where it lives.
type quarantinedDemandRow struct {
	label   string
	store   beads.Store
	beadID  string
	title   string
	reason  string
	strikes int
	route   string
}

func (q quarantinedDemandRow) describe() string {
	title := q.title
	if strings.TrimSpace(title) == "" {
		title = "(untitled)"
	}
	route := q.route
	if strings.TrimSpace(route) == "" {
		route = "(no route)"
	}
	return fmt.Sprintf("%s bead %s %q routed to %s: %d seats minted, none claimed (%s) — not counted as demand",
		q.label, q.beadID, title, route, q.strikes, q.reason)
}

func (c *demandLoopQuarantineCheck) collect() (found []quarantinedDemandRow, skipped []string) {
	scopes := []struct{ label, path string }{{"city", c.cityPath}}
	if c.cfg != nil {
		for _, rig := range c.cfg.Rigs {
			if rig.Suspended || strings.TrimSpace(rig.Path) == "" {
				continue
			}
			scopes = append(scopes, struct{ label, path string }{"rig " + rig.Name, rig.Path})
		}
	}
	for _, sc := range scopes {
		if c.newStore == nil || strings.TrimSpace(sc.path) == "" {
			continue
		}
		store, err := c.newStore(sc.path)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: opening bead store: %v", sc.label, err))
			continue
		}
		// A targeted metadata query, not a scan: the marker is the index. Open
		// rows only — a closed row's marker is history, and this check reports
		// what an operator can still act on.
		items, err := store.List(beads.ListQuery{
			Metadata: map[string]string{beadmeta.DemandLoopQuarantinedMetadataKey: "true"},
		})
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: listing beads: %v", sc.label, err))
			continue
		}
		for _, b := range items {
			if !isDemandLoopQuarantined(b) {
				continue
			}
			// A marker is not a finding on its own. Two kinds of row still carry
			// one and are nothing for an operator to do:
			//
			//   - a CLOSED row. Whatever it was doing, it is over. The marker is
			//     history, and reporting history as a live finding is how a check
			//     teaches people to skip it.
			//   - a row whose ledger no longer matches its own fingerprint. The
			//     row was edited after it was parked, so it is already being
			//     counted again; the marker is stale and the next seat mint
			//     clears it.
			//
			// What IS still reported is a parked row inside its retry gap. The
			// suppression is a moment and the finding is a state: an operator
			// looking at a row that mints one probe seat every ten minutes needs
			// to see it, not to catch it on the right tick.
			if !strings.EqualFold(strings.TrimSpace(b.Status), "open") {
				continue
			}
			ledger, ledgerOK := parseDemandLoopLedger(b.Metadata[beadmeta.DemandLoopStrikesMetadataKey])
			if !ledgerOK || ledger.Fingerprint != demandLoopFingerprint(b) {
				continue
			}
			reason := strings.TrimSpace(b.Metadata[beadmeta.DemandLoopQuarantineReasonMetadataKey])
			if reason == "" {
				reason = "unknown"
			}
			found = append(found, quarantinedDemandRow{
				label:   sc.label,
				store:   store,
				beadID:  b.ID,
				title:   b.Title,
				reason:  reason,
				strikes: ledger.Strikes,
				route:   strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey]),
			})
		}
	}
	return found, skipped
}

func (c *demandLoopQuarantineCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	found, skipped := c.collect()
	if len(found) == 0 && len(skipped) == 0 {
		return okCheck(c.Name(), "no row is generating unclaimable demand")
	}
	details := make([]string, 0, len(found)+len(skipped))
	for _, q := range found {
		details = append(details, q.describe())
	}
	details = append(details, skipped...)
	sort.Strings(details)
	if len(found) == 0 {
		return warnCheck(c.Name(),
			fmt.Sprintf("demand-loop quarantine review skipped %d scope(s)", len(skipped)),
			"fix bead store access, then rerun gc doctor",
			details)
	}
	msg := fmt.Sprintf("%d row(s) generating unclaimable demand, no longer counted", len(found))
	if len(skipped) > 0 {
		msg = fmt.Sprintf("%s; %d scope(s) skipped", msg, len(skipped))
	}
	return warnCheck(c.Name(), msg,
		"find why a worker's own query will not serve each row (route, hold label, type, blocking dep), fix it, then run gc doctor --fix to re-arm",
		details)
}

func (c *demandLoopQuarantineCheck) Fix(_ *doctor.CheckContext) error {
	found, skipped := c.collect()
	for _, q := range found {
		if err := q.store.SetMetadataBatch(q.beadID, clearDemandLoopLedger()); err != nil {
			return fmt.Errorf("%s bead %s: clearing demand-loop quarantine: %w", q.label, q.beadID, err)
		}
	}
	if len(skipped) > 0 {
		return fmt.Errorf("demand-loop-quarantine skipped %d scope(s): %s", len(skipped), strings.Join(skipped, "; "))
	}
	return nil
}
