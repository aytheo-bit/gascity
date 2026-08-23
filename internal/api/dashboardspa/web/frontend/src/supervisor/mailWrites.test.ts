import { afterEach, describe, expect, it, vi } from 'vitest';
import { resetSupervisorApiForTests, setSupervisorApiForTests, type SupervisorApi } from './client';
import { replySupervisorMail, sendSupervisorMail } from './mailWrites';

// A reply through the dashboard was — until notify existed — the ONLY way to
// answer a session's mail with no way to wake it: the reply persisted as an
// unread bead and nothing else, so the recipient could sit idle indefinitely
// unaware an answer had arrived ("Dashboard replies don't wake anyone").
// These are planted-proof tests for the fix: a reply defaults to notify:true,
// a fresh compose does not (matching the CLI's own opt-in default for send),
// and either can still be overridden explicitly.

const baseApi: SupervisorApi = {
  baseUrl: '/gc-supervisor',
  health: vi.fn(),
  cityHealth: vi.fn(),
  cityStatus: vi.fn(),
  cityUsage: vi.fn(),
  runCensus: vi.fn(),
  listCities: vi.fn(),
  listAgents: vi.fn(),
  listRigs: vi.fn(),
  listBeads: vi.fn(),
  listEvents: vi.fn(),
  getBead: vi.fn(),
  createBead: vi.fn(),
  updateBead: vi.fn(),
  closeBead: vi.fn(),
  sling: vi.fn(),
  formulaFeed: vi.fn(),
  listMail: vi.fn(),
  markMailRead: vi.fn(),
  markMailUnread: vi.fn(),
  archiveMail: vi.fn(),
  replyMail: vi.fn(),
  sendMail: vi.fn(),
  mailThread: vi.fn(),
  cityEventStreamUrl: vi.fn(),
  sessionStreamUrl: vi.fn(),
  listSessions: vi.fn(),
  sessionPending: vi.fn(),
  respondSession: vi.fn(),
  sessionTranscript: vi.fn(),
  workflowRun: vi.fn(),
  formulaDetail: vi.fn(),
  mutationHeaders: () => ({ 'X-GC-Request': 'dashboard' }),
};

describe('supervisor mail writes: notify', () => {
  afterEach(() => {
    resetSupervisorApiForTests();
  });

  it('PLANTED: replySupervisorMail defaults to notify:true when the draft omits it', async () => {
    const replyMail = vi.fn(async () => undefined);
    setSupervisorApiForTests({ ...baseApi, replyMail });

    await replySupervisorMail({ id: 'msg-1' }, { body: 'got it' }, 'human');

    expect(replyMail).toHaveBeenCalledWith(
      'test-city',
      'msg-1',
      { body: 'got it', from: 'human', notify: true },
      undefined,
    );
  });

  it('PLANTED: replySupervisorMail honors an explicit notify:false override', async () => {
    const replyMail = vi.fn(async () => undefined);
    setSupervisorApiForTests({ ...baseApi, replyMail });

    await replySupervisorMail({ id: 'msg-1' }, { body: 'got it', notify: false }, 'human');

    expect(replyMail).toHaveBeenCalledWith(
      'test-city',
      'msg-1',
      { body: 'got it', from: 'human', notify: false },
      undefined,
    );
  });

  it('sendSupervisorMail does not set notify unless the caller opts in (matches gc mail send default)', async () => {
    const sendMail = vi.fn(async () => undefined);
    setSupervisorApiForTests({ ...baseApi, sendMail });

    await sendSupervisorMail({ to: 'worker', subject: 'hi', body: 'hello' }, 'human');

    expect(sendMail).toHaveBeenCalledWith('test-city', {
      to: 'worker',
      subject: 'hi',
      body: 'hello',
      from: 'human',
    });
  });

  it('sendSupervisorMail passes an explicit notify:true through', async () => {
    const sendMail = vi.fn(async () => undefined);
    setSupervisorApiForTests({ ...baseApi, sendMail });

    await sendSupervisorMail({ to: 'worker', subject: 'hi', body: 'hello', notify: true }, 'human');

    expect(sendMail).toHaveBeenCalledWith('test-city', {
      to: 'worker',
      subject: 'hi',
      body: 'hello',
      notify: true,
      from: 'human',
    });
  });
});
