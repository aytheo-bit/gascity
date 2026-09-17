package main

import (
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
		noteDemandSeatMinted(store, row, now, io.Discard)
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
	noteDemandSeatMinted(store, parked, justAfter, io.Discard)
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
}

// TestStrikesExpireOutsideTheWindow: a row that mints a seat now and then is
// not the loop, and must not accumulate its way into a quarantine.
func TestStrikesExpireOutsideTheWindow(t *testing.T) {
	store := beads.NewMemStore()
	created, err := store.Create(breakerRow("b-6"))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	now := demandLoopNow
	for i := 0; i < 10; i++ {
		row, err := store.Get(created.ID)
		if err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
		noteDemandSeatMinted(store, row, now, io.Discard)
		parked, err := store.Get(created.ID)
		if err != nil {
			t.Fatalf("mint %d re-read: %v", i, err)
		}
		if demandLoopQuarantineActive(parked, now) {
			t.Fatalf("a row minting one seat per %v was quarantined after %d mints", demandLoopStrikeWindow+time.Minute, i+1)
		}
		now = now.Add(demandLoopStrikeWindow + time.Minute)
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
		noteDemandSeatMinted(store, row, at, io.Discard)
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
	noteDemandSeatMinted(store, edited, demandLoopNow.Add(time.Minute), io.Discard)
	cleared, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("re-reading after mint: %v", err)
	}
	if isDemandLoopQuarantined(cleared) {
		t.Error("an edited row kept its quarantine marker; only gc doctor --fix could clear it")
	}
}

// TestPoolWithAssignedWorkChargesNoStrikes is review finding 2: the controller
// hands a new seat a trigger row, but the pool is PULL and the seat claims
// whatever its own query ranks first. A stable low-priority row can justify five
// seats and be passed over by all five. That is starvation, not unclaimability,
// and it must not park the row.
func TestPoolWithAssignedWorkChargesNoStrikes(t *testing.T) {
	store := beads.NewMemStore()
	created, err := store.Create(breakerRow("r-2"))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	busy := PoolDesiredState{Template: agreementTemplate, Requests: []SessionRequest{
		// A seat resuming work it already holds: this pool is claiming.
		{Template: agreementTemplate, Tier: "resume", SessionBeadID: "sess-1", WorkBeadID: "other-row"},
		{Template: agreementTemplate, Tier: "new", WorkBeadID: created.ID},
	}}
	now := demandLoopNow
	for i := 0; i < 20; i++ {
		recordDemandSeatMints([]PoolDesiredState{busy}, store, nil, now, io.Discard)
		now = now.Add(20 * time.Second)
	}
	row, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("re-reading: %v", err)
	}
	if isDemandLoopQuarantined(row) {
		t.Error("a row on a pool that is actively claiming was parked — starvation was misread as unclaimability")
	}
	if raw := row.Metadata[beadmeta.DemandLoopStrikesMetadataKey]; strings.TrimSpace(raw) != "" {
		t.Errorf("strikes were charged on a claiming pool: %q", raw)
	}
}

// TestIdlePoolChargesStrikesThroughTheRealMintPath is the other half: with no
// assigned work, recordDemandSeatMints must actually reach the ledger. Drives
// the production entry point rather than noteDemandSeatMinted, so a wrong
// request filter or store resolution shows up as an inert breaker here.
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
		recordDemandSeatMints([]PoolDesiredState{idle}, store, nil, now, io.Discard)
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
