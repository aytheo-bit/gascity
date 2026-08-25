import { describe, expect, it } from 'vitest';
import {
  deriveSessionProgress,
  SESSION_PROGRESS_STALLED_MS,
  SESSION_PROGRESS_WARNING_MS,
} from './sessionProgress';

const NOW = Date.parse('2026-08-25T12:00:00Z');

function agoIso(ms: number): string {
  return new Date(NOW - ms).toISOString();
}

describe('deriveSessionProgress', () => {
  it('trusts activity="in-turn" unconditionally, even over a stale last_active', () => {
    // A session mid-turn right now is live work, regardless of when
    // last_active was last stamped (it may lag the in-flight turn).
    const result = deriveSessionProgress(
      { activity: 'in-turn', last_active: agoIso(40 * 60_000) },
      NOW,
    );
    expect(result.kind).toBe('in-turn');
    expect(result.tone).toBe('ok');
  });

  it('does not treat activity="idle" as an override — falls through to last_active age', () => {
    const result = deriveSessionProgress({ activity: 'idle', last_active: agoIso(60_000) }, NOW);
    expect(result.kind).toBe('fresh');
  });

  it('buckets a fresh last_active (<5m) as fresh/ok', () => {
    const result = deriveSessionProgress({ last_active: agoIso(30_000) }, NOW);
    expect(result.kind).toBe('fresh');
    expect(result.tone).toBe('ok');
  });

  it('buckets 5m-30m as warning/quiet', () => {
    const result = deriveSessionProgress({ last_active: agoIso(10 * 60_000) }, NOW);
    expect(result.kind).toBe('warning');
    expect(result.tone).toBe('warn');
    expect(result.label).toBe('quiet');
  });

  it('buckets >=30m as stalled/stuck', () => {
    const result = deriveSessionProgress({ last_active: agoIso(45 * 60_000) }, NOW);
    expect(result.kind).toBe('stalled');
    expect(result.tone).toBe('stuck');
  });

  it('is unknown with no last_active and no in-turn activity', () => {
    const result = deriveSessionProgress({}, NOW);
    expect(result.kind).toBe('unknown');
    expect(result.tone).toBe('neutral');
  });

  it('is unknown with an unparsable last_active rather than silently defaulting to fresh', () => {
    const result = deriveSessionProgress({ last_active: 'not-a-timestamp' }, NOW);
    expect(result.kind).toBe('unknown');
  });

  it('boundary: exactly at the warning threshold reads warning, not fresh', () => {
    const result = deriveSessionProgress({ last_active: agoIso(SESSION_PROGRESS_WARNING_MS) }, NOW);
    expect(result.kind).toBe('warning');
  });

  it('boundary: exactly at the stalled threshold reads stalled, not warning', () => {
    const result = deriveSessionProgress({ last_active: agoIso(SESSION_PROGRESS_STALLED_MS) }, NOW);
    expect(result.kind).toBe('stalled');
  });
});
