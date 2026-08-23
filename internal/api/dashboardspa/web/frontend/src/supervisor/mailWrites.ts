import { activeCityOrThrow } from '../api/cityBase';
import { supervisorApi } from './client';

export interface MailComposeDraft {
  to: string;
  subject: string;
  body: string;
  // Best-effort nudge the recipient's live session after sending, mirroring
  // `gc mail send --notify`. Optional and unset by default for a fresh
  // compose (matching the CLI's own opt-in default) -- see MailReplyDraft's
  // notify for why a REPLY defaults the opposite way.
  notify?: boolean;
}

export interface MailReplyDraft {
  body: string;
  // Defaults to true in replySupervisorMail below. Unlike a fresh compose,
  // a reply through the dashboard was — until this field existed — the
  // ONLY way to answer a session's mail with no way to wake it: the reply
  // persisted as an unread bead and nothing else, so the recipient could
  // sit idle indefinitely unaware an answer had arrived (see "Dashboard
  // replies don't wake anyone" in the project's own incident record). A
  // reply is, by definition, answering something the recipient is already
  // waiting on -- silently not-notifying should be the deliberate
  // exception, not the default.
  notify?: boolean;
}

export interface MailActionTarget {
  id: string;
  rig?: string;
}

export async function sendSupervisorMail(
  draft: MailComposeDraft,
  operatorWireAlias: string,
): Promise<void> {
  await supervisorApi().sendMail(activeCityOrThrow('send supervisor mail'), {
    ...draft,
    from: operatorWireAlias,
  });
}

export async function markSupervisorMailRead(message: MailActionTarget): Promise<void> {
  await supervisorApi().markMailRead(
    activeCityOrThrow('mark supervisor mail read'),
    message.id,
    mailActionQuery(message),
  );
}

export async function markSupervisorMailUnread(message: MailActionTarget): Promise<void> {
  await supervisorApi().markMailUnread(
    activeCityOrThrow('mark supervisor mail unread'),
    message.id,
    mailActionQuery(message),
  );
}

export async function archiveSupervisorMail(message: MailActionTarget): Promise<void> {
  await supervisorApi().archiveMail(
    activeCityOrThrow('archive supervisor mail'),
    message.id,
    mailActionQuery(message),
  );
}

export async function replySupervisorMail(
  message: MailActionTarget,
  draft: MailReplyDraft,
  operatorWireAlias: string,
): Promise<void> {
  await supervisorApi().replyMail(
    activeCityOrThrow('reply supervisor mail'),
    message.id,
    {
      notify: true,
      ...draft,
      from: operatorWireAlias,
    },
    mailActionQuery(message),
  );
}

function mailActionQuery(message: MailActionTarget): { rig: string } | undefined {
  return message.rig === undefined || message.rig.length === 0 ? undefined : { rig: message.rig };
}
