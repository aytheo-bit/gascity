// Package humantrust implements the cryptographic trust root that lets a
// genuinely authenticated human operator (or an explicitly provisioned
// trusted relay process) mark a mail message as verified-reserved-identity
// ("human" or "controller") in a way that a spawned gc session's own process
// cannot forge — not "hard to forge," genuinely unable to, because the
// private key is never present on any filesystem a spawned session can read.
//
// # Why this exists
//
// gc mail send --from human previously had no way to distinguish a genuine
// human operator from a managed session that simply claimed to be one. The
// first fix (see docs/reference/trust-boundaries.md, "Mail Sender Identity")
// gated on managed-session identity env vars (GC_SESSION_ID/GC_ALIAS/
// GC_AGENT) being absent — but those are ordinary environment variables the
// calling process fully controls, so a session that runs
// `env -u GC_SESSION_ID -u GC_ALIAS -u GC_AGENT gc mail send --from human`
// defeats the check trivially. That gap was confirmed empirically and
// documented as an accepted, unclosed limitation.
//
// A first cryptographic fix (Ed25519 keypair, private key under this
// process's Gas City home, gated on the absence of a managed-session env
// var) closed the ORIGINAL bypass but was itself found, empirically, to
// have a second gap: gascity's spawned sessions never get their own
// GC_HOME (internal/session/lifecycle.go never sets it), so
// internal/gchome.Default() resolves to the operator's REAL $HOME/.gc for a
// spawned session exactly as it does for the operator. A process that
// merely stripped GC_SESSION_ID/GC_ALIAS/GC_AGENT — the exact original
// bypass shape — could load the operator's real private key from that real
// home and produce a genuinely verifying signature. "Different GC_HOME" was
// never the same thing as "different machine," and the tests at the time
// asserted the former while describing it as the latter.
//
// # The real fix: the private key never touches a session-running node
//
// This package now leans on the machine boundary gascity's deployment
// already has, instead of an environment variable or a same-host directory
// convention:
//
//   - A human operator runs `gc mail trust init`, once, on their OWN
//     machine (or an explicitly provisioned trusted relay machine) — a
//     machine that does not run the gascity orchestrator or any spawned
//     session. This generates the Ed25519 keypair and writes the PRIVATE
//     key to that machine's Gas City home (WriteKeyPair, mode 0600). The
//     private key is never copied, synced, or otherwise made reachable from
//     any node that actually runs spawned gc sessions — enforced by the
//     operator following this flow, not by anything this package can detect
//     about the machine it's running on (see "What this does NOT prove").
//   - The PUBLIC key is safe to distribute anywhere, including to every
//     node that verifies mail — knowing it does not let you forge a
//     signature. `gc mail trust show` prints it (and its fingerprint) for
//     the operator to copy; `gc mail trust import` (WritePublicKeyOnly)
//     writes ONLY the public key on a verifying node, including nodes that
//     spawn sessions — it has no code path that can create, receive, or
//     require the private key file.
//   - Signing happens on the operator's machine: `gc mail trust sign`
//     (or `gc mail send --sign` when the process running it genuinely IS
//     the key-holding machine) loads the private key and produces a
//     signature. `gc mail trust sign` additionally packages the result into
//     a portable, self-contained Envelope (EncodeEnvelope) — identity,
//     recipient, subject, body, issuance time, and signature — that never
//     contains the private key and is safe to relay over the network (an
//     SSH command line, a pasted terminal argument) into a target node's
//     `gc mail send --signed-envelope`. That command decodes the envelope,
//     locally re-checks the signature against the PUBLIC key already
//     provisioned on that node (Verify — no private key involved), and
//     stores it. The target node's `gc mail send` process — and therefore
//     every spawned session that could ever run on that same node — never
//     loads, receives, or requires the private key at any point in this
//     flow.
//
// # What this does NOT prove
//
//   - Enforcement that the operator actually keeps the private key off any
//     node that spawns sessions is OPERATIONAL, not automatic. Nothing in
//     `gc mail trust init` or `gc mail trust sign` can detect "this machine
//     also runs the gascity orchestrator" in general; an operator who runs
//     `gc mail trust init` directly on an orchestrator node recreates the
//     exact gap this redesign closes. The empirical guarantee this package
//     and its tests provide is conditional: GIVEN that the private key was
//     only ever generated and kept on a machine with no shared filesystem
//     with the target node (the deployment gascity actually uses today —
//     an operator's own machine, or a CI/relay host, reaching a node over
//     SSH), a process on that target node — spawned session or not — cannot
//     load a key that was never placed there. It is not a claim that gc
//     itself prevents operator misuse of its own tooling.
//   - The public-key-import step is trust-on-first-use: whoever has write
//     access to a verifying node's trust directory before (or instead of) a
//     legitimate `gc mail trust import` can plant their own public key,
//     paired with their own privately-held key, and their own self-signed
//     mail then verifies on that node exactly as if it were genuine. This
//     is why `gc mail trust import` refuses to silently overwrite an
//     existing public key without --force, supports --expect-fingerprint
//     for a same-command sanity check, and why operators are told (see
//     docs/reference/trust-boundaries.md) to compare the fingerprint
//     `gc mail trust show` reports on the target node, out-of-band (a
//     voice call, a separate already-trusted channel — never the same
//     channel the key blob traveled over), against the fingerprint printed
//     at `trust init` time. That check is only as good as the operator's
//     diligence in actually performing it, every time a key is imported,
//     not just once.
//   - Everything unrelated to key location is unchanged and still holds:
//     the signature only bounds validity to MaxSignatureAge and is not full
//     replay protection, the subject/title line is never signed, only the
//     built-in beadmail provider implements SignedSender, key rotation
//     invalidates every previously verified message, and the operator's own
//     key-holding machine being compromised is outside what any of this
//     can help with — that machine holding a real secret with real
//     consequences if leaked is not a new problem this package introduces.
package humantrust

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/gchome"
)

// TrustDirName is the subdirectory of the Gas City home
// (internal/gchome.Default) that holds the human-signing keypair.
const TrustDirName = "trust"

// PrivateKeyFileName is the private-key filename within TrustDirName. It is
// written with mode 0600 and must never be copied into a session's working
// directory, environment, or any city-relative path.
const PrivateKeyFileName = "human_signing_ed25519.key"

// PublicKeyFileName is the public-key filename within TrustDirName. Reading
// it is always safe — it is written with mode 0644 by design — since a
// public key does not let its reader forge a signature.
const PublicKeyFileName = "human_signing_ed25519.pub"

// MaxSignatureAge bounds how long a signature remains acceptable after it was
// issued. This is NOT full replay protection (a captured, still-fresh valid
// signature can be replayed verbatim within this window — there is no nonce
// or used-signature ledger); it only bounds how long a leaked or intercepted
// signed payload stays usable. See the package doc and
// docs/reference/trust-boundaries.md for the honest limitation statement.
const MaxSignatureAge = 15 * time.Minute

// ErrKeyNotFound is returned by LoadPrivateKey/LoadPublicKey when no trust
// key has been provisioned yet.
var ErrKeyNotFound = errors.New("humantrust: no signing key found (run 'gc mail trust init')")

// TrustDir returns the directory holding the human-signing keypair.
func TrustDir() string {
	return filepath.Join(gchome.Default(), TrustDirName)
}

// PrivateKeyPath returns the path to the private key file.
func PrivateKeyPath() string {
	return filepath.Join(TrustDir(), PrivateKeyFileName)
}

// PublicKeyPath returns the path to the public key file.
func PublicKeyPath() string {
	return filepath.Join(TrustDir(), PublicKeyFileName)
}

// GenerateKeyPair creates a new random Ed25519 keypair.
func GenerateKeyPair() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("humantrust: generating keypair: %w", err)
	}
	return pub, priv, nil
}

// WriteKeyPair writes pub and priv to their well-known paths under TrustDir,
// atomically and with restrictive permissions on the private key (0600) and
// world-readable permissions on the public key (0644, safe by design). It
// refuses to overwrite an existing private key unless force is true, so a
// careless re-run cannot silently rotate the operator's key out from under
// already-verified mail.
func WriteKeyPair(pub ed25519.PublicKey, priv ed25519.PrivateKey, force bool) error {
	dir := TrustDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("humantrust: creating trust dir: %w", err)
	}
	privPath := PrivateKeyPath()
	if !force {
		if _, err := os.Stat(privPath); err == nil {
			return fmt.Errorf("humantrust: private key already exists at %s (pass --force to rotate it)", privPath)
		}
	}
	if err := atomicWriteFile(dir, privPath, []byte(base64.StdEncoding.EncodeToString(priv)), 0o600); err != nil {
		return fmt.Errorf("humantrust: writing private key: %w", err)
	}
	if err := atomicWriteFile(dir, PublicKeyPath(), []byte(base64.StdEncoding.EncodeToString(pub)), 0o644); err != nil {
		return fmt.Errorf("humantrust: writing public key: %w", err)
	}
	return nil
}

// WritePublicKeyOnly writes ONLY pub to its well-known path under TrustDir.
// It never creates, requires, or otherwise touches the private key file —
// this is the provisioning primitive for a node that must be able to verify
// signed mail but must never hold the private key (including every node
// that spawns gc sessions). "gc mail trust import" is the CLI surface for
// this.
//
// Refuses to silently overwrite an existing public key unless force is
// true, mirroring WriteKeyPair's rotation-safety posture: replacing a
// node's trusted public key is a trust-changing operation (see the package
// doc's note on trust-on-first-use) and should be deliberate and visible,
// never accidental.
func WritePublicKeyOnly(pub ed25519.PublicKey, force bool) error {
	dir := TrustDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("humantrust: creating trust dir: %w", err)
	}
	pubPath := PublicKeyPath()
	if !force {
		if _, err := os.Stat(pubPath); err == nil {
			return fmt.Errorf("humantrust: public key already exists at %s (pass --force to replace it — confirm the new key's fingerprint out-of-band first)", pubPath)
		}
	}
	if err := atomicWriteFile(dir, pubPath, []byte(base64.StdEncoding.EncodeToString(pub)), 0o644); err != nil {
		return fmt.Errorf("humantrust: writing public key: %w", err)
	}
	return nil
}

// ParsePublicKeyString decodes a base64-encoded public key exactly as
// printed by "gc mail trust show" (the same encoding LoadPublicKey reads
// from disk), for "gc mail trust import" to consume.
func ParsePublicKeyString(s string) (ed25519.PublicKey, error) {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("humantrust: decoding public key: %w", err)
	}
	if len(decoded) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("humantrust: public key has invalid length %d", len(decoded))
	}
	return ed25519.PublicKey(decoded), nil
}

// EncodePublicKeyString returns pub's canonical base64 form: what
// ParsePublicKeyString accepts, what the on-disk file contains, and what
// "gc mail trust show" prints for an operator to copy onto another node.
func EncodePublicKeyString(pub ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(pub)
}

// atomicWriteFile writes data to path via a temp file in dir followed by a
// rename, mirroring internal/convergence.WriteToken's crash-safety pattern.
func atomicWriteFile(dir, path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(dir, ".humantrust-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { os.Remove(tmpName) } //nolint:errcheck // best-effort cleanup
	if _, err := tmp.Write(data); err != nil {
		tmp.Close() //nolint:errcheck // cleanup after write failure
		cleanup()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close() //nolint:errcheck // cleanup after chmod failure
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close() //nolint:errcheck // cleanup after sync failure
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// LoadPrivateKey reads and decodes the private key. Returns ErrKeyNotFound
// when no key has been provisioned.
func LoadPrivateKey() (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(PrivateKeyPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrKeyNotFound
		}
		return nil, fmt.Errorf("humantrust: reading private key: %w", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("humantrust: decoding private key: %w", err)
	}
	if len(decoded) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("humantrust: private key at %s has invalid length %d", PrivateKeyPath(), len(decoded))
	}
	return ed25519.PrivateKey(decoded), nil
}

// LoadPublicKey reads and decodes the public key. Returns ErrKeyNotFound
// when no key has been provisioned; callers on the read/verify side must
// treat that as "cannot verify" (Verified=false), never as an error that
// blocks reading otherwise-ordinary mail.
func LoadPublicKey() (ed25519.PublicKey, error) {
	raw, err := os.ReadFile(PublicKeyPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrKeyNotFound
		}
		return nil, fmt.Errorf("humantrust: reading public key: %w", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("humantrust: decoding public key: %w", err)
	}
	if len(decoded) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("humantrust: public key at %s has invalid length %d", PublicKeyPath(), len(decoded))
	}
	return ed25519.PublicKey(decoded), nil
}

// Fingerprint returns a short, human-comparable hex digest of a public key,
// for operators to confirm out-of-band that two hosts (or a host and a
// backup) agree on the same trust root without comparing the full base64
// blob.
func Fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// canonicalPayload builds the exact byte sequence that is signed and
// verified. It deliberately excludes the message subject/title: subject
// derivation in beadmail can rewrite an empty subject to a truncated prefix
// of the body, which would make byte-for-byte signature verification
// fragile for no security benefit. Only identity, recipient, body, and
// issuance time are authenticated; put anything that matters in the body,
// not the subject line.
//
// The leading version tag provides domain separation against payloads
// signed for a different purpose ever being replayed here, and the "to:"/
// "identity:" labels prevent a Send payload from verifying as some other
// field arrangement.
func canonicalPayload(identity, to, body string, issuedAt time.Time) []byte {
	var b strings.Builder
	b.WriteString("gascity-mail-verified-v1\x00identity:")
	b.WriteString(identity)
	b.WriteString("\x00to:")
	b.WriteString(to)
	b.WriteString("\x00body:")
	b.WriteString(body)
	b.WriteString("\x00issued_at:")
	b.WriteString(issuedAt.UTC().Format(time.RFC3339Nano))
	return []byte(b.String())
}

// Sign produces a signature over (identity, to, body, issuedAt) using priv.
// Only a process holding priv can produce a signature that Verify accepts;
// see the package doc for exactly which process that is (and is not).
func Sign(priv ed25519.PrivateKey, identity, to, body string, issuedAt time.Time) []byte {
	return ed25519.Sign(priv, canonicalPayload(identity, to, body, issuedAt))
}

// Verify reports whether sig is a valid, non-expired signature over
// (identity, to, body, issuedAt) under pub. It recomputes the canonical
// payload from the CURRENT values passed in — it never trusts a caller's
// claim about what was signed — so mutating any of identity/to/body after
// signing invalidates the signature, and a signature is only ever accepted
// for the exact content it was issued over.
//
// Verify enforces MaxSignatureAge as a bounded validity window, not full
// replay protection: a genuinely valid, still-fresh signature can be
// replayed verbatim (same identity/to/body/issuedAt) within that window.
func Verify(pub ed25519.PublicKey, identity, to, body string, issuedAt time.Time, sig []byte) bool {
	if len(pub) != ed25519.PublicKeySize || len(sig) != ed25519.SignatureSize {
		return false
	}
	if issuedAt.IsZero() {
		return false
	}
	now := time.Now()
	if issuedAt.After(now.Add(1 * time.Minute)) {
		// Reject signatures claiming to be issued from the future (beyond a
		// small clock-skew allowance) rather than silently accepting them.
		return false
	}
	if now.Sub(issuedAt) > MaxSignatureAge {
		return false
	}
	return ed25519.Verify(pub, canonicalPayload(identity, to, body, issuedAt), sig)
}

// Envelope is a portable, self-contained signed mail payload: everything
// [mail.SendSigned] needs to store a signed message, plus everything
// [Verify] needs to re-check the signature, bundled together so a process
// that does NOT hold the private key can relay it into a target node's mail
// store without trusting that it wasn't corrupted or tampered with in
// transit. Subject is carried but — like canonicalPayload — is not itself
// authenticated by the signature.
//
// Build one with BuildEnvelope on the machine that holds the private key;
// transport it with EncodeEnvelope/DecodeEnvelope; land it on a target node
// with "gc mail send --signed-envelope", which never needs the private key
// to do so.
type Envelope struct {
	Identity  string
	To        string
	Subject   string
	Body      string
	IssuedAt  time.Time
	Signature []byte
}

// envelopeWireVersion tags every encoded envelope, for the same domain-
// separation reason canonicalPayload has a leading tag: a token produced
// for some other purpose must never be mistaken for a signed mail envelope.
const envelopeWireVersion = "gc-mail-envelope-v1"

type envelopeWire struct {
	Version   string `json:"v"`
	Identity  string `json:"identity"`
	To        string `json:"to"`
	Subject   string `json:"subject"`
	Body      string `json:"body"`
	IssuedAt  string `json:"issued_at"`
	Signature string `json:"signature"`
}

// BuildEnvelope signs (identity, to, body, issuedAt) with priv and packages
// the result — plus subject, which Sign/Verify never cover — into an
// Envelope ready for EncodeEnvelope.
//
// Call this ONLY from a process that legitimately holds priv: the
// operator's own machine, or an explicitly provisioned trusted relay —
// never from a node that runs spawned gc sessions. Nothing in this function
// enforces that; see the package doc's "What this does NOT prove".
func BuildEnvelope(priv ed25519.PrivateKey, identity, to, subject, body string, issuedAt time.Time) Envelope {
	return Envelope{
		Identity:  identity,
		To:        to,
		Subject:   subject,
		Body:      body,
		IssuedAt:  issuedAt,
		Signature: Sign(priv, identity, to, body, issuedAt),
	}
}

// EncodeEnvelope serializes env into a single opaque token safe to pass as
// one command-line argument, pipe over SSH, or paste into a terminal:
// JSON wrapped in standard base64 behind a version prefix.
func EncodeEnvelope(env Envelope) (string, error) {
	wire := envelopeWire{
		Version:   envelopeWireVersion,
		Identity:  env.Identity,
		To:        env.To,
		Subject:   env.Subject,
		Body:      env.Body,
		IssuedAt:  env.IssuedAt.UTC().Format(time.RFC3339Nano),
		Signature: base64.StdEncoding.EncodeToString(env.Signature),
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		return "", fmt.Errorf("humantrust: encoding envelope: %w", err)
	}
	return envelopeWireVersion + ":" + base64.StdEncoding.EncodeToString(raw), nil
}

// DecodeEnvelope reverses EncodeEnvelope, validating the version prefix and
// field shapes. It does NOT check the signature itself — callers use Verify
// for that against whatever public key this process has, which may
// legitimately be absent (no trust key imported yet) or simply different
// from the signer's (wrong key, tampered token).
func DecodeEnvelope(token string) (Envelope, error) {
	prefix := envelopeWireVersion + ":"
	if !strings.HasPrefix(strings.TrimSpace(token), prefix) {
		return Envelope{}, fmt.Errorf("humantrust: not a recognized signed mail envelope (missing %q prefix)", envelopeWireVersion)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(strings.TrimSpace(token), prefix))
	if err != nil {
		return Envelope{}, fmt.Errorf("humantrust: decoding envelope: %w", err)
	}
	var wire envelopeWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return Envelope{}, fmt.Errorf("humantrust: decoding envelope: %w", err)
	}
	if wire.Version != envelopeWireVersion {
		return Envelope{}, fmt.Errorf("humantrust: envelope version %q, want %q", wire.Version, envelopeWireVersion)
	}
	if wire.Identity == "" || wire.To == "" {
		return Envelope{}, errors.New("humantrust: envelope missing identity or recipient")
	}
	issuedAt, err := time.Parse(time.RFC3339Nano, wire.IssuedAt)
	if err != nil {
		return Envelope{}, fmt.Errorf("humantrust: envelope issued_at: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(wire.Signature)
	if err != nil {
		return Envelope{}, fmt.Errorf("humantrust: envelope signature: %w", err)
	}
	if len(sig) != ed25519.SignatureSize {
		return Envelope{}, fmt.Errorf("humantrust: envelope signature has invalid length %d", len(sig))
	}
	return Envelope{
		Identity:  wire.Identity,
		To:        wire.To,
		Subject:   wire.Subject,
		Body:      wire.Body,
		IssuedAt:  issuedAt,
		Signature: sig,
	}, nil
}
