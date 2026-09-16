---
title: "Command Execution Trust Boundaries"
---

Gas City intentionally runs operator-configured commands. Those commands are a
feature, not a sandbox. Treat city config, imported packs, exec provider
scripts, and agent startup commands as trusted code with the same review
expectations as shell scripts committed to the repository.

## Trust Model

| Input | Trust level | Rule |
|-------|-------------|------|
| Maintainer-authored city config and local site config | Trusted operator code | May define shell commands and explicit env. Review before use. |
| Imported packs and rig configs | Trusted dependency code | Pin/review packs before importing into a privileged city. |
| Bead titles, descriptions, mail, formula vars, PR text, and API request fields | Untrusted data | Do not concatenate into shell commands. Pass as env, JSON, stdin, or argv. |
| GitHub Actions `pull_request_target` payloads | Untrusted data in a privileged workflow | Do not checkout or execute contributor code. Use metadata-only operations. |
| Ambient process environment | Untrusted for secret propagation | Orchestrator-side shell helpers strip inherited secret-looking env keys by default. |

## Execution Surfaces

| Surface | Command source | Actor | Working directory | Env behavior | Log behavior |
|---------|----------------|-------|-------------------|--------------|--------------|
| `work_query` via `gc hook` and orchestrator probes | Agent config | Trusted operator or pack | Agent's canonical city or rig repo | Inherited secrets are stripped; Gas City projects explicit store/session env. | Errors are diagnostic only. Avoid placing secrets in command literals. |
| `scale_check` | Agent config | Trusted operator or pack | Agent's canonical city or rig repo | Inherited secrets are stripped; Gas City projects explicit store env. | Parse failures include command context; command literals must not contain secrets. |
| `on_boot` and `on_death` | Agent pool config | Trusted operator or pack | City or rig repo | Inherited secrets are stripped; explicit store env may be provided when needed. | Hook failures are logged; output should not include secrets. |
| Order `check` triggers | Order config | Trusted operator or pack | Order target scope | Inherited secrets are stripped; explicit condition env may be provided. | Failure reason records exit status, not command output. |
| Order `exec` | Order config | Trusted operator or pack | Order target scope | Inherited secrets are stripped; explicit order env may be provided. | Failure errors and output are redacted before logs/events. |
| `gc sling` and `/sling` command runner | Sling target config | Trusted operator or pack | City or rig repo | Inherited secrets are stripped; explicit routing/store env may be provided. | Returned command output is caller-visible. Do not route untrusted text into shell. |
| Agent `command` | Agent config | Trusted operator or pack | Session work directory | Session env is explicit runtime env plus configured env. Secrets may be passed only by intentional config. | Agent stdout/stderr is session output and may be visible to operators. |
| `pre_start` | Agent config | Trusted operator or pack | Session work directory | Provider-specific runtime env; intended for setup before session start. | Provider warnings should avoid secrets. |
| `session_setup`, `session_setup_script`, `session_live` | Agent config | Trusted operator or pack | Running session environment | Provider-specific runtime env; remote providers run inside the target container or pod. | Provider warnings should avoid secrets. |
| `exec:` session provider | User-supplied provider script | Trusted operator code | Provider-defined | Direct exec, not `sh -c`; start config is JSON on stdin. | Provider stderr may be surfaced in errors. Do not print secrets. |
| `exec:` beads, mail, and events providers | User-supplied provider script | Trusted operator code | Provider-defined | Direct exec, not `sh -c`; request data is stdin/argv. | Provider stderr may be surfaced in errors. Do not print secrets. |
| Pack fetch/include, Git probes, Docker, Dolt, tmux, kubectl, `bd` helpers | Gas City code plus configured paths/URLs | Maintainer-reviewed code paths | Command-specific | Direct exec with argv except provider setup scripts where documented. | Errors are surfaced for diagnosis; avoid embedding credentials in URLs. |

## Secret Propagation

Orchestrator-side shell helpers remove inherited environment variables whose keys
look secret-bearing, including names containing `TOKEN`, `PASSWORD`, `SECRET`,
`PRIVATE_KEY`, `API_KEY`, `ACCESS_KEY`, `CREDENTIAL`, `OAUTH`, or `AUTH_JSON`.
This prevents ambient CI or maintainer shell secrets from reaching `work_query`,
`scale_check`, hooks, order checks, order exec commands, and sling helpers by
accident.

If a command truly needs a secret, pass it explicitly through the relevant city,
rig, provider, or workflow configuration. Explicit values are preserved because
they represent an operator decision, and failure logs redact known secret values
before writing order exec errors or events.

## Mail Sender Identity

Mail `From` (and the `mail.created_by` provenance metadata alongside it) is a
caller-supplied claim, not a verified identity — this is true for `gc mail
send`/`gc mail reply`/`gc handoff` and for the HTTP `POST /v0/mail` and
`POST /v0/mail/{id}/reply` endpoints alike, **except** for messages carrying a
valid cryptographic signature (`Message.Verified`), described below. Absent
that signature, treat every mail `From` and `mail.created_by` value as a claim
from the same trust domain as the caller — useful for reconstructing what a
legitimate caller did, never as evidence capable of unmasking a deliberately
spoofed message.

### The env-var gate (defense-in-depth, not authentication)

The CLI refuses a sender claim that resolves to a reserved identity ("human"
or "controller") when a managed-session identity env var (`GC_SESSION_ID`,
`GC_ALIAS`, `GC_AGENT`) is present or set to that reserved name (see
`refuseUnauthenticatedHumanSender` and `isReservedMailIdentity` in
`cmd/gc/cmd_mail.go`). This is defense-in-depth against careless or naive
misuse, not authentication: those env vars are ordinary process environment
(see "Ambient process environment" above) that the calling process fully
controls, so a session that deliberately clears them before invoking `gc` is
indistinguishable from a genuinely unmanaged human shell and is not caught.
This was the exact gap used live to inject a fabricated "authenticated human
escalation" telling an agent session to bypass its own approval gate.

While building the mechanism below, a second, previously-undocumented gap in
this same gate was found and fixed: every call site checked literally
`sender == "human"` and never considered `"controller"`, so a **fully managed
session using its own real `GC_SESSION_ID`** — no env-stripping required at
all — could run `gc mail send --from controller ...` and sail through with no
refusal whatsoever. That was a strictly worse and simpler bypass than the
documented env-strip limitation. The gate now applies uniformly to both
reserved names via `isReservedMailIdentity`.

The HTTP API has no equivalent guard at all: `POST /v0/mail` accepts a
caller-supplied `from` (including "human"/"controller") from any process able
to reach 127.0.0.1:9443, with no per-caller authentication. Closing this needs
a caller-authentication design (e.g. per-session tokens) and remains a
follow-up.

### Cryptographic verification: `gc mail send --sign` (internal/humantrust)

`gc mail send --from human --sign` (or `--from controller --sign`) produces a
message that a reader can independently confirm really was produced by
whoever holds a specific Ed25519 private key — **without trusting the sending
process's environment at all**, closing the class of bypass above for good
rather than raising the bar on it.

**Design.** A human operator runs `gc mail trust init`, once, directly in
their own terminal (never as a side effect of anything a spawned session
does — the command refuses outright when a managed-session identity env var
is present). This generates an Ed25519 keypair and writes:

- the **private** key to `<Gas City home>/trust/human_signing_ed25519.key`
  (mode 0600) — `internal/humantrust.PrivateKeyPath()`, resolved via
  `internal/gchome` (`$GC_HOME` or `~/.gc`), a machine-local path distinct
  from any city directory a spawned session's working directory might
  overlap with;
- the **public** key alongside it, mode 0644 — safe to read from anywhere,
  including inside a spawned session, because knowing a public key does not
  let you forge a signature under it.

`gc mail send --sign` signs `(identity, recipient, body, issuedAt)` with the
private key (`internal/humantrust.Sign`) and stores the signature, claimed
identity, and issuance time as ordinary message metadata
(`mail.verified_signature`, `mail.verified_identity`,
`mail.verified_issued_at`). It refuses outright — never silently falling back
to an unsigned send — when: this process carries a managed-session identity
env var; the sender is not a reserved identity; or no private key is
loadable (`internal/humantrust.ErrKeyNotFound`, pointing the caller at
`gc mail trust init`).

**Verification never trusts the metadata's own say-so.** Every reader
(`beadmail.beadToMessage`, exercised by `gc mail check --inject`, `gc mail
inbox`, and the HTTP mail-list/get endpoints) *recomputes* the signed payload
from the message's own current `From`/recipient/body and checks it against
the trusted public key (`internal/humantrust.Verify`) on every read. A
message that merely carries `mail.verified_identity`/`mail.verified_signature`
metadata — hand-crafted by a session with direct store-write access, or left
over after the body was edited — does **not** verify unless that metadata
decodes to a signature that is actually valid for the message's exact current
content. `Message.Verified`/`Message.VerifiedIdentity` are the only fields a
caller should read; nothing about a message's stored metadata should be
treated as evidence on its own.

Messages that verify are rendered with a distinct
`[cryptographically verified <identity> sender]` marker in the
`<system-reminder>` block `gc mail check --inject` produces
(`formatInjectOutput`). Every other message — everything sent before this
mechanism existed, and any `--from human`/`--from controller` send made
without `--sign` — renders exactly as before, with no marker, and
`Message.Verified` is `false`. Introducing a real verified category does not
make the unverified category look more trustworthy by comparison; it stays
exactly as skeptically treated as it always was.

**What this proves (with empirical adversarial evidence).**
`internal/humantrust/humantrust_test.go`,
`internal/mail/beadmail/beadmail_test.go`, and
`cmd/gc/cmd_mail_test.go` construct a simulated spawned session with full
control of its own environment variables, `--from` claim, and mail
content — including the exact env-strip shape
(`GC_SESSION_ID`/`GC_ALIAS`/`GC_AGENT` all cleared) that defeated the
env-var gate above — but with **no private key ever provisioned into its
`GC_HOME`**, exactly the state a spawned session is in by construction
(the orchestrator never puts the operator's private key into a session's
environment or working directory). Every attempt to produce a verified
message is refused
(`TestCmdMailSendSignCannotForgeVerificationWithoutTrustKey`), and a
hand-crafted bead carrying fabricated `mail.verified_*` metadata (simulating
a session with direct store-write access forging the fields `SendSigned`
would have set, without the private key) never verifies
(`TestBeadToMessageNeverTrustsHandCraftedVerifiedMetadata`). The positive
case — a process with no managed-session identity and a genuinely
provisioned key succeeds and reads back verified — is proven by
`TestCmdMailSendSignSucceedsFromGenuineUnmanagedTerminal` and
`TestSendSignedVerifiesOnReadWithGenuineKey`. Tampering with a genuinely
signed message's body after the fact un-verifies it on the next read
(`TestBeadToMessageRejectsTamperedContentAfterGenuineSigning`), proving
verification is recomputed from live content, not cached.

**What this does NOT prove — read this before relying on it.**

- **No privilege separation on this host.** gascity has no separation
  between the orchestrator process and a spawned session's process: both run
  as the same OS user with no setuid, container, or namespace boundary
  (confirmed by reading `internal/session/lifecycle.go` and
  `internal/runtime/tmux`; session env is curated via `tmux -e` flags rather
  than inherited wholesale, but the spawned process is still a plain child of
  the same user's tmux server). This mechanism defeats a session
  manipulating its **own environment variables, CLI flags, or mail content**
  — the exact incident and the documented env-strip bypass — but it does
  **not** defend against a session that goes further and directly reads
  arbitrary files as that same OS user. If a session discovers the private
  key's path and that path is readable by its own UID (which it is, absent
  privilege separation), it can read the raw key exactly as it could already
  read `internal/convergence`'s controller token — a pre-existing,
  separately acknowledged gap (see the comment in `cmd/gc/controller.go`
  about keeping that token out of `os.Environ()`). Closing this residual
  risk needs real privilege separation (a different OS user or container
  boundary for spawned sessions, or a signing daemon reachable only over an
  authenticated channel a session cannot open) — infrastructure that does
  not exist in gascity today. Do not describe this mechanism as immune to a
  same-user filesystem adversary; it is not.
- **Bounded validity window, not full replay protection.** A signature is
  only accepted within `internal/humantrust.MaxSignatureAge` (15 minutes) of
  its claimed issuance time, but there is no nonce or used-signature ledger:
  a genuinely valid, still-fresh signature can be replayed verbatim (same
  identity/recipient/body/timestamp) within that window.
- **The subject/title line is not signed at all**, since beadmail can rewrite
  an empty subject into a truncated prefix of the body — put anything
  security-relevant in the body, not the subject.
- **Single-machine trust model.** The public key lookup is local to the Gas
  City home on this host. There is no built-in multi-host key distribution;
  copying the `.pub` file to another host that needs to verify the same
  operator's mail is a manual step (`gc mail trust show` prints the
  fingerprint for out-of-band confirmation).
- **Send scope: CLI `gc mail send` only.** `gc mail reply`, `gc handoff`, the
  `exec:` mail provider, and the HTTP API's `POST /v0/mail` send/reply paths
  do not support `--sign` — only `internal/mail/beadmail` implements
  `mail.SignedSender`. This is a deliberate, documented MVP scope, not an
  oversight: the reply path's recipient is derived from the original
  message rather than resolved the way `Send` resolves it, and extending
  signing there needs its own signed-payload shape.
- **Read scope: CLI + HTTP API list/get, not HTTP API send.** `Verified`/
  `VerifiedIdentity` round-trip through the generated HTTP client
  (`internal/api/genclient`, `internal/api/decode_mail.go`) so `gc mail check
  --inject` shows the verified marker whether or not a controller/supervisor
  is serving the request. The dashboard SPA's separate generated client
  (`internal/api/dashboardspa/web/shared/src/generated/gc-supervisor-client`)
  has not been regenerated and does not yet carry these fields.
- **Key rotation invalidates history.** `gc mail trust init --force`
  overwrites the keypair; every previously verified message stops verifying
  against the new key (the old signature, produced under the old key, cannot
  validate under the new public key). There is no multi-key/rotation-grace
  window.

Do not describe the env-var gate as verifying who sent a message — it never
does. Do describe `Message.Verified` as an actual cryptographic confirmation,
scoped exactly as this section states.

## Rules For Authors

- Do not put secrets directly in command strings. Use env variables or provider
  credential files.
- Do not interpolate bead content, PR text, mail, formula vars, branch names, or
  other user-controlled values into `sh -c` commands.
- When showing a command for a human to copy, build it from argv and quote each
  argument with Gas City's shell quoting helper.
- Keep `pull_request_target` workflows metadata-only. They may label or comment
  but must not checkout or run contributor code with privileged tokens.
- Prefer direct `exec.Command(..., args...)` style boundaries for new provider
  contracts. Use `sh -c` only for explicitly operator-authored shell snippets.
