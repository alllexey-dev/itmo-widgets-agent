package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/alllexey-dev/itmo-widgets-agent/internal/agent"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/state"
)

func TestStatusAfterRevokeShowsDisconnected(t *testing.T) {
	a := &App{Ring: &Ring{}, Now: time.Now}
	frame := a.statusFrame(state.Files{Dir: t.TempDir()}, time.Now(), false, agent.ErrTokenRevoked, false)
	for _, want := range []string{"● отключено", "[p] подключить заново   [q] выйти"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("frame lacks %q:\n%s", want, frame)
		}
	}
	if strings.Contains(frame, "[u] отключить") {
		t.Fatal("a revoked agent still offers unpair")
	}
}
