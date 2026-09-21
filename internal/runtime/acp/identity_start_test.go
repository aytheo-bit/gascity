package acp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestStartIdentityFailureNeverPublishesReservation(t *testing.T) {
	p := newTestProvider(t)
	name := testName()
	blocked := p.metaPath(name, "GC_INSTANCE_TOKEN")
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "block"), []byte("block"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := p.Start(context.Background(), name, runtime.Config{Command: "false", Env: map[string]string{"GC_SESSION_ID": "owner", "GC_INSTANCE_TOKEN": "token"}})
	if err == nil || !strings.Contains(err.Error(), "publishing session identity") {
		t.Fatalf("Start = %v", err)
	}
	p.mu.Lock()
	_, reserved := p.conns[name]
	p.mu.Unlock()
	if reserved {
		t.Error("failed identity write published live reservation")
	}
	if got, _ := p.GetMeta(name, "GC_SESSION_ID"); got != "" {
		t.Errorf("partial identity retained: %q", got)
	}
}

func TestStartFailureClearsIdentity(t *testing.T) {
	p := newTestProvider(t)
	name := testName()
	err := p.Start(context.Background(), name, runtime.Config{Env: map[string]string{"GC_SESSION_ID": "owner", "GC_INSTANCE_TOKEN": "token", "GC_RUNTIME_EPOCH": "1"}})
	if err == nil || !strings.Contains(err.Error(), "requires a command") {
		t.Fatalf("Start = %v", err)
	}
	for _, key := range []string{"GC_SESSION_ID", "GC_INSTANCE_TOKEN", "GC_RUNTIME_EPOCH"} {
		if got, _ := p.GetMeta(name, key); got != "" {
			t.Errorf("failed start retained %s=%q", key, got)
		}
	}
}

func TestStoppedIdentityCleanupPreservesReplacement(t *testing.T) {
	p := newTestProvider(t)
	name := testName()
	replacement := &sessionConn{done: make(chan struct{})}
	p.conns[name] = replacement
	if err := p.SetMeta(name, "GC_SESSION_ID", "replacement"); err != nil {
		t.Fatal(err)
	}
	p.cleanupStoppedMeta(name)
	if got, _ := p.GetMeta(name, "GC_SESSION_ID"); got != "replacement" {
		t.Errorf("old stop erased replacement: %q", got)
	}
	delete(p.conns, name)
	p.cleanupStoppedMeta(name)
	if got, _ := p.GetMeta(name, "GC_SESSION_ID"); got != "" {
		t.Errorf("unowned metadata not cleared: %q", got)
	}
}
