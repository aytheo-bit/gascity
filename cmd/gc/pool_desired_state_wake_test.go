package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// TestComputePoolDesiredStates_FreshWakePreservesPostSuspendUserHold proves
// that the generic pool demand pass retains a session explicitly suspended by
// the user. The drain completion deliberately clears sleep_intent, leaving the
// durable post-suspend shape as asleep + sleep_reason=user-hold + a future
// held_until. A fresh-wake worker must not interpret that row as a stale asleep
// holder and create a replacement for its still-in-progress work.
func TestComputePoolDesiredStates_FreshWakePreservesPostSuspendUserHold(t *testing.T) {
	const sessionID = "sess-user-held"
	now := time.Date(2026, 9, 21, 16, 8, 0, 0, time.UTC)
	agent := poolAgent("claude", "", intPtr(1), 0)
	agent.WakeMode = "fresh"
	cfg := &config.City{Agents: []config.Agent{agent}}

	// This is the persisted state after gc session suspend drains the runtime:
	// CompleteDrainPatch preserves the user-hold sleep reason and held_until,
	// while clearing the transient sleep_intent marker.
	held := poolSessionBeadWithState(sessionID, "asleep", "")
	held.Metadata["sleep_reason"] = "user-hold"
	held.Metadata["held_until"] = now.Add(time.Hour).Format(time.RFC3339)
	held.Metadata["sleep_intent"] = ""
	work := []beads.Bead{workBead("w1", "claude", sessionID, "in_progress", 2)}

	result := ComputePoolDesiredStatesWithDemandTracedAt(
		cfg, work, sessionInfosFromBeads([]beads.Bead{held}), nil, nil, now, nil,
	)
	if len(result) != 1 || len(result[0].Requests) != 1 {
		t.Fatalf("pool demand requests = %#v, want exactly the held session resume", result)
	}
	req := result[0].Requests[0]
	if req.Tier != "resume" || req.SessionBeadID != sessionID {
		t.Fatalf("request = %#v, want resume of user-held session %q without a replacement", req, sessionID)
	}
	if req.WorkBeadID != work[0].ID || work[0].Status != "in_progress" || work[0].Assignee != sessionID {
		t.Fatalf("assigned work changed or was detached: work=%#v request=%#v", work[0], req)
	}

	// Once the hold expires, the ordinary fresh-wake recovery rule applies
	// again: a stopped session is stale and its work needs a clean replacement.
	expired := held
	expired.Metadata = make(map[string]string, len(held.Metadata))
	for key, value := range held.Metadata {
		expired.Metadata[key] = value
	}
	expired.Metadata["held_until"] = now.Add(-time.Hour).Format(time.RFC3339)
	expiredResult := ComputePoolDesiredStatesWithDemandTracedAt(
		cfg, work, sessionInfosFromBeads([]beads.Bead{expired}), nil, nil, now, nil,
	)
	if len(expiredResult) != 1 || len(expiredResult[0].Requests) != 1 {
		t.Fatalf("expired-hold pool demand requests = %#v, want exactly one replacement", expiredResult)
	}
	expiredReq := expiredResult[0].Requests[0]
	if expiredReq.Tier != "wake-known-identity" || expiredReq.SessionBeadID != "" {
		t.Fatalf("expired-hold request = %#v, want clean replacement after the hold expires", expiredReq)
	}
}

// closedPoolSessionBead creates a closed pool-managed session bead whose
// template metadata matches the given qualified template name. Used to
// construct "session bead closed but template still configured" scenarios.
func closedPoolSessionBead(id, template string) beads.Bead {
	return beads.Bead{
		ID:     id,
		Status: "closed",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":             template,
			poolManagedMetadataKey: boolMetadata(true),
		},
	}
}

// TestComputePoolDesiredStates_WakeKnownIdentityForClosedSession verifies that
// an in-progress work bead assigned to a configured, non-suspended pool
// template produces a "wake-known-identity" request when no live session owns
// it.
//
// This is the canonical "orphan recovery" case: a pool agent claimed work,
// the city restarted (or the session was killed), and the session bead is now
// closed — but the template is still live. The reconciler must revive the
// template rather than leaving the work stranded.
func TestComputePoolDesiredStates_WakeKnownIdentityForClosedSession(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "rig", nil, 0)},
	}
	work := []beads.Bead{
		workBead("w1", "rig/claude", "rig/claude", "in_progress", 5),
	}
	closed := closedPoolSessionBead("sess-1", "rig/claude")

	result := ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads([]beads.Bead{closed}), nil)

	wakeCount := 0
	for _, ds := range result {
		for _, req := range ds.Requests {
			if req.Tier == "wake-known-identity" {
				wakeCount++
			}
		}
	}
	if wakeCount != 1 {
		t.Errorf("wake-known-identity count = %d, want 1 — closed session with known template must produce a wake request", wakeCount)
	}
}

// TestComputePoolDesiredStates_WakeKnownIdentityUnknownAssigneeProducesNoRequest
// verifies that a work bead whose assignee does not match any session bead
// (open or closed) produces no request. An unknown assignee cannot be mapped
// to a known identity, so it remains orphaned.
func TestComputePoolDesiredStates_WakeKnownIdentityUnknownAssigneeProducesNoRequest(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "rig", nil, 0)},
	}
	work := []beads.Bead{
		workBead("w1", "rig/claude", "unknown-session-id", "in_progress", 5),
	}
	// No session beads at all — assignee doesn't resolve.
	result := ComputePoolDesiredStates(cfg, work, nil, nil)

	total := 0
	for _, ds := range result {
		total += len(ds.Requests)
	}
	if total != 0 {
		t.Errorf("total requests = %d, want 0 — unknown assignee must produce no request", total)
	}
}

// TestComputePoolDesiredStates_WakeKnownIdentityDedupsMultipleBeadsForSameSession
// verifies that two work beads both assigned to the same configured template
// deduplicate to exactly one wake-known-identity request, not two.
func TestComputePoolDesiredStates_WakeKnownIdentityDedupsMultipleBeadsForSameSession(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "rig", nil, 0)},
	}
	work := []beads.Bead{
		workBead("w1", "rig/claude", "rig/claude", "in_progress", 5),
		workBead("w2", "rig/claude", "rig/claude", "open", 3),
	}
	closed := closedPoolSessionBead("sess-1", "rig/claude")

	result := ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads([]beads.Bead{closed}), nil)

	wakeCount := 0
	for _, ds := range result {
		for _, req := range ds.Requests {
			if req.Tier == "wake-known-identity" {
				wakeCount++
			}
		}
	}
	if wakeCount != 1 {
		t.Errorf("wake-known-identity count = %d, want 1 — two beads for the same closed session must deduplicate to one wake request", wakeCount)
	}
}

// TestComputePoolDesiredStates_LiveSessionContinuesAsResumeTier verifies that
// open sessions still produce Tier="resume" and are not reclassified as
// wake-known-identity. Closed-session recovery must not touch live sessions.
func TestComputePoolDesiredStates_LiveSessionContinuesAsResumeTier(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "rig", nil, 0)},
	}
	work := []beads.Bead{
		workBead("w1", "rig/claude", "sess-live", "in_progress", 5),
	}
	sessions := []beads.Bead{sessionBead("sess-live", "open")}

	result := ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads(sessions), nil)

	if len(result) != 1 || len(result[0].Requests) != 1 {
		t.Fatalf("expected 1 request, got %#v", result)
	}
	req := result[0].Requests[0]
	if req.Tier != "resume" {
		t.Errorf("tier = %q, want resume — live session must stay in resume tier", req.Tier)
	}
	if req.SessionBeadID != "sess-live" {
		t.Errorf("SessionBeadID = %q, want sess-live", req.SessionBeadID)
	}
}

// TestApplyNestedCaps_WakeKnownIdentityRanksBeforeNew verifies that when a cap
// admits only one request and both a wake-known-identity request and a new
// request have equal priority, wake-known-identity is accepted. The sort
// comparator in applyNestedCaps must treat "wake-known-identity" as a
// resume-like tier that ranks ahead of "new" at the same bead priority.
func TestApplyNestedCaps_WakeKnownIdentityRanksBeforeNew(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(1), 0)},
	}
	// New request is listed first so current sort preserves it ahead of
	// wake-known-identity. After the fix, wake-known-identity wins.
	requests := []SessionRequest{
		{Template: "claude", Tier: "new", BeadPriority: 5},
		{Template: "claude", Tier: "wake-known-identity", SessionBeadID: "sess-closed", BeadPriority: 5},
	}

	result := applyNestedCaps(cfg, requests, nil, nil)

	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}
	if len(result[0].Requests) != 1 {
		t.Fatalf("accepted = %d, want 1 (cap=1)", len(result[0].Requests))
	}
	if result[0].Requests[0].Tier != "wake-known-identity" {
		t.Errorf("accepted tier = %q, want wake-known-identity — must rank before new at same priority", result[0].Requests[0].Tier)
	}
}

// TestComputePoolDesiredStates_FreshWakeSkipsAsleepResume verifies the
// wake_mode="fresh" guard for stale asleep pool sessions
// (gastownhall/gascity#4849).
//
// Before the guard, computePoolDesiredStates emitted a Tier:"resume" request for
// any assigned work bead whose assignee resolved to a non-closed session bead —
// without consulting the agent's wake_mode or the session's asleep-ness. So a
// wake_mode="fresh" pool agent with a stale asleep session (left over from before
// a city stop) was told to resume a session it would never reuse, leaving the
// assigned work bound to a row the pool cannot materialize.
//
// The table pins the discriminating branches: a fresh-wake agent must SKIP the
// asleep resume and plan a clean session bound to the same work
// (Tier:"wake-known-identity" with no SessionBeadID) — including under the
// default min_active_sessions=0, where no min-fill request exists to cover it;
// a default/resume-wake agent must STILL resume the asleep session (the guard
// must not over-fire); and a fresh-wake agent must still resume a LIVE session
// (the guard is asleep-specific, not a blanket fresh-agent block).
func TestComputePoolDesiredStates_FreshWakeSkipsAsleepResume(t *testing.T) {
	const sessionID = "sess-1"
	cases := []struct {
		name           string
		wakeMode       string
		sessionState   string
		minSessions    int
		wantTier       string
		wantSessionID  string // resume target; "" when a clean session is expected
		wantWorkBeadID string
	}{
		{
			name:           "fresh wake skips asleep resume and plans a clean session",
			wakeMode:       "fresh",
			sessionState:   "asleep",
			minSessions:    1,
			wantTier:       "wake-known-identity",
			wantSessionID:  "",
			wantWorkBeadID: "w1",
		},
		{
			// The default config: min_active_sessions unset (0) means no
			// min-fill request exists, so the guard itself must carry the
			// replacement or the work is stranded.
			name:           "fresh wake plans a clean session at default min_active_sessions",
			wakeMode:       "fresh",
			sessionState:   "asleep",
			minSessions:    0,
			wantTier:       "wake-known-identity",
			wantSessionID:  "",
			wantWorkBeadID: "w1",
		},
		{
			// normalizeInfoState folds "drained" into StateAsleep, so a
			// drained session takes the same path.
			name:           "fresh wake treats drained as asleep",
			wakeMode:       "fresh",
			sessionState:   "drained",
			minSessions:    0,
			wantTier:       "wake-known-identity",
			wantSessionID:  "",
			wantWorkBeadID: "w1",
		},
		{
			name:           "resume wake still resumes asleep session",
			wakeMode:       "", // unset → EffectiveWakeMode defaults to "resume"
			sessionState:   "asleep",
			minSessions:    1,
			wantTier:       "resume",
			wantSessionID:  sessionID,
			wantWorkBeadID: "w1",
		},
		{
			name:           "fresh wake still resumes a live session",
			wakeMode:       "fresh",
			sessionState:   "active",
			minSessions:    1,
			wantTier:       "resume",
			wantSessionID:  sessionID,
			wantWorkBeadID: "w1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent := poolAgent("claude", "", intPtr(5), tc.minSessions)
			agent.WakeMode = tc.wakeMode
			cfg := &config.City{Agents: []config.Agent{agent}}
			sessions := []beads.Bead{poolSessionBeadWithState(sessionID, tc.sessionState, "")}
			work := []beads.Bead{workBead("w1", "claude", sessionID, "in_progress", 2)}

			result := ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads(sessions), nil)

			if len(result) != 1 {
				t.Fatalf("len(result) = %d, want 1: %#v", len(result), result)
			}
			reqs := result[0].Requests
			if len(reqs) != 1 {
				t.Fatalf("requests = %d, want 1: %#v", len(reqs), reqs)
			}
			if reqs[0].Tier != tc.wantTier {
				t.Errorf("tier = %q, want %q", reqs[0].Tier, tc.wantTier)
			}
			if reqs[0].SessionBeadID != tc.wantSessionID {
				t.Errorf("SessionBeadID = %q, want %q", reqs[0].SessionBeadID, tc.wantSessionID)
			}
			if reqs[0].WorkBeadID != tc.wantWorkBeadID {
				t.Errorf("WorkBeadID = %q, want %q — the replacement session must stay bound to the assigned work", reqs[0].WorkBeadID, tc.wantWorkBeadID)
			}
			// The doomed-resume regression: a fresh-wake agent must never carry a
			// resume for the stale asleep session.
			if tc.wakeMode == "fresh" && tc.sessionState == "asleep" {
				for _, r := range reqs {
					if r.Tier == "resume" && r.SessionBeadID == sessionID {
						t.Errorf("fresh-wake agent resumed stale asleep session %s: %#v", sessionID, r)
					}
				}
			}
		})
	}
}
