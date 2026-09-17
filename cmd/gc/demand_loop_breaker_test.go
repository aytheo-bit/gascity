package main

import (
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// OPS-78 repro. Two properties, one defect.
//
// A bead is counted as demand, so the reconciler spawns a seat for it. The seat
// cannot claim it, so it stops. The demand persists, so another seat spawns.
// Measured on Node C 2026-09-17: 90 divergences in 40 minutes on four rows,
// ~16.8k tokens in 90 seconds on one lane, empty worktree, nothing produced.

// demandLoopNow is a fixed clock for the corpus below: rows are built relative
// to it so "deferred" means deferred at the instant both sides are asked.
var demandLoopNow = time.Date(2026, 9, 17, 20, 0, 0, 0, time.UTC)

// unclaimableDemandRow is one row plus the single verdict both sides must give.
type unclaimableDemandRow struct {
	name string
	bead beads.Bead
	// wantCounted is what the DEMAND side must say. The claim side is not a
	// second expectation: the property is that it says the same thing.
	wantCounted bool
}

func unclaimableDemandRows() []unclaimableDemandRow {
	routed := func(id string) beads.Bead {
		return beads.Bead{
			ID: id, Status: "open", Type: "task",
			Metadata: map[string]string{beadmeta.RoutedToMetadataKey: agreementTemplate},
		}
	}
	deferUntil := func(b beads.Bead, at time.Time) beads.Bead {
		b.DeferUntil = &at
		return b
	}
	return []unclaimableDemandRow{
		{
			name:        "plain routed task is demand",
			bead:        routed("d-1"),
			wantCounted: true,
		},
		{
			// The row the rollout log's operator reached for first. Deferring is
			// not a workaround: "blocked AND DEFERRED routed rows are still
			// counted as demand" is the defect's own wording, so the defer is
			// one of the two states that CAUSES the loop.
			name:        "routed task deferred into the future is not demand",
			bead:        deferUntil(routed("d-2"), demandLoopNow.Add(8*time.Hour)),
			wantCounted: false,
		},
		{
			// bd's own indefinite deferral, the other half of beads.IsDeferred.
			name: "routed task deferred indefinitely is not demand",
			bead: func() beads.Bead {
				b := routed("d-3")
				b.IndefinitelyDeferred = true
				return b
			}(),
			wantCounted: false,
		},
		{
			// A defer that has already elapsed is not a deferral at all.
			name:        "routed task whose defer_until has elapsed is demand",
			bead:        deferUntil(routed("d-4"), demandLoopNow.Add(-time.Minute)),
			wantCounted: true,
		},
	}
}

// TestDemandCountsExactlyWhatTheClaimSideFindsClaimable is the agreement
// property stated against the CLAIM side's own production classifier rather
// than a mirror of it.
//
// classifyDemandTrigger reports events.DemandClaimDivergence for exactly one
// shape: a row that is still open, still servable, still route-matching, and
// still CLAIMABLE — the row the controller counted and no seat could take. If
// the demand predicate and the claim predicate agreed, that classification
// would be unreachable for a counted row, because the controller would never
// have counted a row the claim side refuses.
//
// So the property is an equality, and it fails in EITHER direction: a row
// counted but not claimable is a seat minted to drain, and a row claimable but
// not counted is work no seat is ever minted for.
func TestDemandCountsExactlyWhatTheClaimSideFindsClaimable(t *testing.T) {
	cfg := agreementConfig()
	templates := map[string]struct{}{agreementTemplate: {}}
	opts := hookClaimOptions{
		Assignee:     "rig/worker-1",
		RouteTargets: hookClaimRouteTargets(agreementTemplate),
		Env:          []string{"GC_SPAWN_ORIGIN=demand", "GC_TEMPLATE=" + agreementTemplate},
	}

	for _, row := range unclaimableDemandRows() {
		t.Run(row.name, func(t *testing.T) {
			bead := postCanonicalizeBead(cfg, row.bead)

			_, counted := demandServableForTemplatesAt(cfg, bead, templates, demandLoopNow)
			if counted != row.wantCounted {
				t.Errorf("counted by demand = %v, want %v", counted, row.wantCounted)
			}

			// The claim side, driven through the real classifier. ConfirmBlocked
			// answers "not blocked" — the shape of a row with no unmet plain
			// `blocks` edge — so the only thing left to disagree about is what
			// this test is about.
			ops := demandDivergenceOpsForBead(bead, nil)
			ops.Now = func() time.Time { return demandLoopNow }
			_, classification := classifyDemandTrigger(bead.ID, "", opts, ops)
			claimable := classification == events.DemandClaimDivergence

			if counted != claimable {
				t.Fatalf("AGREEMENT VIOLATED: demand counts %s = %v but the claim side finds it claimable = %v (classification %q)"+
					"\n  a row counted and not claimable spawns a seat that drains and is counted again next tick (OPS-78)",
					bead.ID, counted, claimable, classification)
			}
		})
	}
}

// TestUnclaimableDemandStopsSpawningSeats is the loop itself, at the level the
// controller runs it: count demand, mint a seat for the counted row, the seat
// finds nothing and exits, the row is unchanged, tick again.
//
// Node C ran this ~59 times in 40 minutes on one row. The assertion is that the
// number of seats minted for one unchanged unclaimable row is BOUNDED, not that
// the row is diagnosed: emitting an event 90 times while changing no behaviour
// is telemetry, not a control.
func TestUnclaimableDemandStopsSpawningSeats(t *testing.T) {
	cfg := agreementConfig()
	templates := map[string]struct{}{agreementTemplate: {}}
	store := beads.NewMemStore()

	// A row that is counted as demand and that no seat will ever claim. The
	// cause is deliberately NOT one of the enumerated exclusions: the breaker
	// has to bound a loop whose cause nobody enumerated, which is the whole
	// reason the enumerated fix alone is not enough.
	created, err := store.Create(beads.Bead{
		Title: "unclaimable demand", Type: "task",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: agreementTemplate},
	})
	if err != nil {
		t.Fatalf("seeding row: %v", err)
	}

	const ticks = 40
	now := demandLoopNow
	seatsMinted := 0
	for i := 0; i < ticks; i++ {
		row, err := store.Get(created.ID)
		if err != nil {
			t.Fatalf("tick %d: reading row: %v", i, err)
		}
		if _, counted := demandServableForTemplatesAt(cfg, row, templates, now); !counted {
			continue
		}
		// The controller mints a seat for the counted row.
		seatsMinted++
		noteDemandSeatMinted(store, row, now, io.Discard)
		// The seat starts perfectly, its own query serves it nothing, and it
		// drains. Nothing about the row changes.
		now = now.Add(20 * time.Second)
	}

	if seatsMinted >= ticks {
		t.Fatalf("OPS-78: %d ticks minted %d seats for one unchanged unclaimable row — the loop is unbounded", ticks, seatsMinted)
	}
	if seatsMinted > demandLoopStrikeLimit {
		t.Fatalf("minted %d seats, want at most %d before the breaker trips", seatsMinted, demandLoopStrikeLimit)
	}

	// And it must say so loudly, on the row, where an operator can find it.
	final, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("reading final row: %v", err)
	}
	if !isDemandLoopQuarantined(final) {
		t.Fatalf("row %s stopped generating seats but carries no quarantine marker: an operator has no way to see it", final.ID)
	}
}
