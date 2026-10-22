package agent

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alllexey-dev/itmo-widgets-agent/internal/fake"
)

func TestUnpair(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(h *harness)
		wantServer bool
		wantErr    bool
		wantItmo   bool
		wantText   string
	}{
		{name: "200 revokes on the server", wantServer: true, wantText: "сервер отозвал подключение"},
		{name: "401: already revoked", setup: func(h *harness) { h.b.Token = "iwc_other" }, wantServer: true},
		{name: "503: a warning", setup: func(h *harness) { h.b.SelfStatus = 503 }, wantErr: true, wantText: "Внимание"},
		{name: "network error: a warning", setup: func(h *harness) { h.b.Srv.Close() }, wantErr: true,
			wantText: "доступ агента к серверу не закрыт"},
		{name: "ITMO session already ended", wantServer: true, setup: func(h *harness) {
			h.f.Set(func(f *fake.ITMO) { f.LogoutStatus, f.LogoutBody = 400, `{"error":"invalid_grant"}` })
		}},
		{name: "ITMO.ID down: a warning", wantServer: true, wantItmo: true, wantText: "не завершён", setup: func(h *harness) {
			h.f.Set(func(f *fake.ITMO) { f.LogoutStatus = 503 })
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, true)
			if tt.setup != nil {
				tt.setup(h)
			}
			u, err := h.a.Unpair(h.ctx)
			if err != nil {
				t.Fatal(err)
			}
			if u.Server != tt.wantServer || (u.Err != nil) != tt.wantErr || (u.ItmoErr != nil) != tt.wantItmo {
				t.Fatalf("got %+v, want server %v, error %v, itmo error %v", u, tt.wantServer, tt.wantErr, tt.wantItmo)
			}
			if !strings.Contains(u.Message(), tt.wantText) {
				t.Fatalf("message %q lacks %q", u.Message(), tt.wantText)
			}
			for _, p := range []string{h.a.Files.TokenPath, h.a.Files.SessionPath()} {
				if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("%s not wiped", p)
				}
			}
			if strings.Contains(h.logs.String(), fake.AgentToken) || strings.Contains(u.Message(), fake.AgentToken) {
				t.Fatal("the token leaked into a log or the message")
			}
			if got := h.f.LoggedOut(); !slices.Equal(got, []string{"SECRET-REFRESH-0"}) {
				t.Fatalf("logged out %v, want the agent's own session", got)
			}
			if _, refreshes := h.f.Snapshot(); refreshes != 0 || h.f.MyCalls != 0 {
				t.Fatal("My ITMO was called while unpairing")
			}
		})
	}
}

func TestUnpairRevokesOnceAndARunningLoopThenStops(t *testing.T) {
	h := newHarness(t, false)
	if u, err := h.a.Unpair(h.ctx); err != nil || !u.Server || h.b.SelfRevokes != 1 {
		t.Fatalf("unpair %+v %v, revokes %d", u, err, h.b.SelfRevokes)
	}
	// A second unpair has no token left and asks nothing.
	if u, err := h.a.Unpair(h.ctx); err != nil || u.Server || u.Err != nil || h.b.SelfRevokes != 1 {
		t.Fatalf("second unpair %+v %v, revokes %d", u, err, h.b.SelfRevokes)
	}
	// A service still holding the old token gets 401 on its next poll and stops with exit code 3.
	h.a.Backend = h.a.Backend.WithToken(fake.AgentToken)
	if err := h.a.Files.SaveToken(fake.AgentToken); err != nil {
		t.Fatal(err)
	}
	if err := h.a.Run(h.ctx); !errors.Is(err, ErrTokenRevoked) {
		t.Fatalf("want ErrTokenRevoked, got %v", err)
	}
}

func TestRunWaitsForATokenInsteadOfExiting(t *testing.T) {
	h := newHarness(t, false)
	if err := os.Remove(h.a.Files.TokenPath); err != nil {
		t.Fatal(err)
	}
	var waits []time.Duration
	h.a.Sleep = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		if len(waits) == 2 {
			// `pair` in another process writes the token.
			if err := h.a.Files.SaveToken(fake.AgentToken); err != nil {
				t.Fatal(err)
			}
		}
		return ctx.Err()
	}
	if err := h.a.Run(h.ctx); err != nil {
		t.Fatal(err)
	}
	if len(waits) < 2 || waits[0] != tokenWait || waits[1] != tokenWait || len(h.b.Afters) == 0 {
		t.Fatalf("waits %v, polls %v", waits, h.b.Afters)
	}
	if n := strings.Count(h.logs.String(), "no agent token"); n != 1 {
		t.Fatalf("logged the wait %d times", n)
	}
}

func TestRunWithoutATokenEndsWithItsContext(t *testing.T) {
	h := newHarness(t, false)
	if err := os.Remove(h.a.Files.TokenPath); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(h.ctx)
	cancel()
	if err := h.a.Run(ctx); err != nil {
		t.Fatal(err)
	}
}
