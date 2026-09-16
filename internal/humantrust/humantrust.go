// Package humantrust implements the cryptographic trust root that lets a
// genuinely authenticated human operator (or an explicitly provisioned
// trusted relay process) mark a mail message as verified-reserved-identity
// ("human" or "controller") in a way that a spawned gc session's own process
// cannot forge — no matter what it does to its own environment variables,
// argv, or mail content.
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
// This package closes it with a real, asymmetric-cryptography trust root
// instead of an ambient-environment signal:
//
//   - An Ed25519 keypair is generated once by a human running `gc mail trust
//     init` directly (never by any code path a spawned session can trigger
//     as a side effect of its own operation).
//   - The PRIVATE key is written to a machine-local, human-provisioned file
//     (under the Gas City home directory, see internal/gchome) with mode
//     0600. Nothing in the ordinary `gc mail send`/`gc mail check` code path
//     — the commands a spawned session actually runs — ever reads this file
//     unless the caller explicitly passes --sign, and --sign itself is
//     refused outright whenever this process carries a managed-session
//     identity (see cmd/gc's refuseUnauthenticatedHumanSender family, which
//     this package's callers reuse). A spawned session is never handed this
//     key in its environment, and the ordinary send path never touches it.
//   - The PUBLIC key is safe to read from anywhere — including inside a
//     spawned session's own process — because knowing it does not let you
//     forge a signature. Verification (Verify, below) needs only the public
//     key, so the read side (gc mail check --inject, gc mail inbox, any
//     future HTTP API verification) can check a claimed signature without
//     ever holding, needing, or being able to reproduce the private key.
//
// # What this does NOT prove
//
// gascity has no privilege separation between the orchestrator and a
// spawned session: both run as the same OS user with no setuid, container,
// or namespace boundary (confirmed by reading internal/session/lifecycle.go
// and internal/runtime/tmux — session env is curated via tmux -e flags, not
// inherited wholesale, but the spawned process is a plain child of the same
// user's tmux server). That means:
//
//   - This mechanism defeats the SPECIFIC attack class it was built for: a
//     session manipulating its OWN environment variables, CLI flags, or mail
//     content to make an already-running `gc mail send` invocation claim a
//     reserved identity. That is the exact class used in the fabricated
//     "authenticated human escalation" incident this package responds to,
//     and it is what the adversarial tests in this package and in
//     cmd/gc/cmd_mail_test.go empirically prove is now impossible.
//   - This mechanism does NOT defend against a session that goes further and
//     directly reads arbitrary files on disk as the same OS user that runs
//     the orchestrator. If a session discovers the private key file's path
//     and that path is readable by its own UID (which it is, since there is
//     no privilege separation), it can read the raw key bytes exactly as it
//     could already do to internal/convergence's controller token (a
//     pre-existing, separately acknowledged gap — see the comment in
//     cmd/gc/controller.go about keeping that token out of os.Environ()).
//     Closing that residual risk needs real privilege separation (a
//     different OS user or container boundary for spawned sessions, or a
//     signing daemon reachable only over an authenticated channel a session
//     cannot open) — infrastructure that does not exist in gascity today.
//     Do not describe this package as immune to a same-user filesystem
//     adversary; it is not.
package humantrust

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
