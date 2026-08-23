import { describe, expect, it } from 'vitest';
import type { SupervisorSession } from '../supervisor/sessionReads';
import { deriveActiveOrchestrators, summarizeActiveOrchestrators } from './activeOrchestrators';

// "Orchestrators active" is the positive-display counterpart to Workers
// active: the same session data, but INCLUDING cross-rig orchestration
// sessions (mayor, tier0-/tier1-/tier2- charters) instead of excluding them.
// Before this module existed, an orchestrator session was invisible in the
// dashboard everywhere — this is the missing surface, not a bug fix.

function session(partial: Partial<SupervisorSession> & { id: string }): SupervisorSession {
  return {
    template: 'polecat',
    session_name: partial.id,
    title: partial.id,
    state: 'active',
    created_at: '2026-06-03T00:00:00Z',
    attached: false,
    running: true,
    provider: 'claude',
    ...partial,
  } as SupervisorSession;
}

describe('deriveActiveOrchestrators', () => {
  it('includes cross-rig orchestration sessions: mayor and tier-numbered charters', () => {
    const sessions = [
      session({ id: 'kg-xqh', template: 'mayor', rig: '', title: 'mayor' }),
      session({
        id: 'kna-gy9em2',
        template: 'tier0-a2-dictionary',
        rig: '',
        title: 'Tier 0: A2 Dictionary gate',
      }),
      session({
        id: 'kg-xnzk5x',
        template: 'tier1-linear-closeout',
        rig: '',
        title: 'Close hchf+om6i',
      }),
    ];
    const result = deriveActiveOrchestrators(sessions);
    expect(result.total).toBe(3);
    expect(result.orchestrators.map((o) => o.session.id).sort()).toEqual(
      ['kg-xnzk5x', 'kg-xqh', 'kna-gy9em2'].sort(),
    );
  });

  it('excludes ordinary worker sessions', () => {
    const sessions = [
      session({ id: 'gc-1', template: 'polecat', rig: '/home/ds/gascity' }),
      session({ id: 'gc-2', template: 'scix-worker', rig: 'scix_experiments' }),
    ];
    const result = deriveActiveOrchestrators(sessions);
    expect(result.total).toBe(0);
  });

  it('excludes a rig-scoped session even if its template happens to start with "tier"', () => {
    // isOrchestrationSession is cross-rig by definition (s.rig must be empty).
    // A per-rig session is shown inline within its own rig, not here.
    const sessions = [session({ id: 'gc-1', template: 'tier0-worker', rig: 'gascity' })];
    const result = deriveActiveOrchestrators(sessions);
    expect(result.total).toBe(0);
  });

  it('excludes suspended / non-active orchestrator sessions', () => {
    const sessions = [
      session({ id: 'kg-xqh', template: 'mayor', rig: '', state: 'active' }),
      session({ id: 'kg-old', template: 'tier0-old-charter', rig: '', state: 'closed' }),
    ];
    const result = deriveActiveOrchestrators(sessions);
    expect(result.total).toBe(1);
    expect(result.orchestrators[0]?.session.id).toBe('kg-xqh');
  });

  it('sorts most-recently-active first', () => {
    const sessions = [
      session({ id: 'a', template: 'mayor', rig: '', last_active: '2026-06-03T00:00:00Z' }),
      session({
        id: 'b',
        template: 'tier0-a2-dictionary',
        rig: '',
        last_active: '2026-06-03T00:05:00Z',
      }),
    ];
    const result = deriveActiveOrchestrators(sessions);
    expect(result.orchestrators.map((o) => o.session.id)).toEqual(['b', 'a']);
  });

  it('labels a row by title, falling back to alias then session_name then id', () => {
    const sessions = [
      session({ id: 'a', template: 'mayor', rig: '', title: 'mayor' }),
      session({ id: 'b', template: 'tier0-x', rig: '', title: '', alias: 'tier0-a2' }),
      session({ id: 'c', template: 'tier0-y', rig: '', title: '', session_name: 's-tier0-y' }),
    ];
    const result = deriveActiveOrchestrators(sessions);
    const byId = Object.fromEntries(result.orchestrators.map((o) => [o.session.id, o.label]));
    expect(byId['a']).toBe('mayor');
    expect(byId['b']).toBe('tier0-a2');
    expect(byId['c']).toBe('s-tier0-y');
  });
});

describe('summarizeActiveOrchestrators', () => {
  it('returns the all-clear sentence when none are active', () => {
    expect(summarizeActiveOrchestrators({ orchestrators: [], total: 0 })).toBe(
      'No orchestrators active right now.',
    );
  });

  it('lists active orchestrators by label', () => {
    const summary = summarizeActiveOrchestrators({
      orchestrators: [
        { session: session({ id: 'a', title: 'mayor' }), label: 'mayor' },
        { session: session({ id: 'b', title: 'tier0-a2' }), label: 'tier0-a2' },
      ],
      total: 2,
    });
    expect(summary).toBe('2 orchestrators active: mayor, tier0-a2.');
  });

  it('uses singular noun for exactly one', () => {
    const summary = summarizeActiveOrchestrators({
      orchestrators: [{ session: session({ id: 'a', title: 'mayor' }), label: 'mayor' }],
      total: 1,
    });
    expect(summary).toBe('1 orchestrator active: mayor.');
  });
});
