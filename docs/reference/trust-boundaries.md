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

### Cryptographic verification: verified mail (internal/humantrust)

`gc mail send --from human --sign` (or `--from controller --sign`), and its
cross-machine counterpart `gc mail send --signed-envelope`, produce a message
that a reader can independently confirm really was produced by whoever holds
a specific Ed25519 private key — **without trusting the sending process's
environment at all**, closing the class of bypass above for good rather than
raising the bar on it.

**A first version of this mechanism had a real, empirically confirmed gap.**
The initial design generated the keypair and signed on whatever machine ran
`gc mail send --sign`, gated only on the absence of a managed-session
identity env var. Independent review then proved, empirically — by
provisioning a real key and loading it from a stripped-env process, the same
way the original env-var gate was defeated — that gascity's spawned sessions
never get their own `GC_HOME` (`internal/session/lifecycle.go` never sets
one), so `internal/gchome.Default()` resolves to the operator's **real**
`$HOME/.gc` for a spawned session exactly as it does for the operator. A
process that merely stripped `GC_SESSION_ID`/`GC_ALIAS`/`GC_AGENT` — the
exact original bypass shape — could load the operator's real private key from
that shared home directory and produce a genuinely verifying signature.
"Different `GC_HOME`" (what the first version's tests asserted) was never the
same thing as "different machine" (what a spawned session actually is
relative to the key), and describing it that way was itself part of the gap.

**The design below is the fix, and it changes where the private key is
allowed to exist, not just how it's gated.**

**Design: the private key never touches a node that spawns sessions.**
gascity's real deployment already has a genuine machine boundary: the human
operator (or an explicitly authorized relay) issues mail from a *separate*
machine, reaching the node that runs the orchestrator and every spawned
session over SSH. Spawned sessions run entirely on that node's own
filesystem and OS user, with no network path back to the operator's own
machine. This design puts the private key only where that boundary actually
protects it:

- A human operator runs `gc mail trust init`, once, **on their own machine**
  (or an explicitly provisioned trusted relay machine) — never on a node that
  runs the gascity orchestrator or any spawned session. This generates an
  Ed25519 keypair and writes the **private** key to
  `<Gas City home>/trust/human_signing_ed25519.key` (mode 0600) —
  `internal/humantrust.PrivateKeyPath()`, resolved via `internal/gchome`
  (`$GC_HOME` or `~/.gc`) **on that machine**. Nothing about `gc mail trust
  init`'s own execution can verify "this is the right machine" — that check
  is a deployment discipline the operator follows, not something the tool
  can detect about the host it happens to run on (see "What this does NOT
  prove" below).
- The **public** key is safe to distribute anywhere, including onto every
  node that verifies mail — knowing it does not let you forge a signature.
  `gc mail trust show` prints it (and its fingerprint) for the operator to
  copy; `gc mail trust import <base64-key>` writes **only** the public key
  on a verifying node (`internal/humantrust.WritePublicKeyOnly`) — there is
  no code path in `import` that creates, receives, or requires the private
  key, so running it on a node that spawns sessions does not reintroduce the
  gap above.
- **Signing happens on the key-holding machine.** `gc mail trust sign <to>
  <body>` loads the private key and produces a portable, self-contained
  envelope (`internal/humantrust.Envelope`, via `BuildEnvelope`/
  `EncodeEnvelope`) — identity, recipient, subject, body, issuance time, and
  signature, safe to relay as a single opaque token over SSH or a pasted
  terminal argument. The target node accepts it with `gc mail send
  --signed-envelope <token>` (or `-` to read the token from stdin): that
  command decodes the envelope, resolves the recipient exactly as an
  ordinary send would, locally re-verifies the signature against the
  **public** key already provisioned there (no private key involved), and
  only then stores it. `gc mail send --sign` (loading the private key and
  signing in the same process that sends) still exists for the case where
  that process genuinely IS the key-holding machine — the same "never on a
  node that spawns sessions" constraint governs both.
- Either way, the signature and issuance time are stored as ordinary message
  metadata (`mail.verified_signature`, `mail.verified_identity`,
  `mail.verified_issued_at`). Every signing surface refuses outright — never
  silently falling back to an unsigned send — rather than accept a claim it
  cannot back with a real signature.

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
without `--sign`/`--signed-envelope` — renders exactly as before, with no
marker, and `Message.Verified` is `false`. Introducing a real verified
category does not make the unverified category look more trustworthy by
comparison; it stays exactly as skeptically treated as it always was.

**Out-of-band fingerprint verification (do this on every provisioning
step).** `gc mail trust init`, `gc mail trust show`, and `gc mail trust
import` all print the key's fingerprint (`internal/humantrust.Fingerprint`).
Because a public key is only trustworthy if the copy a verifying node
imported is really the operator's, and not a key an attacker with prior
write access to that node's trust directory planted (paired with their own
privately-held key — see "What this does NOT prove"), compare the
fingerprint `gc mail trust import`/`gc mail trust show` reports on the
target node against the fingerprint printed at `gc mail trust init` time,
over a channel other than the one the key blob traveled over. `gc mail
trust import --expect-fingerprint <hex>` makes this an enforced check
rather than a manual one: pass the fingerprint you confirmed out-of-band and
the import itself refuses on a mismatch.

**What this proves (with empirical adversarial evidence).**
`internal/humantrust/humantrust_test.go` proves the crypto and the new
envelope/public-key-only primitives in isolation (`TestEnvelopeRoundTrip`,
`TestEnvelopeVerifiesAgainstOnlyThePublicKey`,
`TestWritePublicKeyOnlyNeverTouchesPrivateKeyPath`). The core adversarial
proof is `cmd/gc/cmd_mail_trust_relay_test.go`'s
`TestSpawnedSessionCannotForgeVerifiedMailWhenPrivateKeyNeverTouchesItsNode`,
which — learning from the first version's mistake — does not merely point a
process at a different `GC_HOME`. It provisions two genuinely separate home
directories with no shared filesystem (one holding a real keypair, standing
in for the operator's machine; one holding only an imported public key,
standing in for a node that runs the gascity orchestrator and every spawned
session), walks every file under the target directory and proves from actual
bytes on disk that the private key is not merely unreachable but genuinely,
exhaustively absent, then runs the real compiled `gc` binary as a **separate
OS process** — not an in-process function call — with the env-strip bypass
shape (`GC_SESSION_ID`/`GC_ALIAS`/`GC_AGENT` never set) attempting both
`mail send --sign` and `mail trust sign`. Both fail, and no message is
created. The same test then proves the replacement flow's positive case in
the same run: a signature produced by a process that legitimately holds the
key, relayed via `mail send --signed-envelope` into a third process
representing the target node, verifies correctly — and the private key still
never touches that node's filesystem, checked again after the relay
succeeds. `TestBeadToMessageNeverTrustsHandCraftedVerifiedMetadata` (a
hand-crafted bead carrying fabricated `mail.verified_*` metadata, simulating
a session with direct store-write access forging the fields `SendSigned`
would have set, without ever holding the private key) never verifies.
`TestBeadToMessageRejectsTamperedContentAfterGenuineSigning` proves tampering
with a genuinely signed message's body after the fact un-verifies it on the
next read, so verification is recomputed from live content, not cached.

**What this does NOT prove — read this before relying on it.**

- **Enforcement that the key stays off session-spawning nodes is
  operational, not automatic.** Nothing in `gc mail trust init` or `gc mail
  trust sign` can detect "this machine also runs the gascity orchestrator"
  in general. An operator who runs `gc mail trust init` directly on an
  orchestrator node recreates the exact gap this redesign closes — the tool
  can only warn about this in its help text, not prevent it. The empirical
  guarantee the tests above provide is conditional on the private key
  genuinely having been generated and kept only on a machine with no shared
  filesystem with the target node, which is how gascity's real deployment
  works today (an operator's own machine, or a CI/relay host, reaching a
  node over SSH) — it is not a claim that gc prevents an operator from
  misusing its own tooling.
- **The public-key import step is trust-on-first-use.** Whoever has write
  access to a verifying node's trust directory before (or instead of) a
  legitimate `gc mail trust import` can plant their own public key, paired
  with their own privately-held key, and their own self-signed mail then
  verifies on that node exactly as if it were genuine. `gc mail trust
  import` refuses to silently overwrite an existing public key without
  `--force`, and `--expect-fingerprint` lets the command itself enforce an
  out-of-band-confirmed fingerprint — but this is only as good as the
  operator's diligence in actually comparing fingerprints over a separate
  channel, every time a key is imported, not just once. If a node's
  filesystem was already compromised before the operator ever ran `import`
  on it, nothing here catches that.
- **No privilege separation on this host, for anything other than the
  trust key.** gascity has no separation between the orchestrator process
  and a spawned session's process: both still run as the same OS user with
  no setuid, container, or namespace boundary (confirmed by reading
  `internal/session/lifecycle.go` and `internal/runtime/tmux`). This
  redesign removes the *private signing key* specifically from that shared
  filesystem; it does nothing for other same-host secrets. A session that
  goes further and directly reads arbitrary files as that same OS user can
  still read anything else readable by its UID — e.g.
  `internal/convergence`'s controller token, a pre-existing, separately
  acknowledged gap (see the comment in `cmd/gc/controller.go` about keeping
  that token out of `os.Environ()`).
- **Bounded validity window, not full replay protection.** A signature is
  only accepted within `internal/humantrust.MaxSignatureAge` (15 minutes) of
  its claimed issuance time, but there is no nonce or used-signature ledger:
  a genuinely valid, still-fresh signature can be replayed verbatim (same
  identity/recipient/body/timestamp) within that window. For the relay flow
  specifically, this also bounds how long an operator has between running
  `gc mail trust sign` and the relayed `gc mail send --signed-envelope`
  actually reaching the target node — relay promptly.
- **The subject/title line is not signed at all**, since beadmail can rewrite
  an empty subject into a truncated prefix of the body — put anything
  security-relevant in the body, not the subject.
- **The relay flow depends on the recipient resolving identically on both
  machines.** `gc mail trust sign <to> ...` has no access to the target
  node's live session store, so `<to>` must be the exact address the target
  node will itself resolve the recipient to. If it resolves differently
  there, `gc mail send --signed-envelope` fails closed with a clear mismatch
  error (never a silently misdirected or silently-renamed-recipient
  message) — but that means a misremembered alias produces a failed relay,
  not a helpful auto-correction.
- **Send scope: CLI `gc mail send` only.** `gc mail reply`, `gc handoff`, the
  `exec:` mail provider, and the HTTP API's `POST /v0/mail` send/reply paths
  do not support `--sign`/`--signed-envelope` — only `internal/mail/beadmail`
  implements `mail.SignedSender`. This is a deliberate, documented MVP
  scope, not an oversight: the reply path's recipient is derived from the
  original message rather than resolved the way `Send` resolves it, and
  extending signing there needs its own signed-payload shape.
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
- **The operator's own key-holding machine is out of scope.** If that
  machine itself is compromised, nothing in this design helps — a real
  secret with real consequences if leaked is not a new problem this package
  introduces, and defending the operator's own endpoint is outside gascity's
  threat model to begin with.

Do not describe the env-var gate as verifying who sent a message — it never
does. Do describe `Message.Verified` as an actual cryptographic confirmation,
scoped exactly as this section states. Do describe the private key as never
touching a node that spawns sessions **when the documented flow is
followed** — not as something gc itself guarantees regardless of how an
operator chooses to run it.

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
