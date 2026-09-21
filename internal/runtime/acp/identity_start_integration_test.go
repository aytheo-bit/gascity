//go:build integration

package acp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestStartIdentityBeforeHandshake owns the real ACP process/sidecar ordering
// boundary: a blocked handshake is already live and must identify its owner.
func TestStartIdentityBeforeHandshake(t *testing.T) {
	for _, cancelStart := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%v", cancelStart), func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			if err := listener.(*net.TCPListener).SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
				t.Fatal(err)
			}
			p := newTestProvider(t)
			name := testName()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			env := map[string]string{"GC_SESSION_ID": "owner-1", "GC_INSTANCE_TOKEN": "token-1", "GC_RUNTIME_EPOCH": "2", "PRIVATE_API_KEY": "never-persist"}
			command := fmt.Sprintf(`python3 -c 'import socket; s=socket.create_connection(("127.0.0.1", %d)); s.recv(1); s.close()'; `, listener.Addr().(*net.TCPAddr).Port) + fakeACPShellCommand()
			go func() { done <- p.Start(ctx, name, runtime.Config{Command: command, WorkDir: t.TempDir(), Env: env}) }()
			conn, err := listener.Accept()
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			defer conn.Close()
			t.Cleanup(func() { cancel(); _ = p.Stop(name) })
			if !p.IsRunning(name) {
				t.Fatal("blocked handshake must expose its live reservation")
			}
			for key, want := range env {
				if key == "PRIVATE_API_KEY" {
					continue
				}
				got, err := p.GetMeta(name, key)
				if err != nil || got != want {
					t.Errorf("live handshake %s = %q, %v; want %q", key, got, err, want)
				}
				if info, err := os.Stat(p.metaPath(name, key)); err != nil || info.Mode().Perm() != 0o600 {
					t.Errorf("identity sidecar is not private: %v", err)
				}
			}
			if got, _ := p.GetMeta(name, "PRIVATE_API_KEY"); got != "" {
				t.Error("arbitrary credential persisted")
			}
			err = p.Start(ctx, name, runtime.Config{Command: "false", Env: map[string]string{"GC_SESSION_ID": "intruder"}})
			if !errors.Is(err, runtime.ErrSessionExists) {
				t.Errorf("duplicate start = %v", err)
			}
			if got, _ := p.GetMeta(name, "GC_SESSION_ID"); got != "owner-1" {
				t.Errorf("duplicate replaced owner: %q", got)
			}
			if cancelStart {
				if err := p.Stop(name); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("canceled handshake succeeded")
					}
				case <-time.After(10 * time.Second):
					t.Fatal("canceled start did not return")
				}
				for _, key := range []string{"GC_SESSION_ID", "GC_INSTANCE_TOKEN", "GC_RUNTIME_EPOCH"} {
					if got, _ := p.GetMeta(name, key); got != "" {
						t.Errorf("canceled start retained %s", key)
					}
				}
				return
			}
			_ = conn.Close()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("start did not complete")
			}
			if got, _ := p.GetMeta(name, "GC_SESSION_ID"); got != "owner-1" {
				t.Errorf("completed start lost owner: %q", got)
			}
			if err := p.Stop(name); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"GC_SESSION_ID", "GC_INSTANCE_TOKEN", "GC_RUNTIME_EPOCH"} {
				if got, _ := p.GetMeta(name, key); got != "" {
					t.Errorf("Stop retained %s", key)
				}
			}
		})
	}
}
