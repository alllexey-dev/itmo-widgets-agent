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
	inApp         = "Профиль -> «Устройства»"
)

// Unpaired is how `unpair` went; the token and the ITMO session are gone from this machine in every case.
type Unpaired struct {
	// Server: Backend revoked the connection now (200) or had already revoked it (401).
	Server bool
	// Err: why Backend did not answer (network error, 5xx, 429). Nil with Server false: there was no token.
	Err error
	// ItmoErr: why ending the agent's ITMO session failed; nil when it ended or there was none.
	ItmoErr error
}

// Message is the line for the user; with Err or ItmoErr set it is a warning.
func (u Unpaired) Message() string {
	var msg string
	switch {
	case u.Server:
		msg = "Агент отключён: сервер отозвал подключение, токен и сессия ИТМО удалены с этой машины."
	case u.Err != nil:
		msg = "Внимание: сервер не ответил (" + u.Err.Error() + "), доступ агента к серверу не закрыт. " +
			"Токен и сессия ИТМО удалены с этой машины. Отключите агент в приложении: " + inApp + "."
	default:
		msg = "Токен и сессия ИТМО удалены с этой машины. Если агент ещё в списке, отключите его в приложении: " + inApp + "."
	}
	if u.ItmoErr != nil {
		msg += " Вход агента в ИТМО не завершён (" + u.ItmoErr.Error() + "): его завершит телефон, когда вы откроете " + inApp + "."
	}
	return msg
}

// Unpair ends the agent's own ITMO session (OIDC logout with its refresh token), asks Backend to revoke the
// connection (DELETE /api/client/self), then deletes the token and the session here, whatever both answered.
func (a *Agent) Unpair(ctx context.Context) (Unpaired, error) {
	ctx, cancel := context.WithTimeout(ctx, unpairTimeout)
	defer cancel()
	var u Unpaired
	if u.ItmoErr = a.endSession(ctx); u.ItmoErr != nil {
		a.Log.Warn("ending the ITMO session failed", "err", u.ItmoErr)
	}
	if token, err := a.Files.Token(); err == nil {
		u.Server, u.Err = classifyRevoke(a.Backend.WithToken(token).RevokeSelf(ctx))
	} else if !errors.Is(err, os.ErrNotExist) {
		a.Log.Warn("connection token unreadable, deleting it locally only", "err", err)
	}
	if u.Err != nil {
		a.Log.Warn("backend did not revoke the connection", "err", u.Err)
	}
	return u, a.Files.Wipe()
}

// endSession logs the stored ITMO session out at ITMO.ID; nil when there is none. A session without a sid is from a
// 0.1.x pairing and shared with the phone: logging it out would sign the phone out, so it is only deleted.
func (a *Agent) endSession(ctx context.Context) error {
	s, _, ok, err := a.Files.Session()
	if err != nil || !ok || s.Sid == "" {
		return err
	}
	return a.ITMO.Logout(ctx, s.RefreshToken)
}

// classifyRevoke: 200 revoked it, 401 means it was revoked already.
func classifyRevoke(err error) (bool, error) {
	if err == nil || backend.StatusOf(err) == http.StatusUnauthorized {
		return true, nil
	}
	return false, err
}
