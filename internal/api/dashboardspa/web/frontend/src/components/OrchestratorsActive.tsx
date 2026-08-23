import { useMemo, useState } from 'react';
import type { SupervisorSession } from '../supervisor/sessionReads';
import { useNow } from '../contexts/NowContext';
import { formatRelative } from '../hooks/time';
import {
  deriveActiveOrchestrators,
  summarizeActiveOrchestrators,
  type ActiveOrchestrator,
} from '../hooks/activeOrchestrators';
import { Button } from './Button';
import { Modal } from './Modal';
import { LiveSessionPeek } from './LiveSessionPeek';
import { StatusBadge, stateTone } from './StatusBadge';

// "Orchestrators active" — the positive-display counterpart to Workers
// active. Workers active explicitly EXCLUDES orchestration sessions (mayor,
// a tier0-/tier1-/tier2- charter): they direct work, they don't perform it.
// Until this component existed, that meant an orchestrator session was
// invisible everywhere in the dashboard — this is that missing surface.
//
// Deliberately mirrors WorkInFlight's shape (same summary-line + row-list +
// peek-modal pattern) minus the bead-joining, which doesn't apply here: an
// orchestrator directs work across many beads, not one.

interface OrchestratorsActiveProps {
  sessions: readonly SupervisorSession[];
  sessionsLoading: boolean;
  sessionsError: string | null;
}

function OrchestratorRow({
  orchestrator,
  accent,
  onPeek,
}: {
  orchestrator: ActiveOrchestrator;
  accent: boolean;
  onPeek: (sessionId: string) => void;
}) {
  const now = useNow();
  const { session, label } = orchestrator;
  // One Mark Rule (mirrors WorkerRow): only the first stuck/failed
  // orchestrator renders its state badge in tone; every other row reads
  // neutral so at most one accent mark appears per viewport.
  const tone = accent ? stateTone(session.state) : 'neutral';
  return (
    <li className="px-2 py-2 -mx-2 rounded-sm transition-colors duration-150 ease-out-quart hover:bg-surface-tint/60">
      <div className="flex items-baseline justify-between gap-4">
        <div className="min-w-0 text-body text-fg">
          <button
            type="button"
            onClick={() => onPeek(session.id)}
            className="group text-left cursor-pointer focus-mark"
            title={`Open ${label} transcript`}
          >
            <span className="font-medium group-hover:text-accent">{label}</span>
          </button>
        </div>
        <div className="flex items-baseline gap-3 shrink-0">
          <StatusBadge tone={tone} label={session.state} />
          <span className="tnum text-fg-muted w-10 text-right">
            {formatRelative(session.last_active, now)}
          </span>
          <Button size="sm" tone="quiet" onClick={() => onPeek(session.id)}>
            Peek
          </Button>
        </div>
      </div>
    </li>
  );
}

function isOrchestratorStreamable(session: SupervisorSession): boolean {
  return session.running === true || session.state === 'active' || session.state === 'running';
}

export function OrchestratorsActive({
  sessions,
  sessionsLoading,
  sessionsError,
}: OrchestratorsActiveProps) {
  const active = useMemo(() => deriveActiveOrchestrators(sessions), [sessions]);
  const summary = useMemo(() => summarizeActiveOrchestrators(active), [active]);

  const [peekSessionId, setPeekSessionId] = useState<string | null>(null);
  const peekOrchestrator = useMemo(
    () =>
      peekSessionId
        ? (active.orchestrators.find((o) => o.session.id === peekSessionId) ?? null)
        : null,
    [active.orchestrators, peekSessionId],
  );

  const accentIndex = useMemo(
    () => active.orchestrators.findIndex((o) => stateTone(o.session.state) === 'stuck'),
    [active.orchestrators],
  );

  // Same fail-safe pattern as Workers active: an honest all-clear requires the
  // sessions fetch to have actually delivered, never a silent "nothing here"
  // that could mask a dead aggregator.
  const sessionsAbsent = sessions.length === 0;
  const sessionsUnavailable = sessionsError !== null && sessionsAbsent;
  const sessionsPending = sessionsLoading && sessionsAbsent;
  const countLabel = sessionsUnavailable || sessionsPending ? '—' : active.total;

  return (
    <section className="mb-10" aria-label="Orchestrators active">
      <header className="flex items-baseline justify-between border-b border-rule pb-2 mb-4">
        <h2 className="text-headline text-fg">Orchestrators active</h2>
        <span className="text-label tnum text-fg-muted">{countLabel}</span>
      </header>
      {sessionsUnavailable ? (
        <p className="text-body text-fg-muted" role="status">
          Orchestrator status unavailable.
        </p>
      ) : sessionsPending ? (
        <p className="text-body text-fg-muted" role="status">
          Checking orchestrator status…
        </p>
      ) : active.total === 0 ? (
        <p className="text-body text-fg-muted">No orchestrators active right now.</p>
      ) : (
        <>
          <p className="text-body text-fg-muted mb-4">{summary}</p>
          <ul className="space-y-1">
            {active.orchestrators.map((orchestrator, i) => (
              <OrchestratorRow
                key={orchestrator.session.id}
                orchestrator={orchestrator}
                accent={i === accentIndex}
                onPeek={setPeekSessionId}
              />
            ))}
          </ul>
        </>
      )}

      <Modal
        open={peekOrchestrator !== null}
        onClose={() => setPeekSessionId(null)}
        title={peekOrchestrator ? peekOrchestrator.label : 'Transcript'}
        caption="Live transcript from the supervisor's session stream."
        widthClass="max-w-5xl"
      >
        <LiveSessionPeek
          sessionId={peekSessionId}
          stream={peekOrchestrator ? isOrchestratorStreamable(peekOrchestrator.session) : false}
          showBadge
          showCaption
        />
      </Modal>
    </section>
  );
}
