import type { SupervisorSession } from '../supervisor/sessionReads';
import { isOrchestrationSession } from './projectOf';

// Session-driven "Orchestrators active" derivation — the positive-display
// counterpart to Workers active (activeWorkers.ts). Workers active explicitly
// EXCLUDES orchestration sessions by design (mayor, a Tier 0/1/2 charter, ...
// direct work, they don't perform it), but until this module existed there
// was no OTHER surface that showed them either — an orchestrator session was
// simply invisible everywhere in the dashboard. This is that surface.
//
// Deliberately simpler than deriveActiveWorkers: an orchestrator session has
// no natural "assigned bead" the way a worker does (it directs work across
// many beads, not one), so there is no bead-joining step here.

/** A live orchestrator session (mayor, or a tier0-/tier1-/tier2- charter). */
export interface ActiveOrchestrator {
  session: SupervisorSession;
  /** Display name: the session's title if set, else its alias, else its id. */
  label: string;
}

export interface ActiveOrchestrators {
  /** Rows, most-recently-active first. */
  orchestrators: ActiveOrchestrator[];
  total: number;
}

function activityKey(o: ActiveOrchestrator): number {
  const ms = o.session.last_active ? Date.parse(o.session.last_active) : NaN;
  return Number.isFinite(ms) ? ms : 0;
}

function orchestratorLabel(session: SupervisorSession): string {
  return session.title.trim() || session.alias?.trim() || session.session_name || session.id;
}

/**
 * Derive the active-orchestrator rows from the live sessions.
 *
 * Steps (mechanical, no semantic judgment):
 *  1. Keep only active orchestration sessions (isOrchestrationSession — the
 *     same classifier Workers active uses to EXCLUDE, used here to INCLUDE).
 *  2. Sort by most-recent activity.
 */
export function deriveActiveOrchestrators(
  sessions: readonly SupervisorSession[],
): ActiveOrchestrators {
  const orchestrators: ActiveOrchestrator[] = [];
  for (const session of sessions) {
    if (session.state !== 'active' && session.state !== 'running') continue;
    if (!isOrchestrationSession(session)) continue;
    orchestrators.push({ session, label: orchestratorLabel(session) });
  }

  orchestrators.sort((a, b) => activityKey(b) - activityKey(a));

  return { orchestrators, total: orchestrators.length };
}

/**
 * The calm one-line summary, e.g. "2 orchestrators active: mayor, tier0-a2".
 * Returns the empty-state sentence when none are active.
 */
export function summarizeActiveOrchestrators(orchestrators: ActiveOrchestrators): string {
  if (orchestrators.total === 0) return 'No orchestrators active right now.';
  const noun = orchestrators.total === 1 ? 'orchestrator' : 'orchestrators';
  const names = orchestrators.orchestrators.map((o) => o.label).join(', ');
  return `${orchestrators.total} ${noun} active: ${names}.`;
}
