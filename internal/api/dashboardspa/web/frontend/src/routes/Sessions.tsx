import { useMemo, useState } from 'react';
import { GC_EVENT_PREFIX } from 'gas-city-dashboard-shared';
import { Button } from '../components/Button';
import { ListSearchBar } from '../components/ListSearchBar';
import { Modal } from '../components/Modal';
import { PageHeader } from '../components/PageHeader';
import { LiveSessionPeek } from '../components/LiveSessionPeek';
import { SseIndicator } from '../components/SseIndicator';
import { StatusBadge, stateTone } from '../components/StatusBadge';
import { Table, type TableColumn } from '../components/Table';
import { useNow } from '../contexts/NowContext';
import { useCachedData } from '../hooks/useCachedData';
import { useGcEventRefresh } from '../hooks/useGcEvents';
import { formatRelative } from '../hooks/time';
import { deriveSessionProgress } from '../hooks/sessionProgress';
import { listSupervisorSessions, type SupervisorSession } from '../supervisor/sessionReads';

// gascity-dashboard-sessions-view: the single place that lists every live
// session, unfiltered by role.
//
// Before this page, a session was only visible if it happened to match one of
// two closed classifiers: "Workers active" (name ends in worker/polecat) or
// "Orchestrators active" (mayor / a tier<N>- prefixed template). A session
// that matches neither — e.g. a one-off Tier-2 execution lane or a recon lane
// named for its task rather than its role — is invisible in both sections and
// absent from the Agents roster too (that roster is agent-config-driven, not
// session-driven, so an ad hoc `gc session create` charter never appears
// there either). This page has no such gate: every row from the sessions API
// renders, labeled and peekable, regardless of naming convention.
//
// It also answers "is this actually doing anything" directly (deriveSessionProgress)
// instead of only showing `state` (a liveness fact — the process is alive —
// not a progress fact), so a session that has been state=active but silent for
// 25 minutes reads differently from one that produced output 10 seconds ago.

function sessionLabel(s: SupervisorSession): string {
  return s.title.trim() || s.alias?.trim() || s.session_name || s.id;
}

function isSessionStreamable(s: SupervisorSession | null): boolean {
  if (!s) return false;
  return s.running === true || s.state === 'active' || s.state === 'running';
}

const SESSION_SEARCH_FIELDS = (s: SupervisorSession): ReadonlyArray<string> =>
  [s.title, s.alias, s.session_name, s.template, s.rig, s.pool, s.provider].filter(
    (field): field is string => typeof field === 'string' && field.length > 0,
  );

/** Calm one-line synopsis: total live sessions, broken down by progress kind. */
export function buildSessionsSynopsis(
  sessions: readonly SupervisorSession[],
  nowMs: number,
): string {
  if (sessions.length === 0) return 'No live sessions.';
  const counts = new Map<string, number>();
  for (const s of sessions) {
    const kind = deriveSessionProgress(s, nowMs).kind;
    counts.set(kind, (counts.get(kind) ?? 0) + 1);
  }
  const parts: string[] = [];
  const inTurn = counts.get('in-turn') ?? 0;
  const fresh = counts.get('fresh') ?? 0;
  const warning = counts.get('warning') ?? 0;
  const stalled = counts.get('stalled') ?? 0;
  const unknown = counts.get('unknown') ?? 0;
  if (inTurn > 0) parts.push(`${inTurn} in-turn`);
  if (fresh > 0) parts.push(`${fresh} active`);
  if (warning > 0) parts.push(`${warning} quiet`);
  if (stalled > 0) parts.push(`${stalled} stalled`);
  if (unknown > 0) parts.push(`${unknown} unknown`);
  const noun = sessions.length === 1 ? 'session' : 'sessions';
  return `${sessions.length} live ${noun}: ${parts.join(', ')}.`;
}

export function SessionsPage() {
  const { data, loading, error, refresh } = useCachedData('sessions', listSupervisorSessions);
  const rows = useMemo<SupervisorSession[]>(() => data?.items ?? [], [data]);
  const now = useNow();
  const [search, setSearch] = useState('');
  const [peekSessionId, setPeekSessionId] = useState<string | null>(null);

  const sseState = useGcEventRefresh([GC_EVENT_PREFIX.session], () => {
    void refresh();
  });

  const visibleRows = useMemo(() => {
    const q = search.trim().toLowerCase();
    if (q.length === 0) return rows;
    return rows.filter((s) =>
      SESSION_SEARCH_FIELDS(s).some((field) => field.toLowerCase().includes(q)),
    );
  }, [rows, search]);

  const peekSession = useMemo(
    () => (peekSessionId === null ? null : (rows.find((s) => s.id === peekSessionId) ?? null)),
    [rows, peekSessionId],
  );

  const synopsis = useMemo(() => buildSessionsSynopsis(rows, now), [rows, now]);
  const sessionsUnavailable = error !== null && rows.length === 0;
  const emptyMessage = sessionsUnavailable
    ? 'Session list unavailable.'
    : rows.length === 0
      ? 'No live sessions.'
      : 'No sessions match the current search.';

  const columns = useMemo<ReadonlyArray<TableColumn<SupervisorSession>>>(
    () => [
      {
        key: 'label',
        label: 'Session',
        sortable: true,
        sortValue: (r) => sessionLabel(r),
        render: (r) => (
          <button
            type="button"
            onClick={() => setPeekSessionId(r.id)}
            className="group text-left cursor-pointer focus-mark"
            title={`Open ${sessionLabel(r)} transcript`}
          >
            <span className="font-medium text-fg group-hover:text-accent">{sessionLabel(r)}</span>
          </button>
        ),
      },
      {
        key: 'state',
        label: 'State',
        sortable: true,
        sortValue: (r) => r.state,
        render: (r) => <StatusBadge tone={stateTone(r.state)} label={r.state} />,
        className: 'w-28',
      },
      {
        key: 'progress',
        label: 'Progress',
        sortable: true,
        sortValue: (r) => deriveSessionProgress(r, now).kind,
        render: (r) => {
          const progress = deriveSessionProgress(r, now);
          return <StatusBadge tone={progress.tone} label={progress.label} />;
        },
        className: 'w-28',
      },
      {
        key: 'last_active',
        label: 'Last active',
        sortable: true,
        sortValue: (r) => (r.last_active ? Date.parse(r.last_active) : 0),
        render: (r) => (
          <span className="tnum text-fg-muted">{formatRelative(r.last_active, now)}</span>
        ),
        className: 'w-28',
      },
      {
        key: 'actions',
        label: '',
        render: (r) => (
          <div className="flex justify-end">
            <Button size="sm" tone="quiet" onClick={() => setPeekSessionId(r.id)}>
              Peek
            </Button>
          </div>
        ),
        align: 'right',
        className: 'w-24',
      },
    ],
    [now],
  );

  return (
    <section>
      <PageHeader
        title="Sessions"
        synopsis={sessionsUnavailable ? 'Session list unavailable.' : synopsis}
        meta={
          <>
            <SseIndicator state={sseState} />
            {error && (
              <span className="normal-case text-body text-accent" role="alert">
                {error}
              </span>
            )}
            <Button size="sm" onClick={() => void refresh()} disabled={loading}>
              {loading ? 'Refreshing' : 'Refresh'}
            </Button>
          </>
        }
      />

      <div className="mb-6">
        <ListSearchBar
          value={search}
          onChange={setSearch}
          placeholder="Search sessions by title, alias, template, rig, provider"
          matchCount={visibleRows.length}
          totalCount={rows.length}
          ariaLabel="Search sessions"
        />
      </div>

      <Table
        rows={visibleRows}
        columns={columns}
        rowKey={(r) => r.id}
        empty={emptyMessage}
        initialSort={{ key: 'last_active', dir: 'desc' }}
      />

      <Modal
        open={peekSessionId !== null}
        onClose={() => setPeekSessionId(null)}
        title={peekSession ? sessionLabel(peekSession) : 'Transcript'}
        caption="Live transcript from the supervisor's session stream."
        widthClass="max-w-5xl"
      >
        <LiveSessionPeek
          sessionId={peekSessionId}
          stream={isSessionStreamable(peekSession)}
          showBadge
          showCaption
        />
      </Modal>
    </section>
  );
}
