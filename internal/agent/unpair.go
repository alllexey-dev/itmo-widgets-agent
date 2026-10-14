package agent

import (
	"context"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/alllexey-dev/itmo-widgets-agent/internal/backend"
)

const (
	unpairTimeout = 10 * time.Second
	removeInApp   = "Чтобы закрыть агенту доступ к серверу, удалите его в приложении: Профиль -> «Подключённые устройства»."
)

// Unpaired is how `unpair` went; the token and the ITMO session are gone from this machine in every case.
type Unpaired struct {
	// Server: Backend revoked the token now (200) or had already revoked it (401).
	Server bool
	// Err: why Backend did not answer (network error, 5xx, 429). Nil with Server false: there was no token, or Backend
	// has no self-revoke route yet (404, 405, or 403 from its route allow-list).
	Err error
}

// Message is the line for the user; with Err set it is a warning.
func (u Unpaired) Message() string {
	switch {
	case u.Server:
		return "Агент отключён: сервер отозвал токен, токен и сессия ИТМО удалены с этой машины."
	case u.Err != nil:
		return "Внимание: сервер не ответил (" + u.Err.Error() + "), доступ агента к серверу не закрыт. " +
			"Токен агента и сессия ИТМО удалены с этой машины. " + removeInApp
	default:
		return "Токен агента и сессия ИТМО удалены с этой машины. " + removeInApp
	}
}

// Unpair asks Backend to revoke the token (DELETE /api/agent/self), then deletes the token and the ITMO session here,
// whatever Backend answered. It never calls ITMO.ID: the session is shared with the phone.
func (a *Agent) Unpair(ctx context.Context) (Unpaired, error) {
	var u Unpaired
	if token, err := a.Files.Token(); err == nil {
		ctx, cancel := context.WithTimeout(ctx, unpairTimeout)
		defer cancel()
		u = classifyRevoke(a.Backend.WithToken(token).RevokeSelf(ctx))
	} else if !errors.Is(err, os.ErrNotExist) {
		a.Log.Warn("agent token unreadable, deleting it locally only", "err", err)
	}
	if u.Err != nil {
		a.Log.Warn("backend did not revoke the agent token", "err", u.Err)
	}
	return u, a.Files.Wipe()
}

func classifyRevoke(err error) Unpaired {
	switch backend.StatusOf(err) {
	case 0:
		if err == nil {
			return Unpaired{Server: true}
		}
		return Unpaired{Err: err}
	case http.StatusUnauthorized:
		return Unpaired{Server: true}
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusForbidden:
		// A Backend without the route: its allow-list answers 403 before any controller.
		return Unpaired{}
	default:
		return Unpaired{Err: err}
	}
}
