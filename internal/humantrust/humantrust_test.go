package humantrust

import (
	"errors"
	"os"
	"path/filepath"
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
// real one. This is the state a spawned session is in by construction: the
// orchestrator never provisions a trust key into a session's own GC_HOME.
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
