package main

// The demand/claim loop breaker (OPS-78).
//
// A bead is counted as demand, so the reconciler mints a seat for it. The seat
// starts perfectly, its own query serves it nothing, and it stops. The demand
// persists, so another seat is minted. Indefinitely.
//
// Measured on Node C over one 40-minute window on 2026-09-17: 59 session.woke,
// 55 session.stopped, 90 session.demand_claim_divergence, and no work. One lane
// burned 16.8k tokens in 90 seconds against an empty worktree, because
// wake_mode="fresh" re-pays full context cost every cycle.
//
// Nothing already in the system catches this, and that is the point:
//
//   - The crash-loop restart ledger (crash_tracker.go) counts failed STARTS.
//     Every seat here starts perfectly. It never fires.
//   - The capacity queue bounds CONCURRENT sessions. These sessions succeed and
//     exit immediately, so concurrency is never the binding constraint.
//   - demand_divergence.go sees the condition exactly and emits an event for it
//     — 90 times in 40 minutes, changing nothing. That is telemetry, not a
//     control.
//
// Two things close it, and both are needed.
//
// FIRST, the enumerated half: a row the claim side refuses must not be counted
// as demand. demand_serve_predicate.go owns that, and this file is not it.
//
// SECOND, this file: the residual. The enumerated half can only exclude causes
// somebody enumerated, and the loop's cost does not depend on its cause — it is
// the same seat, the same fresh context, the same drained quota whether the row
// is unclaimable for a reason in that list or a reason nobody has thought of
// yet. So the breaker keys on the OUTCOME instead: this row keeps being counted,
// seats keep being minted for it, and it never gets claimed. After
// demandLoopStrikeLimit of those, stop counting it.
//
// Why the ledger lives on the bead rather than in controller memory, unlike
// crashTracker: the two halves of the loop are in DIFFERENT PROCESSES. The
// controller counts the row and mints the seat; the seat's own `gc hook --claim`
// is where the failure to claim is observable. A sync.Mutex map in the
// controller cannot see the second half, and the store is the only thing both
// processes read. It is also what gives `gc doctor` the row for free — the same
// arrangement the route-recovery lane already uses (route_recovery_lane.go,
// doctor_route_recovery_quarantine.go), and the reason its marker is a targeted
// metadata query rather than a scan: the marker is the index.
//
// Where this DELIBERATELY DIVERGES from route recovery: there, "quarantine is a
// LABEL, never a skip" — the lane keeps re-evaluating and the marker only tells
// an operator. Here the skip IS the fix. A marker that did not stop the counting
// would be the ninetieth event in forty minutes again.

import (
	"fmt"
	"hash/fnv"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

const (
	// demandLoopStrikeLimit is how many seats may be minted for the SAME
	// unchanged row before it stops being counted as demand.
	//
	// Five, not one: a seat minted for a row that a sibling seat claims first is
	// correct pull, and the controller's demand read is allowed to be up to one
	// tick stale relative to the worker's (controllerDemandReady says so in as
	// many words). A couple of consecutive mints against one row is ordinary
	// race, not a loop. Five consecutive mints with the row untouched throughout
	// is not a race any more — at Node C's observed ~20s cadence it is reached in
	// under two minutes, against the ninety minutes the unbounded loop ran for.
	demandLoopStrikeLimit = 5

	// demandLoopStrikeWindow is how long a strike counts for. A row that mints a
	// seat once an hour for five hours is not the loop this breaker exists for,
	// and should not be quarantined as though it were.
	demandLoopStrikeWindow = 15 * time.Minute

	// demandLoopQuarantineRetry is how long a quarantined row stays uncounted
	// before it is allowed exactly ONE probe seat again.
	//
	// This is the deliberate softening of "stop spawning for that row", and the
	// judgement is worth stating. A hard permanent stop would bury work whose
	// cause cleared OUTSIDE the row — the blocker closed, a hold was lifted
	// elsewhere, a rig came back, a drained provider refilled — because none of
	// those edit the row itself, so none of them change its fingerprint. The
	// whole process failure underneath OPS-78 is that a known condition sat
	// waiting for someone to notice it, so a breaker whose only escape is "an
	// operator notices and runs gc doctor --fix" rebuilds that failure in a new
	// place.
	//
	// Ten minutes, not an hour. An hour is the better throttle in the steady
	// state (6 probes/hour/row against the ~180/hour the unbounded loop ran at,
	// versus 36) but it is the wrong number for the case that decides this
	// constant: an infrastructure outage in which NO seat can start. Every row
	// then takes its strikes for a reason that has nothing to do with the row,
	// and when the outage clears the city is only as responsive as this
	// interval. Ten minutes bounds that recovery lag at ten minutes and still
	// removes 97% of the loop's spend. Recovery time beats throttle depth,
	// because the throttle only has to be good enough to stop the bleeding.
	demandLoopQuarantineRetry = 10 * time.Minute
)

// demandLoopQuarantineReason is the fixed reason string on the marker. One
// value today, but it is a named constant and a separate metadata key because
// route recovery learned the same lesson: an operator reading the doctor line
// needs to know WHICH condition parked the row, and adding the second reason
// later must not mean re-teaching the surface how to carry one.
const demandLoopQuarantineReason = "unclaimable-demand-loop"

// demandLoopLedger is the parsed gc.demand_loop_strikes marker.
type demandLoopLedger struct {
	Strikes     int
	LastMint    time.Time
	Fingerprint string
}

// encode renders the ledger for storage: "<strikes>|<lastMint>|<fingerprint>".
func (l demandLoopLedger) encode() string {
	return fmt.Sprintf("%d|%s|%s", l.Strikes, l.LastMint.UTC().Format(time.RFC3339), l.Fingerprint)
}

// parseDemandLoopLedger reads the marker. A malformed value is treated as
// ABSENT, never as a reason to suppress: a hand-edited or truncated marker must
// not be able to park a row, which is the failure mode that turns a safety
// device into an outage.
func parseDemandLoopLedger(raw string) (demandLoopLedger, bool) {
	parts := strings.Split(strings.TrimSpace(raw), "|")
	if len(parts) != 3 {
		return demandLoopLedger{}, false
	}
	strikes, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || strikes <= 0 {
		return demandLoopLedger{}, false
	}
	last, err := time.Parse(time.RFC3339, strings.TrimSpace(parts[1]))
	if err != nil {
		return demandLoopLedger{}, false
	}
	fingerprint := strings.TrimSpace(parts[2])
	if fingerprint == "" {
		return demandLoopLedger{}, false
	}
	return demandLoopLedger{Strikes: strikes, LastMint: last, Fingerprint: fingerprint}, true
}

// demandLoopFingerprint captures exactly the row state both readers consult, so
// that any edit which could plausibly change either side's verdict re-arms the
// row immediately and a cosmetic edit does not.
//
// It is built from the same inputs demandRowServable and hookClaimMatchesRoute
// read — status, type, assignee, labels, the route triple, and the deferral
// fields — and deliberately NOT from the title, the timestamps, or the marker
// itself, which would make every strike change the fingerprint and the counter
// could never reach two.
//
// Dependency state is NOT in here, and that is not an oversight: a row's
// blockedness lives on its edges rather than on the row, so it cannot be
// fingerprinted from the bead alone. That is precisely the gap
// demandLoopQuarantineRetry covers — a blocker closing is the canonical
// "cause cleared outside the row" case.
func demandLoopFingerprint(b beads.Bead) string {
	labels := append([]string(nil), b.Labels...)
	for i := range labels {
		labels[i] = strings.TrimSpace(labels[i])
	}
	sort.Strings(labels)
	deferUntil := ""
	if b.DeferUntil != nil {
		deferUntil = b.DeferUntil.UTC().Format(time.RFC3339)
	}
	// Each field is length-prefixed so that no value can impersonate the
	// delimiter structure — two labels "a" and "b:c" must not hash the same as
	// one label "a:b" and one "c". route_recovery's sibling learned this as a
	// review finding; it is cheaper to just not have the bug.
	var sb strings.Builder
	write := func(name, value string) {
		fmt.Fprintf(&sb, "%s=%d:%s;", name, len(value), value)
	}
	write("status", strings.TrimSpace(b.Status))
	write("type", strings.TrimSpace(b.Type))
	write("assignee", strings.TrimSpace(b.Assignee))
	write("labels", strconv.Itoa(len(labels)))
	for _, label := range labels {
		write("label", label)
	}
	write("routed_to", strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey]))
	write("run_target", strings.TrimSpace(b.Metadata[beadmeta.RunTargetMetadataKey]))
	write("kind", strings.TrimSpace(b.Metadata[beadmeta.KindMetadataKey]))
	write("defer_until", deferUntil)
	write("indefinitely_deferred", strconv.FormatBool(b.IndefinitelyDeferred))
	h := fnv.New64a()
	_, _ = h.Write([]byte(sb.String()))
	return strconv.FormatUint(h.Sum64(), 16)
}

// demandLoopQuarantineActive reports whether the breaker is currently
// suppressing this row's demand.
//
// Three ways it is NOT active, and each one is a way work gets out:
//
//  1. Fewer than demandLoopStrikeLimit strikes recorded.
//  2. The row changed since the strikes were recorded (fingerprint mismatch).
//     A reroute, a relabel, an unblock-by-edit, a defer or an undefer all land
//     here and re-arm the row on the very next tick.
//  3. demandLoopQuarantineRetry has elapsed since the last mint, so the row
//     gets one probe seat. If it is still unclaimable that probe records another
//     strike and pushes the next probe out another interval; if the cause
//     cleared, the seat claims it and the row leaves the demand set for good.
//
// A future-dated marker fails OPEN. A clock skew between the controller and a
// seat, or a hand-edited timestamp, must not be able to park a row for as long
// as the skew lasts — the cost of ignoring a good marker is one extra seat, and
// the cost of honoring a bad one is work that never runs.
func demandLoopQuarantineActive(b beads.Bead, now time.Time) bool {
	ledger, ok := parseDemandLoopLedger(b.Metadata[beadmeta.DemandLoopStrikesMetadataKey])
	if !ok || ledger.Strikes < demandLoopStrikeLimit {
		return false
	}
	if ledger.Fingerprint != demandLoopFingerprint(b) {
		return false
	}
	if ledger.LastMint.After(now) {
		return false
	}
	return now.Sub(ledger.LastMint) < demandLoopQuarantineRetry
}

// isDemandLoopQuarantined reports whether a row carries the operator-facing
// marker. Distinct from demandLoopQuarantineActive on purpose: this one answers
// "has the breaker tripped on this row", which is what the doctor check and an
// operator want, and stays true through the retry window when the row is
// briefly countable again. Suppression is a moment; the finding is a state.
func isDemandLoopQuarantined(b beads.Bead) bool {
	return strings.TrimSpace(b.Metadata[beadmeta.DemandLoopQuarantinedMetadataKey]) == "true"
}

// nextDemandLoopLedger computes the ledger a fresh seat-mint produces.
//
// Strikes accumulate only against an UNCHANGED row inside the strike window;
// anything else starts the count over at one. Both resets matter: the
// fingerprint reset is how an edited row gets a clean slate, and the window
// reset is how a row that legitimately mints a seat now and then never
// accumulates its way into a quarantine it does not deserve.
func nextDemandLoopLedger(b beads.Bead, now time.Time) demandLoopLedger {
	fingerprint := demandLoopFingerprint(b)
	next := demandLoopLedger{Strikes: 1, LastMint: now, Fingerprint: fingerprint}
	prior, ok := parseDemandLoopLedger(b.Metadata[beadmeta.DemandLoopStrikesMetadataKey])
	if !ok || prior.Fingerprint != fingerprint {
		return next
	}
	// An ALREADY-QUARANTINED row never resets on the window, and this clause has
	// to come first. demandLoopQuarantineRetry is deliberately longer than
	// demandLoopStrikeWindow — that is what makes a probe a probe — so the window
	// test below is true for every probe by construction. Letting it reset would
	// hand the row a fresh budget of demandLoopStrikeLimit seats after each
	// retry interval: a floodgate wearing a probe's clothes, and a quiet
	// multiplication of exactly the spend this file exists to stop.
	if prior.Strikes >= demandLoopStrikeLimit {
		next.Strikes = prior.Strikes + 1
		return next
	}
	if prior.LastMint.After(now) || now.Sub(prior.LastMint) > demandLoopStrikeWindow {
		return next
	}
	next.Strikes = prior.Strikes + 1
	return next
}

// noteDemandSeatMinted records that the controller spent a seat on this row and
// trips the breaker when the row has now consumed demandLoopStrikeLimit of them
// without ever being claimed.
//
// It is called at the MINT, not at the drain. The drain is in the seat's own
// process and is best-effort by design (demand_divergence.go is explicit that a
// diagnostics counter must never become a second failure mode on the drain
// path), so a breaker that depended on it would be disarmed by exactly the
// conditions — a wedged seat, an unreadable store, a killed pane — that make the
// loop most expensive. The mint is in the controller, happens once per seat, and
// is the event that actually costs the quota.
//
// Every write failure is logged and swallowed. A store that cannot take the
// marker must not stop the controller reconciling; it degrades to the behaviour
// that exists today, which is the loop, and the stderr line is how that is
// visible.
func noteDemandSeatMinted(store beads.Store, b beads.Bead, now time.Time, stderr io.Writer) {
	if store == nil || strings.TrimSpace(b.ID) == "" {
		return
	}
	ledger := nextDemandLoopLedger(b, now)
	patch := map[string]string{beadmeta.DemandLoopStrikesMetadataKey: ledger.encode()}
	tripped := ledger.Strikes == demandLoopStrikeLimit && !isDemandLoopQuarantined(b)
	if ledger.Strikes >= demandLoopStrikeLimit {
		patch[beadmeta.DemandLoopQuarantinedMetadataKey] = "true"
		patch[beadmeta.DemandLoopQuarantineReasonMetadataKey] = demandLoopQuarantineReason
	} else if isDemandLoopQuarantined(b) {
		// The count restarted — the row was edited, or its strikes aged out — so
		// the marker is now describing a row that IS being counted. It clears
		// itself here rather than waiting for `gc doctor --fix`, for the same
		// reason the route-recovery lane clears its own: a finding an operator
		// cannot make go away by fixing the underlying problem trains them to
		// stop reading the check.
		patch[beadmeta.DemandLoopQuarantinedMetadataKey] = ""
		patch[beadmeta.DemandLoopQuarantineReasonMetadataKey] = ""
	}
	if err := store.SetMetadataBatch(b.ID, patch); err != nil {
		if stderr != nil {
			fmt.Fprintf(stderr, "demand loop breaker: %s: recording seat mint: %v\n", b.ID, err) //nolint:errcheck
		}
		return
	}
	if tripped && stderr != nil {
		// Edge-triggered, once per streak, in the shape control.stalled uses.
		// Loud on purpose: this is the line that should have existed on Node C.
		fmt.Fprintf(stderr, "demand loop breaker: %s minted %d seats without ever being claimed — no longer counted as demand (gc doctor: demand-loop-quarantine)\n", //nolint:errcheck
			b.ID, ledger.Strikes)
	}
}

// clearDemandLoopLedger removes every trace of the breaker from a row. Used by
// the doctor check's --fix and by nothing else: a row that leaves the demand set
// legitimately keeps its (now inert) ledger until it is edited, which costs one
// metadata key and avoids a write on the hot path for every row that is behaving.
func clearDemandLoopLedger() map[string]string {
	return map[string]string{
		beadmeta.DemandLoopStrikesMetadataKey:          "",
		beadmeta.DemandLoopQuarantinedMetadataKey:      "",
		beadmeta.DemandLoopQuarantineReasonMetadataKey: "",
	}
}

// recordDemandSeatMints charges every seat this tick minted on demand evidence
// against the row that justified it.
//
// Only an ANONYMOUS NEW request counts. A protected or in-flight request is a
// session that already exists — it was charged on the tick that created it, and
// charging it again every tick it stays alive would quarantine rows that are
// being worked on perfectly well. That is the distinction between "the
// controller spent a seat on this row" and "a seat that exists is pointed at
// this row", and only the first one costs anything.
//
// Each charged row is READ BACK from its own store rather than looked up in one
// of the tick's earlier snapshots. Deliberate, and worth the reads: the
// snapshots are keyed by a store-ref vocabulary this function would have to
// agree with, and several passes rewrite routes through the store after those
// snapshots are taken. A key mismatch or a stale route would make the breaker
// silently inert — no error, no event, and the loop back at full rate — which is
// the single worst failure mode available to a safety device. The read is
// bounded by the number of seats minted this tick (a pool's max, not the
// backlog's depth), and a seat create is orders of magnitude more expensive than
// a bead read.
func recordDemandSeatMints(
	poolStates []PoolDesiredState,
	cityStore beads.Store,
	rigStores map[string]beads.Store,
	now time.Time,
	stderr io.Writer,
) {
	charged := make(map[string]struct{})
	for _, state := range poolStates {
		// A template whose pool is CLAIMING is not in a demand/claim loop, and
		// none of its rows may take a strike this tick.
		//
		// Without this, the breaker punishes a row for the scheduler's
		// behaviour. The controller hands each new seat a trigger row off the
		// demand list, but the pool is PULL: the seat runs its own query and
		// claims whatever that query ranks first, which under load is a
		// higher-priority row that arrived later. A stable low-priority row can
		// therefore justify five seats in a row, be passed over by all five
		// because a sibling outranked it, and get parked while perfectly
		// healthy — starvation misread as unclaimability.
		//
		// A non-"new" request is the evidence: ComputePoolDesiredStates mints
		// "resume" and "wake-known-identity" tiers only from work that is
		// actually ASSIGNED to this template. If any exist, seats are reaching
		// work and the empty-drain loop is not what is happening here.
		//
		// The cost is a real false negative: a genuine loop row on a pool that
		// is also doing useful work goes uncaught, and behaves as it does today.
		// Taken deliberately. A false positive buries work an operator is
		// waiting on; a false negative leaves in place a loop that has run
		// unbounded for months. Those are not symmetric, and the first landing
		// of a breaker should be the conservative one.
		if poolStateHasAssignedWork(state) {
			continue
		}
		for _, req := range state.Requests {
			if req.Tier != "new" || strings.TrimSpace(req.SessionBeadID) != "" {
				continue
			}
			id := strings.TrimSpace(req.WorkBeadID)
			if id == "" {
				continue
			}
			if _, done := charged[id]; done {
				continue
			}
			charged[id] = struct{}{}
			store := demandLoopStoreForRef(req.WorkStoreRef, cityStore, rigStores)
			if store == nil {
				// Fail open and SAY SO. A silently uncharged mint is the
				// breaker's worst state: the controller keeps minting seats for
				// this row every tick, strikes never accumulate, and there is no
				// error anywhere to explain why the loop this file exists to
				// stop is running at full rate.
				if stderr != nil {
					fmt.Fprintf(stderr, "demand loop breaker: %s: no store for ref %q — seat mint not charged, this row cannot be parked\n", id, req.WorkStoreRef) //nolint:errcheck
				}
				continue
			}
			row, err := store.Get(id)
			if err != nil {
				// Same discipline: a store that cannot be read degrades to
				// today's behaviour, which is the loop, and the line is how that
				// is visible instead of silent.
				if stderr != nil {
					fmt.Fprintf(stderr, "demand loop breaker: %s: reading row to charge a seat mint: %v\n", id, err) //nolint:errcheck
				}
				continue
			}
			noteDemandSeatMinted(store, row, now, stderr)
		}
	}
}

// poolStateHasAssignedWork reports whether this template currently has work
// assigned to it — a session resuming in-progress work, or one to be woken for
// it. Both tiers are minted only from assigned rows, so either is proof the
// pool's seats are reaching work.
func poolStateHasAssignedWork(state PoolDesiredState) bool {
	for _, req := range state.Requests {
		if req.Tier != "new" {
			return true
		}
	}
	return false
}

// demandLoopStoreForRef resolves the store a demand row was counted in. An
// empty or unrecognized ref falls back to the city store, matching
// normalizeDemandStoreRef's own treatment of a class binding as city scope.
func demandLoopStoreForRef(storeRef string, cityStore beads.Store, rigStores map[string]beads.Store) beads.Store {
	if rigName, ok := strings.CutPrefix(normalizeDemandStoreRef(storeRef), "rig:"); ok {
		return rigStores[rigName]
	}
	return cityStore
}
