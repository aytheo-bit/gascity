package main

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

func breakerRow(id string) beads.Bead {
	return beads.Bead{
		ID: id, Status: "open", Type: "task",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: agreementTemplate},
	}
}

// mintUntilQuarantined drives seat mints against a stored row until the breaker
// parks it, and reports how many it took. It re-reads between mints exactly as
// the controller does, so the ledger it accumulates is the persisted one.
// It returns the mint count, the parked row, and the CLOCK it parked at. The
// clock is not incidental: demandLoopQuarantineActive fails open on a marker
// dated after `now`, so asking about the parked row at any earlier instant
// correctly reports it as countable.
func mintUntilQuarantined(t *testing.T, store beads.Store, id string, start time.Time, step time.Duration, maxMints int) (int, beads.Bead, time.Time) {
	t.Helper()
	now := start
	for i := 1; i <= maxMints; i++ {
		row, err := store.Get(id)
		if err != nil {
			t.Fatalf("mint %d: reading row: %v", i, err)
		}
		if err := noteDemandSeatMinted(store, row, now, io.Discard); err != nil {
			t.Fatalf("mint %d: charging: %v", i, err)
		}
		parked, err := store.Get(id)
		if err != nil {
			t.Fatalf("mint %d: re-reading row: %v", i, err)
		}
		if demandLoopQuarantineActive(parked, now) {
			return i, parked, now
		}
		now = now.Add(step)
	}
	t.Fatalf("row %s was not quarantined after %d mints", id, maxMints)
	return 0, beads.Bead{}, time.Time{}
}

// TestBreakerTripsAtTheStrikeLimit pins the bound itself. Five is a choice, and
// a test that derived it from the constant would pin nothing.
func TestBreakerTripsAtTheStrikeLimit(t *testing.T) {
	store := beads.NewMemStore()
	created, err := store.Create(breakerRow("b-1"))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	mints, parked, _ := mintUntilQuarantined(t, store, created.ID, demandLoopNow, 20*time.Second, 20)
	if mints != 5 {
		t.Errorf("breaker tripped after %d mints, want 5", mints)
	}
	if !isDemandLoopQuarantined(parked) {
		t.Error("row is suppressed but carries no operator-facing marker")
	}
	if got := parked.Metadata[beadmeta.DemandLoopQuarantineReasonMetadataKey]; got != demandLoopQuarantineReason {
		t.Errorf("quarantine reason = %q, want %q", got, demandLoopQuarantineReason)
	}
}

// TestEditingTheRowReArmsItImmediately is the escape that matters most: an
// operator (or a lane) who fixes the actual cause must not also have to know
// the breaker exists.
func TestEditingTheRowReArmsItImmediately(t *testing.T) {
	cfg := agreementConfig()
	templates := map[string]struct{}{agreementTemplate: {}}
	store := beads.NewMemStore()
	created, err := store.Create(breakerRow("b-2"))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	_, parked, parkedAt := mintUntilQuarantined(t, store, created.ID, demandLoopNow, 20*time.Second, 20)
	if _, counted := demandServableForTemplatesAt(cfg, parked, templates, parkedAt); counted {
		t.Fatal("a parked row is still counted as demand")
	}

	// The row is re-routed, which is a change to a servability input.
	if err := store.SetMetadataBatch(created.ID, map[string]string{
		beadmeta.RoutedToMetadataKey: agreementTemplate + "-2",
	}); err != nil {
		t.Fatalf("re-routing: %v", err)
	}
	edited, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("re-reading: %v", err)
	}
	if demandLoopQuarantineActive(edited, parkedAt) {
		t.Fatal("the row changed but the breaker is still suppressing it — an edited row must re-arm on the next tick")
	}
	// And the strike count starts over rather than resuming at five.
	if next := nextDemandLoopLedger(edited, parkedAt); next.Strikes != 1 {
		t.Errorf("strikes after an edit = %d, want 1", next.Strikes)
	}
}

// TestQuarantineLetsOneProbeThroughPerRetryInterval is the softening of "stop
// spawning", and the reason it is safe to park a row at all: a cause that
// clears OUTSIDE the row — a blocker closing, a rig returning — changes nothing
// the fingerprint can see, so without this the row would be buried until a
// human noticed. That is the exact process failure OPS-78 is about.
func TestQuarantineLetsOneProbeThroughPerRetryInterval(t *testing.T) {
	store := beads.NewMemStore()
	created, err := store.Create(breakerRow("b-3"))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	_, parked, _ := mintUntilQuarantined(t, store, created.ID, demandLoopNow, 20*time.Second, 20)
	ledger, ok := parseDemandLoopLedger(parked.Metadata[beadmeta.DemandLoopStrikesMetadataKey])
	if !ok {
		t.Fatal("parked row has no readable ledger")
	}

	justBefore := ledger.LastMint.Add(demandLoopQuarantineRetry - time.Second)
	if !demandLoopQuarantineActive(parked, justBefore) {
		t.Error("suppression lifted before the retry interval elapsed")
	}
	justAfter := ledger.LastMint.Add(demandLoopQuarantineRetry + time.Second)
	if demandLoopQuarantineActive(parked, justAfter) {
		t.Error("the retry interval elapsed and no probe is allowed through")
	}

	// The probe is ONE seat, not a re-opened floodgate: minting against it
	// pushes the next probe out another full interval.
	if err := noteDemandSeatMinted(store, parked, justAfter, io.Discard); err != nil {
		t.Fatalf("probe mint: %v", err)
	}
	probed, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("re-reading after probe: %v", err)
	}
	if !demandLoopQuarantineActive(probed, justAfter.Add(time.Minute)) {
		t.Error("the probe did not re-park the row — the retry window is a floodgate, not a probe")
	}
}

// TestBreakerFailsOpenOnAnUnusableMarker. A safety device whose failure mode is
// "park the work forever" is worse than the loop it prevents, so every way the
// marker can be wrong resolves to "count the row".
func TestBreakerFailsOpenOnAnUnusableMarker(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{"absent", ""},
		{"truncated", "5|2026-09-17T20:00:00Z"},
		{"non-numeric strikes", "many|2026-09-17T20:00:00Z|abc"},
		{"unparseable timestamp", "5|yesterday|abc"},
		{"empty fingerprint", "5|2026-09-17T20:00:00Z|"},
		{"negative strikes", "-5|2026-09-17T20:00:00Z|abc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := breakerRow("b-4")
			row.Metadata[beadmeta.DemandLoopStrikesMetadataKey] = tc.value
			if demandLoopQuarantineActive(row, demandLoopNow) {
				t.Errorf("a %s marker suppressed the row; it must fail open", tc.name)
			}
		})
	}

	// A marker stamped in the future is clock skew or a hand edit, and honoring
	// it would park the row for as long as the skew lasts.
	future := breakerRow("b-5")
	future.Metadata[beadmeta.DemandLoopStrikesMetadataKey] = demandLoopLedger{
		Strikes:     demandLoopStrikeLimit,
		LastMint:    demandLoopNow.Add(24 * time.Hour),
		Fingerprint: demandLoopFingerprint(future),
	}.encode()
	if demandLoopQuarantineActive(future, demandLoopNow) {
		t.Error("a future-dated marker suppressed the row; it must fail open")
	}

	// POSITIVE CONTROL. Everything above asserts that nothing happened, and a
	// breaker that had been deleted outright would satisfy every one of those
	// assertions (agy, 2026-09-17, finding 3). This is the same call, on a row
	// that differs from the fail-open corpus only in carrying a WELL-FORMED
	// marker: it proves the suppression machinery was present and chose not to
	// act, rather than being absent.
	usable := breakerRow("b-9")
	usable.Metadata[beadmeta.DemandLoopStrikesMetadataKey] = demandLoopLedger{
		Strikes:     demandLoopStrikeLimit,
		LastMint:    demandLoopNow.Add(-time.Minute),
		Fingerprint: demandLoopFingerprint(usable),
	}.encode()
	if !demandLoopQuarantineActive(usable, demandLoopNow) {
		t.Fatal("CONTROL FAILED: a well-formed at-limit marker did not suppress the row — the fail-open cases above prove nothing, because nothing suppresses anything")
	}
}

// TestStrikesExpireOutsideTheWindow: a row that mints a seat now and then is
// not the loop, and must not accumulate its way into a quarantine.
func TestStrikesExpireOutsideTheWindow(t *testing.T) {
	// mintAtCadence drives ten mints spaced `step` apart and reports whether the
	// row ever got parked. Both halves of this test run it; the ONLY difference
	// between them is the spacing, which is the variable the window is about.
	mintAtCadence := func(t *testing.T, id string, step time.Duration) bool {
		t.Helper()
		store := beads.NewMemStore()
		created, err := store.Create(breakerRow(id))
		if err != nil {
			t.Fatalf("seeding: %v", err)
		}
		now := demandLoopNow
		for i := 0; i < 10; i++ {
			row, err := store.Get(created.ID)
			if err != nil {
				t.Fatalf("mint %d: %v", i, err)
			}
			if err := noteDemandSeatMinted(store, row, now, io.Discard); err != nil {
				t.Fatalf("mint %d: charging: %v", i, err)
			}
			parked, err := store.Get(created.ID)
			if err != nil {
				t.Fatalf("mint %d re-read: %v", i, err)
			}
			if demandLoopQuarantineActive(parked, now) {
				return true
			}
			now = now.Add(step)
		}
		return false
	}

	if mintAtCadence(t, "b-6", demandLoopStrikeWindow+time.Minute) {
		t.Errorf("a row minting one seat per %v was quarantined", demandLoopStrikeWindow+time.Minute)
	}

	// POSITIVE CONTROL. The assertion above is that nothing happened, and it
	// would hold just as well if the ledger were never written at all (agy,
	// 2026-09-17, finding 3). Same helper, same row shape, same number of mints
	// — only the cadence changes — so a pass here is proof that the strikes the
	// test above is asserting the EXPIRY of were actually being recorded.
	if !mintAtCadence(t, "b-6b", 20*time.Second) {
		t.Fatal("CONTROL FAILED: ten mints twenty seconds apart never parked the row — the expiry assertion above proves nothing, because no strike is ever recorded")
	}
}

// TestFingerprintIgnoresCosmeticEdits. If the marker itself, or a title, moved
// the fingerprint, the counter could never reach two and the breaker would be
// permanently inert — the quietest possible way for this fix to not exist.
func TestFingerprintIgnoresCosmeticEdits(t *testing.T) {
	base := breakerRow("b-7")
	want := demandLoopFingerprint(base)

	cosmetic := breakerRow("b-7")
	cosmetic.Title = "a completely different title"
	cosmetic.Metadata[beadmeta.DemandLoopStrikesMetadataKey] = "3|2026-09-17T20:00:00Z|deadbeef"
	cosmetic.Metadata[beadmeta.DemandLoopQuarantinedMetadataKey] = "true"
	if got := demandLoopFingerprint(cosmetic); got != want {
		t.Errorf("a cosmetic edit moved the fingerprint (%s != %s)", got, want)
	}

	// And every servability input must move it.
	for name, mutate := range map[string]func(*beads.Bead){
		"status":   func(b *beads.Bead) { b.Status = "in_progress" },
		"type":     func(b *beads.Bead) { b.Type = "epic" },
		"assignee": func(b *beads.Bead) { b.Assignee = "someone" },
		"label":    func(b *beads.Bead) { b.Labels = []string{beadmeta.DispatchHoldLabels[0]} },
		"route":    func(b *beads.Bead) { b.Metadata[beadmeta.RoutedToMetadataKey] = "rig/other" },
		"kind":     func(b *beads.Bead) { b.Metadata[beadmeta.KindMetadataKey] = beadmeta.KindWorkflow },
		"defer": func(b *beads.Bead) {
			at := demandLoopNow.Add(time.Hour)
			b.DeferUntil = &at
		},
		"indefinite defer": func(b *beads.Bead) { b.IndefinitelyDeferred = true },
	} {
		t.Run(name, func(t *testing.T) {
			row := breakerRow("b-7")
			mutate(&row)
			if got := demandLoopFingerprint(row); got == want {
				t.Errorf("changing %s did not move the fingerprint — an edited row would stay parked", name)
			}
		})
	}

	// Label sets must not collide through the delimiter.
	a := breakerRow("b-8")
	a.Labels = []string{"x", "y:z"}
	b := breakerRow("b-8")
	b.Labels = []string{"x:y", "z"}
	if demandLoopFingerprint(a) == demandLoopFingerprint(b) {
		t.Error("two different label sets share a fingerprint")
	}
}

// --- Regressions for the independent review's findings (agy, 2026-09-17) ---

// TestProbeMintNeverResetsAQuarantinedRow is review finding 4, which was a real
// bug: demandLoopQuarantineRetry is longer than demandLoopStrikeWindow, so every
// probe mint tripped the window-expiry reset and handed the row a fresh budget
// of demandLoopStrikeLimit seats. A floodgate wearing a probe's clothes.
func TestProbeMintNeverResetsAQuarantinedRow(t *testing.T) {
	store := beads.NewMemStore()
	created, err := store.Create(breakerRow("r-4"))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	_, parked, _ := mintUntilQuarantined(t, store, created.ID, demandLoopNow, 20*time.Second, 20)
	ledger, ok := parseDemandLoopLedger(parked.Metadata[beadmeta.DemandLoopStrikesMetadataKey])
	if !ok {
		t.Fatal("parked row has no readable ledger")
	}

	// Five probes, each one retry-interval after the last. Every one must add a
	// strike and re-park the row, so the row never gets a second free run.
	at := ledger.LastMint
	want := ledger.Strikes
	for i := 1; i <= 5; i++ {
		at = at.Add(demandLoopQuarantineRetry + time.Second)
		row, err := store.Get(created.ID)
		if err != nil {
			t.Fatalf("probe %d: %v", i, err)
		}
		if !demandLoopQuarantineActive(row, at.Add(-2*time.Second)) {
			t.Fatalf("probe %d: row was not parked going into the probe", i)
		}
		if err := noteDemandSeatMinted(store, row, at, io.Discard); err != nil {
			t.Fatalf("probe %d: charging: %v", i, err)
		}
		probed, err := store.Get(created.ID)
		if err != nil {
			t.Fatalf("probe %d re-read: %v", i, err)
		}
		next, ok := parseDemandLoopLedger(probed.Metadata[beadmeta.DemandLoopStrikesMetadataKey])
		if !ok {
			t.Fatalf("probe %d: ledger unreadable", i)
		}
		want++
		if next.Strikes != want {
			t.Fatalf("probe %d: strikes = %d, want %d — the probe reset the ledger instead of counting", i, next.Strikes, want)
		}
		if !demandLoopQuarantineActive(probed, at.Add(time.Minute)) {
			t.Fatalf("probe %d: the row was not re-parked after its probe", i)
		}
	}
}

// TestEditedRowClearsItsOwnQuarantineMarker is review finding 5: a marker that
// only `gc doctor --fix` can remove makes the check report rows that are being
// counted perfectly well, and an operator who cannot clear a finding by fixing
// the problem stops reading the check.
func TestEditedRowClearsItsOwnQuarantineMarker(t *testing.T) {
	store := beads.NewMemStore()
	created, err := store.Create(breakerRow("r-5"))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	_, parked, _ := mintUntilQuarantined(t, store, created.ID, demandLoopNow, 20*time.Second, 20)
	if !isDemandLoopQuarantined(parked) {
		t.Fatal("row is not marked")
	}
	if err := store.SetMetadataBatch(created.ID, map[string]string{
		beadmeta.RoutedToMetadataKey: agreementTemplate + "-2",
	}); err != nil {
		t.Fatalf("re-routing: %v", err)
	}
	edited, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("re-reading: %v", err)
	}
	if err := noteDemandSeatMinted(store, edited, demandLoopNow.Add(time.Minute), io.Discard); err != nil {
		t.Fatalf("mint after edit: %v", err)
	}
	cleared, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("re-reading after mint: %v", err)
	}
	if isDemandLoopQuarantined(cleared) {
		t.Error("an edited row kept its quarantine marker; only gc doctor --fix could clear it")
	}
}

// TestBreakerStaysArmedOnAPoolWithAssignedWork is the reversal of the first
// landing's pool-wide exemption (agy, 2026-09-17, finding 2).
//
// The first draft skipped every row on a template that had any resume or wake
// request, on a starvation argument. The evidence did not support it — an
// assigned row is excluded from a worker's ready query, so it never competes
// with the new seats — and the signal is a level that stays true for the whole
// life of a resumed session, so one long-running worker switched the breaker off
// for its template indefinitely. See the essay above chargeDemandSeatMints.
//
// This test is the one the old behaviour fails: an unclaimable row on a pool
// that also has a session resuming other work must still be parked.
func TestBreakerStaysArmedOnAPoolWithAssignedWork(t *testing.T) {
	store := beads.NewMemStore()
	created, err := store.Create(breakerRow("r-2"))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	busy := PoolDesiredState{Template: agreementTemplate, Requests: []SessionRequest{
		// A seat resuming work it already holds. Under the first landing this
		// one request exempted every other row on the template.
		{Template: agreementTemplate, Tier: "resume", SessionBeadID: "sess-1", WorkBeadID: "other-row"},
		{Template: agreementTemplate, Tier: "new", WorkBeadID: created.ID, WorkStoreRef: "city"},
	}}
	now := demandLoopNow
	for i := 0; i < demandLoopStrikeLimit; i++ {
		states := []PoolDesiredState{busy}
		chargeDemandSeatMints(states, store, nil, now, io.Discard)
		now = now.Add(20 * time.Second)
	}
	row, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("re-reading: %v", err)
	}
	if !isDemandLoopQuarantined(row) {
		t.Fatal("a pool with one resumed session disabled the breaker for an unclaimable row on the same template")
	}

	// And the resume request itself is untouched: the breaker charges seats the
	// controller is about to spend, never a session that already exists.
	if raw := strings.TrimSpace(busy.Requests[0].WorkBeadID); raw != "other-row" {
		t.Errorf("resume request was rewritten: %+v", busy.Requests[0])
	}
	other, err := store.Get("other-row")
	if err == nil && strings.TrimSpace(other.Metadata[beadmeta.DemandLoopStrikesMetadataKey]) != "" {
		t.Error("the resumed row was charged a strike")
	}
}

// TestASeatAlreadyPointedAtARowIsNotChargedAgain is the exemption that DID
// survive the review, and the correct unit for one: the row, not the pool.
//
// An in-flight or protected "new" request carries the session bead of a seat
// that already exists. It was charged on the tick that created it, and charging
// it again every tick it lives would park rows that are being worked on. This is
// also what makes a strike mean one SEAT rather than one tick.
func TestASeatAlreadyPointedAtARowIsNotChargedAgain(t *testing.T) {
	charge := func(t *testing.T, id string, req SessionRequest) beads.Bead {
		t.Helper()
		store := beads.NewMemStore()
		created, err := store.Create(breakerRow(id))
		if err != nil {
			t.Fatalf("seeding: %v", err)
		}
		req.WorkBeadID = created.ID
		now := demandLoopNow
		for i := 0; i < 20; i++ {
			chargeDemandSeatMints([]PoolDesiredState{{Template: agreementTemplate, Requests: []SessionRequest{req}}}, store, nil, now, io.Discard)
			now = now.Add(20 * time.Second)
		}
		row, err := store.Get(created.ID)
		if err != nil {
			t.Fatalf("re-reading: %v", err)
		}
		return row
	}

	inFlight := charge(t, "r-2b", SessionRequest{Template: agreementTemplate, Tier: "new", SessionBeadID: "sess-9", WorkStoreRef: "city"})
	if isDemandLoopQuarantined(inFlight) {
		t.Error("a row whose seat already exists was charged again every tick and parked")
	}
	if raw := strings.TrimSpace(inFlight.Metadata[beadmeta.DemandLoopStrikesMetadataKey]); raw != "" {
		t.Errorf("strikes charged against a live seat's row: %q", raw)
	}

	// POSITIVE CONTROL. The assertions above are that nothing happened, and they
	// hold trivially if chargeDemandSeatMints never charges anything (agy,
	// 2026-09-17, finding 3). Same helper, same row, same ticks — the request
	// differs only by the SessionBeadID that makes it a seat that already
	// exists.
	anonymous := charge(t, "r-2c", SessionRequest{Template: agreementTemplate, Tier: "new", WorkStoreRef: "city"})
	if !isDemandLoopQuarantined(anonymous) {
		t.Fatal("CONTROL FAILED: the same row charged through an anonymous request was never parked either — the exemption above proves nothing, because nothing is ever charged")
	}
}

// TestIdlePoolChargesStrikesThroughTheRealMintPath drives the production entry
// point rather than noteDemandSeatMinted, so a wrong request filter or store
// resolution shows up as an inert breaker here.
func TestIdlePoolChargesStrikesThroughTheRealMintPath(t *testing.T) {
	store := beads.NewMemStore()
	created, err := store.Create(breakerRow("r-6"))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	idle := PoolDesiredState{Template: agreementTemplate, Requests: []SessionRequest{
		{Template: agreementTemplate, Tier: "new", WorkBeadID: created.ID, WorkStoreRef: "city"},
	}}
	now := demandLoopNow
	for i := 0; i < demandLoopStrikeLimit; i++ {
		chargeDemandSeatMints([]PoolDesiredState{idle}, store, nil, now, io.Discard)
		now = now.Add(20 * time.Second)
	}
	row, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("re-reading: %v", err)
	}
	if !isDemandLoopQuarantined(row) {
		t.Fatal("the real mint path charged no strikes — the breaker is inert")
	}
}

// --- Finding 1: a strike that cannot be recorded must not read as no strike ---

// refusedWriteStore takes reads and refuses every metadata write: a store that
// has degraded, or one whose schema the controller no longer matches.
type refusedWriteStore struct {
	beads.Store
	err error
}

func (s refusedWriteStore) SetMetadataBatch(string, map[string]string) error { return s.err }

// refusedReadStore refuses to hand back the row the seat was minted on.
type refusedReadStore struct {
	beads.Store
	err error
}

func (s refusedReadStore) Get(string) (beads.Bead, error) { return beads.Bead{}, s.err }

// TestAnUnaccountableSeatIsWithheld is review finding 1, the DO-NOT-MERGE one.
//
// The first landing logged read and write failures and reconciled on. So when
// the datastore degraded, strikes never accumulated, the breaker was silently
// defeated, and the loop ran unbounded exactly when the system was already
// unhealthy — the failure mode this file exists to prevent, reintroduced through
// its own error path.
//
// The fix is to withhold the seat, and it is not heavy-handed: a claim is itself
// a write to this same store, so a store that cannot take the strike cannot take
// the assignee either, and every seat minted against it drains empty by
// construction.
func TestAnUnaccountableSeatIsWithheld(t *testing.T) {
	boom := errors.New("datastore unavailable")
	for _, tc := range []struct {
		name  string
		store func(beads.Store) beads.Store
		rigs  map[string]beads.Store
		ref   string
	}{
		{name: "write refused", store: func(s beads.Store) beads.Store { return refusedWriteStore{Store: s, err: boom} }, ref: "city"},
		{name: "read refused", store: func(s beads.Store) beads.Store { return refusedReadStore{Store: s, err: boom} }, ref: "city"},
		{name: "no store for the row's ref", store: func(s beads.Store) beads.Store { return s }, ref: "rig:absent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backing := beads.NewMemStore()
			created, err := backing.Create(breakerRow("w-1"))
			if err != nil {
				t.Fatalf("seeding: %v", err)
			}
			var log strings.Builder
			states := []PoolDesiredState{{Template: agreementTemplate, Requests: []SessionRequest{
				{Template: agreementTemplate, Tier: "new", WorkBeadID: created.ID, WorkStoreRef: tc.ref},
			}}}
			chargeDemandSeatMints(states, tc.store(backing), tc.rigs, demandLoopNow, &log)

			if len(states[0].Requests) != 0 {
				t.Fatalf("the seat was still minted after its strike could not be recorded: %+v", states[0].Requests)
			}
			if !strings.Contains(log.String(), "seat withheld this tick") {
				t.Errorf("withholding was silent; stderr was %q", log.String())
			}
		})
	}

	// POSITIVE CONTROL. Every assertion above is that a request DISAPPEARED,
	// which a function that dropped every request unconditionally would also
	// satisfy. Identical call on a healthy store: the seat survives and the
	// strike lands.
	healthy := beads.NewMemStore()
	created, err := healthy.Create(breakerRow("w-2"))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	states := []PoolDesiredState{{Template: agreementTemplate, Requests: []SessionRequest{
		{Template: agreementTemplate, Tier: "new", WorkBeadID: created.ID, WorkStoreRef: "city"},
	}}}
	chargeDemandSeatMints(states, healthy, nil, demandLoopNow, io.Discard)
	if len(states[0].Requests) != 1 {
		t.Fatal("CONTROL FAILED: a healthy store's seat was withheld too — the withholding assertions above prove nothing")
	}
	row, err := healthy.Get(created.ID)
	if err != nil {
		t.Fatalf("re-reading: %v", err)
	}
	if next, ok := parseDemandLoopLedger(row.Metadata[beadmeta.DemandLoopStrikesMetadataKey]); !ok || next.Strikes != 1 {
		t.Fatalf("CONTROL FAILED: healthy store recorded ledger %q, want one strike", row.Metadata[beadmeta.DemandLoopStrikesMetadataKey])
	}
}

// TestWithholdingNeverTouchesASessionThatExists bounds the blast radius of
// finding 1's fix. Withholding saves a seat that has not been created; applied
// to a resume or an in-flight request it would STOP a running session, which is
// how a safety device becomes the outage.
func TestWithholdingNeverTouchesASessionThatExists(t *testing.T) {
	backing := beads.NewMemStore()
	created, err := backing.Create(breakerRow("w-3"))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	states := []PoolDesiredState{{Template: agreementTemplate, Requests: []SessionRequest{
		{Template: agreementTemplate, Tier: "resume", SessionBeadID: "sess-1", WorkBeadID: created.ID},
		{Template: agreementTemplate, Tier: "new", SessionBeadID: "sess-2", WorkBeadID: created.ID, WorkStoreRef: "city"},
		{Template: agreementTemplate, Tier: "new", FloorGuarantee: true},
		{Template: agreementTemplate, Tier: "new", WorkBeadID: created.ID, WorkStoreRef: "city"},
	}}}
	chargeDemandSeatMints(states, refusedWriteStore{Store: backing, err: errors.New("datastore unavailable")}, nil, demandLoopNow, io.Discard)

	if len(states[0].Requests) != 3 {
		t.Fatalf("withheld the wrong requests: %+v", states[0].Requests)
	}
	for _, req := range states[0].Requests {
		if req.Tier == "new" && req.SessionBeadID == "" && !req.FloorGuarantee {
			t.Errorf("the anonymous unaccountable seat survived: %+v", req)
		}
	}
	// The floor request is a promise about the pool, not a claim about a row: it
	// carries no work bead, so the breaker must never be able to suppress a
	// min_active_sessions spawn.
	floors := 0
	for _, req := range states[0].Requests {
		if req.FloorGuarantee {
			floors++
		}
	}
	if floors != 1 {
		t.Errorf("a min_active_sessions floor seat was withheld by the breaker (%d left)", floors)
	}
}
