package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/alllexey-dev/itmo-widgets-agent/internal/agent"
)

func TestStatusAfterRevokeShowsDisconnected(t *testing.T) {
	a := &App{Now: time.Now}
	frame := a.statusFrame(agent.Live{Stopped: agent.ErrTokenRevoked.Error(), Revoked: true}, false, false)
	for _, want := range []string{"● отключено", "[p] подключить заново   [q] выйти"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("frame lacks %q:\n%s", want, frame)
		}
	}
	if strings.Contains(frame, "[u] отключить") {
		t.Fatal("a revoked agent still offers unpair")
	}
}

func TestAttachedFrameLeavesTheAgentRunning(t *testing.T) {
	a := &App{Now: time.Now}
	live := agent.Live{Version: "1.2", Name: "nas", Running: true, Started: time.Now(), Log: []string{"a", "b", "c", "d", "e"}}
	frame := a.statusFrame(live, false, true)
	for _, want := range []string{"1.2 · nas", "ждёт подключения", "[q] закрыть экран, агент продолжит работу", "  e"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("frame lacks %q:\n%s", want, frame)
		}
	}
	if strings.Contains(frame, "  a") {
		t.Fatalf("frame shows more than 4 log lines:\n%s", frame)
	}
}
