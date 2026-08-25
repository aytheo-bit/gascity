import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { SessionsPage } from './Sessions';
import { invalidate } from '../api/cache';
import { NowProvider } from '../contexts/NowContext';
import { resetSupervisorApiForTests } from '../supervisor/client';

// Regression coverage for the "invisible session" gap this page fixes: before
// it existed, a live session was only ever shown if its name matched the
// worker-role suffix regex ("Workers active") or the mayor/tier<N>- prefix set
// ("Orchestrators active" on the Agents page). A session like `a2-linux-port`
// (a real Tier-2 execution lane, named for its task rather than a role)
// matched neither and was dashboard-invisible. This page has no such gate —
// every row from `/sessions` renders.

function jsonResponse(payload: unknown, init?: ResponseInit): Response {
  return new Response(JSON.stringify(payload), {
    status: init?.status ?? 200,
    headers: { 'content-type': 'application/json' },
  });
}

function requestUrl(input: RequestInfo | URL): string {
  const url =
    input instanceof Request ? input.url : input instanceof URL ? input.toString() : String(input);
  const origin = window.location.origin;
  return url.startsWith(origin) ? url.slice(origin.length) : url;
}

// NowProvider always seeds from the real Date.now() (no test override), so
// fixtures use offsets from the actual test-run time rather than fixed ISO
// literals — pinning a literal "now" would make the stalled/fresh assertions
// depend on when the suite happens to run.
const NOW_MS = Date.now();
function agoIso(ms: number): string {
  return new Date(NOW_MS - ms).toISOString();
}

const SESSIONS_PAYLOAD = {
  items: [
    {
      id: 'kna-wisp-aszhom',
      session_name: 's-kna-wisp-aszhom',
      state: 'active',
      // Deliberately matches neither the worker-suffix regex (does not end in
      // worker/polecat) nor the tier<N>-/mayor orchestration set — the exact
      // shape that was invisible everywhere before this page existed.
      template: 'a2-linux-port',
      alias: 'a2-linux-port',
      title: 'Tier 2: A2 ContentBuild Linux port execution',
      provider: 'claude',
      running: true,
      attached: false,
      created_at: '2026-08-24T17:38:54Z',
      last_active: agoIso(5_000),
      activity: 'in-turn',
    },
    {
      id: 'kna-rc5',
      session_name: 'mayor',
      state: 'active',
      template: 'mayor',
      alias: 'mayor',
      title: 'mayor',
      provider: 'claude',
      running: true,
      attached: false,
      created_at: '2026-08-22T21:52:58Z',
      // 40 minutes before "now" — stalled by age, with no in-turn activity
      // signal to override it.
      last_active: agoIso(40 * 60_000),
    },
  ],
  total: 2,
};

function stubFetch() {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = requestUrl(input);
      if (url.startsWith('/v0/city/test-city/sessions')) {
        return jsonResponse(SESSIONS_PAYLOAD);
      }
      throw new Error(`unexpected fetch: ${url}`);
    }),
  );
  resetSupervisorApiForTests();
}

beforeEach(() => {
  invalidate('sessions');
  stubFetch();
});

afterEach(() => {
  cleanup();
  resetSupervisorApiForTests();
  vi.unstubAllGlobals();
});

describe('SessionsPage', () => {
  it('renders every live session regardless of naming convention, including one that matches no worker/orchestrator classifier', async () => {
    render(
      <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
        <NowProvider intervalMs={1_000_000}>
          <SessionsPage />
        </NowProvider>
      </MemoryRouter>,
    );

    expect(await screen.findByText('Tier 2: A2 ContentBuild Linux port execution')).toBeTruthy();
    expect(screen.getByText('mayor')).toBeTruthy();
  });

  it('shows an in-turn session as genuinely progressing, distinct from a stalled one', async () => {
    render(
      <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
        <NowProvider intervalMs={1_000_000}>
          <SessionsPage />
        </NowProvider>
      </MemoryRouter>,
    );

    await screen.findByText('Tier 2: A2 ContentBuild Linux port execution');

    // The a2-linux-port row carries a live in-turn signal.
    expect(screen.getByText('in-turn')).toBeTruthy();
    // mayor's last_active is 40 minutes stale with no in-turn override —
    // it must NOT read the same as the genuinely active row.
    expect(screen.getByText('stalled')).toBeTruthy();
  });

  it('filters by search across title/alias/template', async () => {
    render(
      <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
        <NowProvider intervalMs={1_000_000}>
          <SessionsPage />
        </NowProvider>
      </MemoryRouter>,
    );

    await screen.findByText('mayor');
    const search = screen.getByRole('searchbox', { name: /search sessions/i });
    fireEvent.change(search, { target: { value: 'linux-port' } });

    expect(screen.queryByText('mayor')).toBeNull();
    expect(screen.getByText('Tier 2: A2 ContentBuild Linux port execution')).toBeTruthy();
  });
});
