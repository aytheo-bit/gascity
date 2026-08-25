import type { StatusTone } from '../components/StatusBadge';

// gascity-dashboard: an honest, classification-independent busy/idle signal
// for a live session.
//
// Two purpose-built displays already read session state, but neither answers
// "is this session actually doing anything right now":
//
//  - StatusBadge's `stateTone`/`state` column answers "is the process alive"
//    (active/running vs asleep/detached/closed) — a liveness fact, not a
//    progress fact. A session can be state=active for hours while doing
//    nothing.
//  - OrchestratorsActive renders `session.state` as its badge label and a
//    muted last-active relative time, but never reads `session.activity` (the
//    supervisor's own transcript-tail inference of in-turn vs idle) — so every
//    row that isn't literally `stuck` reads as a flat, identical "· active"
//    regardless of whether it produced output 5 seconds or 25 minutes ago.
//
// This module is the fix: a single derivation, usable by any session-list
// surface, that answers the progress question honestly using the best
// available signal and degrades gracefully when that signal is absent (as it
// currently is on cities where the sessions LIST endpoint's transcript
// enrichment can't attribute a shared work_dir to one session — see
// gascity-dashboard-sessions-view PR description for the backend gap this
// does NOT attempt to fix).

export const SESSION_PROGRESS_WARNING_MS = 5 * 60_000;
export const SESSION_PROGRESS_STALLED_MS = 30 * 60_000;

export type SessionProgressKind = 'in-turn' | 'fresh' | 'warning' | 'stalled' | 'unknown';

export interface SessionProgress {
  kind: SessionProgressKind;
  tone: StatusTone;
  label: string;
}

export interface SessionProgressInput {
  /** Coarse activity hint from the supervisor: 'idle' | 'in-turn' | ... */
  activity?: string;
  last_active?: string;
}

/**
 * Derive a session's live-progress signal.
 *
 * Precedence:
 *  1. `activity === 'in-turn'` — the supervisor's transcript tail shows the
 *     session mid-turn right now. This is trusted unconditionally: it is a
 *     direct read of live work, not an inference from elapsed time.
 *  2. Otherwise (activity is 'idle', unset, or any other value), bucket by
 *     the age of `last_active` into the same fresh/warning/stalled tiers
 *     `useStaleness.ts` uses for Run lanes (5m / 30m), so "how long has this
 *     been quiet" reads with one shared vocabulary across the dashboard.
 *     `activity: 'idle'` alone does NOT mean stalled — a session that just
 *     finished a turn a second ago is idle by that field but still fresh by
 *     age, and the two should not be conflated into a false "stuck" read.
 *  3. No parseable `last_active` and no in-turn signal — 'unknown', not a
 *     silent default to any other tier: the dashboard has no operator-visible
 *     fact to bucket, and pretending otherwise would be a false all-clear (or
 *     false alarm).
 */
export function deriveSessionProgress(
  session: SessionProgressInput,
  nowMs: number,
): SessionProgress {
  if (session.activity === 'in-turn') {
    return { kind: 'in-turn', tone: 'ok', label: 'in-turn' };
  }

  if (!session.last_active) {
    return { kind: 'unknown', tone: 'neutral', label: 'unknown' };
  }
  const lastActiveMs = Date.parse(session.last_active);
  if (!Number.isFinite(lastActiveMs)) {
    return { kind: 'unknown', tone: 'neutral', label: 'unknown' };
  }

  const ageMs = Math.max(0, nowMs - lastActiveMs);
  if (ageMs >= SESSION_PROGRESS_STALLED_MS) {
    return { kind: 'stalled', tone: 'stuck', label: 'stalled' };
  }
  if (ageMs >= SESSION_PROGRESS_WARNING_MS) {
    return { kind: 'warning', tone: 'warn', label: 'quiet' };
  }
  return { kind: 'fresh', tone: 'ok', label: 'active' };
}
