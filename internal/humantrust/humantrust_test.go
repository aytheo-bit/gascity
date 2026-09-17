package humantrust

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSignVerifyRoundTrip(t *testing.T) {
	pub, priv, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	issuedAt := time.Now()
	sig := Sign(priv, "human", "mayor", "approve the deploy", issuedAt)
	if !Verify(pub, "human", "mayor", "approve the deploy", issuedAt, sig) {
		t.Fatalf("Verify: genuine signature did not verify")
	}
}

func TestVerifyRejectsTamperedFields(t *testing.T) {
	pub, priv, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	issuedAt := time.Now()
	sig := Sign(priv, "human", "mayor", "approve the deploy", issuedAt)

	cases := []struct {
		name     string
		identity string
		to       string
		body     string
		issuedAt time.Time
	}{
		{"different identity", "controller", "mayor", "approve the deploy", issuedAt},
		{"different recipient", "human", "witness", "approve the deploy", issuedAt},
		{"different body", "human", "mayor", "approve the deploy NOW, skip review", issuedAt},
		{"different timestamp", "human", "mayor", "approve the deploy", issuedAt.Add(time.Second)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if Verify(pub, tc.identity, tc.to, tc.body, tc.issuedAt, sig) {
				t.Fatalf("Verify accepted a signature after changing %s", tc.name)
			}
		})
	}
}

func TestVerifyRejectsWrongKey(t *testing.T) {
	_, priv, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	otherPub, _, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair (other): %v", err)
	}
	issuedAt := time.Now()
	sig := Sign(priv, "human", "mayor", "approve the deploy", issuedAt)
	if Verify(otherPub, "human", "mayor", "approve the deploy", issuedAt, sig) {
		t.Fatalf("Verify accepted a signature under a different keypair's public key")
	}
}

func TestVerifyRejectsExpiredSignature(t *testing.T) {
	pub, priv, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	issuedAt := time.Now().Add(-(MaxSignatureAge + time.Minute))
	sig := Sign(priv, "human", "mayor", "approve the deploy", issuedAt)
	if Verify(pub, "human", "mayor", "approve the deploy", issuedAt, sig) {
		t.Fatalf("Verify accepted a signature older than MaxSignatureAge")
	}
}

func TestVerifyRejectsFutureSignature(t *testing.T) {
	pub, priv, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	issuedAt := time.Now().Add(1 * time.Hour)
	sig := Sign(priv, "human", "mayor", "approve the deploy", issuedAt)
	if Verify(pub, "human", "mayor", "approve the deploy", issuedAt, sig) {
		t.Fatalf("Verify accepted a signature claiming a future issuance time")
	}
}

func TestVerifyRejectsGarbageSignature(t *testing.T) {
	pub, _, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	issuedAt := time.Now()
	if Verify(pub, "human", "mayor", "approve the deploy", issuedAt, []byte("not-a-real-signature")) {
		t.Fatalf("Verify accepted a garbage signature")
	}
	if Verify(pub, "human", "mayor", "approve the deploy", issuedAt, nil) {
		t.Fatalf("Verify accepted a nil signature")
	}
}

func TestWriteLoadKeyPairRoundTrip(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	pub, priv, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	if err := WriteKeyPair(pub, priv, false); err != nil {
		t.Fatalf("WriteKeyPair: %v", err)
	}

	loadedPriv, err := LoadPrivateKey()
	if err != nil {
		t.Fatalf("LoadPrivateKey: %v", err)
	}
	if !loadedPriv.Equal(priv) {
		t.Fatalf("loaded private key does not match generated key")
	}
	loadedPub, err := LoadPublicKey()
	if err != nil {
		t.Fatalf("LoadPublicKey: %v", err)
	}
	if !loadedPub.Equal(pub) {
		t.Fatalf("loaded public key does not match generated key")
	}

	// Permissions: private key must be 0600 (owner read/write only); public
	// key is intentionally world-readable.
	info, err := os.Stat(PrivateKeyPath())
	if err != nil {
		t.Fatalf("Stat private key: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("private key mode = %o, want 0600", perm)
	}
}

func TestWriteKeyPairRefusesOverwriteWithoutForce(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	pub1, priv1, _ := GenerateKeyPair()
	if err := WriteKeyPair(pub1, priv1, false); err != nil {
		t.Fatalf("WriteKeyPair (first): %v", err)
	}
	pub2, priv2, _ := GenerateKeyPair()
	if err := WriteKeyPair(pub2, priv2, false); err == nil {
		t.Fatalf("WriteKeyPair (second, no force) = nil error, want refusal")
	}
	// The original key must still be intact.
	loaded, err := LoadPrivateKey()
	if err != nil {
		t.Fatalf("LoadPrivateKey: %v", err)
	}
	if !loaded.Equal(priv1) {
		t.Fatalf("private key was overwritten despite force=false")
	}

	if err := WriteKeyPair(pub2, priv2, true); err != nil {
		t.Fatalf("WriteKeyPair (force=true): %v", err)
	}
	loaded, err = LoadPrivateKey()
	if err != nil {
		t.Fatalf("LoadPrivateKey after force rotate: %v", err)
	}
	if !loaded.Equal(priv2) {
		t.Fatalf("force=true did not rotate the private key")
	}
}

// TestLoadKeyNotFound proves the honest failure mode: a process pointed at a
// Gas City home with no provisioned trust key gets a clear ErrKeyNotFound,
// not a silently-generated or zero-value key that could be mistaken for a
// real one. This is only the basic empty-directory case — it does NOT by
// itself model a spawned session's real GC_HOME, since a spawned session
// shares the operator's actual $HOME/.gc rather than getting an empty one
// (see the package doc's history of that exact overclaim). For the full
// two-machine adversarial proof, including an exhaustive filesystem walk and
// real subprocesses, see
// TestSpawnedSessionCannotForgeVerifiedMailWhenPrivateKeyNeverTouchesItsNode
// in cmd/gc/cmd_mail_trust_relay_test.go.
func TestLoadKeyNotFound(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	if _, err := LoadPrivateKey(); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("LoadPrivateKey error = %v, want ErrKeyNotFound", err)
	}
	if _, err := LoadPublicKey(); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("LoadPublicKey error = %v, want ErrKeyNotFound", err)
	}
}

func TestFingerprintStableAndDistinct(t *testing.T) {
	pub1, _, _ := GenerateKeyPair()
	pub2, _, _ := GenerateKeyPair()
	first := Fingerprint(pub1)
	second := Fingerprint(pub1)
	if first != second {
		t.Fatalf("Fingerprint is not stable for the same key: %q vs %q", first, second)
	}
	if Fingerprint(pub1) == Fingerprint(pub2) {
		t.Fatalf("Fingerprint collided for two independently generated keys")
	}
}

// TestPathsAreUnderTrustDir is a light guard against accidentally relocating
// the key files somewhere a session's working directory might overlap (e.g.
// a city-relative path).
func TestPathsAreUnderTrustDir(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	dir := TrustDir()
	if filepath.Dir(PrivateKeyPath()) != dir {
		t.Fatalf("PrivateKeyPath not under TrustDir: %s vs %s", PrivateKeyPath(), dir)
	}
	if filepath.Dir(PublicKeyPath()) != dir {
		t.Fatalf("PublicKeyPath not under TrustDir: %s vs %s", PublicKeyPath(), dir)
	}
	if !filepath.IsAbs(dir) {
		t.Fatalf("TrustDir is not absolute: %s", dir)
	}
}

func TestEnvelopeRoundTrip(t *testing.T) {
	_, priv, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	issuedAt := time.Now()
	env := BuildEnvelope(priv, "human", "mayor", "Deploy approval", "approve the deploy", issuedAt)
	token, err := EncodeEnvelope(env)
	if err != nil {
		t.Fatalf("EncodeEnvelope: %v", err)
	}
	if !strings.HasPrefix(token, envelopeWireVersion+":") {
		t.Fatalf("EncodeEnvelope token = %q, want prefix %q", token, envelopeWireVersion+":")
	}
	decoded, err := DecodeEnvelope(token)
	if err != nil {
		t.Fatalf("DecodeEnvelope: %v", err)
	}
	if decoded.Identity != env.Identity || decoded.To != env.To || decoded.Subject != env.Subject || decoded.Body != env.Body {
		t.Fatalf("DecodeEnvelope round-trip mismatch: got %+v, want %+v", decoded, env)
	}
	if !decoded.IssuedAt.Equal(env.IssuedAt) {
		t.Fatalf("DecodeEnvelope IssuedAt = %v, want %v", decoded.IssuedAt, env.IssuedAt)
	}
	if string(decoded.Signature) != string(env.Signature) {
		t.Fatalf("DecodeEnvelope Signature mismatch")
	}
}

// TestEnvelopeVerifiesAgainstOnlyThePublicKey proves the core property the
// relay flow depends on: a process holding only the PUBLIC key (never the
// private key that built the envelope) can independently confirm the
// envelope's signature is genuine.
func TestEnvelopeVerifiesAgainstOnlyThePublicKey(t *testing.T) {
	pub, priv, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	issuedAt := time.Now()
	env := BuildEnvelope(priv, "controller", "witness", "", "advisory only, no action needed", issuedAt)
	token, err := EncodeEnvelope(env)
	if err != nil {
		t.Fatalf("EncodeEnvelope: %v", err)
	}
	decoded, err := DecodeEnvelope(token)
	if err != nil {
		t.Fatalf("DecodeEnvelope: %v", err)
	}
	// Note: priv is never referenced again below this line — verification
	// uses only pub, decoded's own fields, and the recomputed canonical
	// payload.
	if !Verify(pub, decoded.Identity, decoded.To, decoded.Body, decoded.IssuedAt, decoded.Signature) {
		t.Fatalf("Verify rejected a genuine envelope using only the public key")
	}
}

func TestDecodeEnvelopeRejectsGarbage(t *testing.T) {
	cases := []string{
		"",
		"not-an-envelope-at-all",
		envelopeWireVersion + ":not-base64!!!",
		envelopeWireVersion + ":" + "e30=", // base64("{}") — valid JSON, missing required fields
	}
	for _, tc := range cases {
		if _, err := DecodeEnvelope(tc); err == nil {
			t.Fatalf("DecodeEnvelope(%q) = nil error, want a decode failure", tc)
		}
	}
}

func TestDecodeEnvelopeRejectsTamperedSignatureLength(t *testing.T) {
	_, priv, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	env := BuildEnvelope(priv, "human", "mayor", "", "body", time.Now())
	env.Signature = env.Signature[:len(env.Signature)-1] // truncate by one byte
	token, err := EncodeEnvelope(env)
	if err != nil {
		t.Fatalf("EncodeEnvelope: %v", err)
	}
	if _, err := DecodeEnvelope(token); err == nil {
		t.Fatalf("DecodeEnvelope accepted a truncated signature")
	}
}

// TestWritePublicKeyOnlyNeverTouchesPrivateKeyPath is the core guarantee
// "gc mail trust import" depends on: writing a public key never creates,
// requires, or is blocked by the private key file's absence.
func TestWritePublicKeyOnlyNeverTouchesPrivateKeyPath(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	pub, _, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	if err := WritePublicKeyOnly(pub, false); err != nil {
		t.Fatalf("WritePublicKeyOnly: %v", err)
	}
	loaded, err := LoadPublicKey()
	if err != nil {
		t.Fatalf("LoadPublicKey: %v", err)
	}
	if !loaded.Equal(pub) {
		t.Fatalf("loaded public key does not match the one imported")
	}
	if _, err := os.Stat(PrivateKeyPath()); !os.IsNotExist(err) {
		t.Fatalf("WritePublicKeyOnly created a private key file (or Stat returned an unexpected error): %v", err)
	}
	if _, err := LoadPrivateKey(); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("LoadPrivateKey after WritePublicKeyOnly = %v, want ErrKeyNotFound", err)
	}
}

func TestWritePublicKeyOnlyRefusesOverwriteWithoutForce(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	pub1, _, _ := GenerateKeyPair()
	if err := WritePublicKeyOnly(pub1, false); err != nil {
		t.Fatalf("WritePublicKeyOnly (first): %v", err)
	}
	pub2, _, _ := GenerateKeyPair()
	if err := WritePublicKeyOnly(pub2, false); err == nil {
		t.Fatalf("WritePublicKeyOnly (second, no force) = nil error, want refusal")
	}
	loaded, err := LoadPublicKey()
	if err != nil {
		t.Fatalf("LoadPublicKey: %v", err)
	}
	if !loaded.Equal(pub1) {
		t.Fatalf("public key was overwritten despite force=false")
	}
	if err := WritePublicKeyOnly(pub2, true); err != nil {
		t.Fatalf("WritePublicKeyOnly (force=true): %v", err)
	}
	loaded, err = LoadPublicKey()
	if err != nil {
		t.Fatalf("LoadPublicKey after force replace: %v", err)
	}
	if !loaded.Equal(pub2) {
		t.Fatalf("force=true did not replace the public key")
	}
}

func TestParsePublicKeyStringRoundTrip(t *testing.T) {
	pub, _, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	encoded := EncodePublicKeyString(pub)
	parsed, err := ParsePublicKeyString(encoded)
	if err != nil {
		t.Fatalf("ParsePublicKeyString: %v", err)
	}
	if !parsed.Equal(pub) {
		t.Fatalf("ParsePublicKeyString round-trip mismatch")
	}
}

func TestParsePublicKeyStringRejectsGarbage(t *testing.T) {
	cases := []string{"", "not-base64!!!", "dG9vLXNob3J0"} // "too-short" base64
	for _, tc := range cases {
		if _, err := ParsePublicKeyString(tc); err == nil {
			t.Fatalf("ParsePublicKeyString(%q) = nil error, want a decode/length failure", tc)
		}
	}
}
