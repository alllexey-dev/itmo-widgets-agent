// Package agent is the poll loop: it long-polls Backend events and hands them to the features.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"time"

	"github.com/alllexey-dev/itmo-widgets-agent/internal/backend"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/budget"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/config"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/itmo"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/notify"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/pair"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/sport"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/state"
)

const (
	pollWait       = 30
	itmoPerHour    = 60
	keepAlive      = 24 * time.Hour
	maxBackoff     = 5 * time.Minute
	rateLimitPause = time.Minute
	accessOffPause = time.Hour
	healthyPoll    = 2 * time.Minute
)

// ErrTokenRevoked: Backend answered 401, the token and the session were wiped (exit code 3).
var ErrTokenRevoked = errors.New("токен агента отозван: токен и сессия ИТМО удалены, подключите агент заново (pair)")

type Agent struct {
	Files    state.Files
	Backend  *backend.Client
	ITMO     *itmo.Client
	Sport    *sport.Executor
	Notify   *notify.Ntfy
	Features map[string]bool
	Log      *slog.Logger
	Now      func() time.Time
	Sleep    func(context.Context, time.Duration) error

	// handle processes one event; tests replace it.
	handle      func(context.Context, backend.Event) error
	st          state.State
	loadedMtime int64
	sessionOK   bool
	jitter      func(time.Duration) time.Duration
}

// Build wires the real clients. now is the only clock; tests pass a fake one.
func Build(cfg config.Config, version string, log *slog.Logger, now func() time.Time) *Agent {
	ua := "itmo-widgets-agent/" + version
	plain := &http.Client{Transport: http.DefaultTransport}
	itmoHTTP := &http.Client{
		Transport:     budget.New(http.DefaultTransport, itmoPerHour, time.Hour, now),
		CheckRedirect: itmo.NoRedirects,
	}
	a := &Agent{
		Files:    state.Files{Dir: cfg.StateDir, TokenPath: cfg.TokenFile},
		Backend:  &backend.Client{BaseURL: cfg.BackendURL, UserAgent: ua, HTTP: plain},
		ITMO:     &itmo.Client{HTTP: itmoHTTP, TokenURL: itmo.DefaultTokenURL, BaseURL: itmo.DefaultBaseURL, UserAgent: ua, Now: now},
		Notify:   &notify.Ntfy{URL: cfg.NtfyURL, UserAgent: ua, HTTP: plain},
		Features: cfg.Features,
		Log:      log,
		Now:      now,
		Sleep:    sleep,
	}
	a.ITMO.Save = func(s state.Session) error {
		m, err := a.Files.SaveSession(s)
		if err == nil {
			a.loadedMtime = m
		}
		return err
	}
	a.Sport = &sport.Executor{Backend: a.Backend, ITMO: a.ITMO, Notify: a.Notify, Log: log, Now: now,
		Record: func(at state.SportAttempt) { a.st.AddSport(at) }}
	a.jitter = Jitter
	return a
}

// PairFlow returns the pairing flow over the same clients.
func (a *Agent) PairFlow(name, version string, view pair.View) *pair.Flow {
	u, _ := url.Parse(a.Backend.BaseURL)
	return &pair.Flow{
		Backend: a.Backend, ITMO: a.ITMO, Files: a.Files, Origin: u.Scheme + "://" + u.Host,
		Name: name, Version: version, View: view, Log: a.Log, Now: a.Now, Sleep: a.Sleep,
	}
}

// Run polls until ctx ends or the token is revoked.
func (a *Agent) Run(ctx context.Context) error {
	token, err := a.Files.Token()
	if err != nil {
		return fmt.Errorf("нет токена агента, сначала выполните pair: %w", err)
	}
	a.Backend = a.Backend.WithToken(token)
	a.Sport.Backend = a.Backend
	if a.handle == nil {
		a.handle = a.handleEvent
	}
	if a.st, err = a.Files.State(); err != nil {
		a.Log.Warn("state.json unreadable, starting from the beginning", "err", err)
	}
	a.Log.Info("agent started", "cursor", a.st.Cursor)
	var backoff time.Duration
	for ctx.Err() == nil {
		a.checkSession(ctx)
		page, err := a.Backend.Events(ctx, a.st.Cursor, pollWait, a.sessionOK)
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			if pause, stop := a.pollFailed(err, &backoff); stop != nil {
				return stop
			} else if a.Sleep(ctx, pause) != nil {
				break
			}
			continue
		}
		backoff = 0
		a.st.LastPollAt, a.st.LastError = a.Now(), ""
		if page.Gap {
			a.Log.Warn("event gap: older events were dropped; sport entries are rechecked per event")
		}
		for _, ev := range page.Events {
			if ctx.Err() != nil {
				return nil
			}
			if !a.st.HasSeen(ev.ID) {
				a.st.AddEvent(state.EventRecord{At: a.Now(), Seq: ev.Seq, Type: ev.Type})
				if err := a.handle(ctx, ev); err != nil {
					a.Log.Warn("event handling failed", "type", ev.Type, "seq", ev.Seq, "err", err)
				}
				a.st.MarkSeen(ev.ID)
			}
			a.st.Cursor = ev.Seq
			a.saveState()
		}
		a.st.Cursor = page.NextCursor
		a.saveState()
		if !page.HasMore && a.Sleep(ctx, time.Second) != nil {
			break
		}
	}
	return nil
}

// pollFailed decides the pause after a failed poll, or stops the agent on 401.
func (a *Agent) pollFailed(err error, backoff *time.Duration) (time.Duration, error) {
	a.st.LastError = err.Error()
	a.saveState()
	switch status := backend.StatusOf(err); {
	case status == http.StatusUnauthorized:
		// Never call ITMO.ID here: the session is shared with the phone.
		if werr := a.Files.Wipe(); werr != nil {
			a.Log.Error("wiping token and session failed", "err", werr)
		}
		a.Log.Error("agent token revoked; token and ITMO session deleted")
		return 0, ErrTokenRevoked
	case status == http.StatusTooManyRequests:
		a.Log.Warn("rate limited by backend")
		return rateLimitPause, nil
	case status == http.StatusForbidden && backend.CodeOf(err) == "permission_denied":
		a.Log.Warn("agent access is off for this account; retrying hourly")
		return accessOffPause, nil
	default:
		*backoff = NextBackoff(*backoff)
		pause := a.jitter(*backoff)
		a.Log.Warn("poll failed", "err", err, "retryIn", pause.String())
		return pause, nil
	}
}

// Jitter spreads a pause over [d/2, d], so many agents do not reconnect in step after an outage.
func Jitter(d time.Duration) time.Duration {
	return d/2 + rand.N(d/2+1)
}

// NextBackoff doubles from 1 s up to 5 min.
func NextBackoff(d time.Duration) time.Duration {
	if d <= 0 {
		return time.Second
	}
	return min(2*d, maxBackoff)
}

// checkSession loads a new or changed session file, keeps a closed one closed until the file changes, and refreshes
// the token at least daily.
func (a *Agent) checkSession(ctx context.Context) {
	mtime, exists := a.Files.SessionMtime()
	switch {
	case !exists:
		if a.sessionOK || a.loadedMtime != 0 {
			a.Log.Info("no ITMO session file")
		}
		a.ITMO.SetSession(nil)
		a.sessionOK, a.loadedMtime = false, 0
		return
	case a.st.SessionClosedMtime != 0 && mtime == a.st.SessionClosedMtime:
		a.sessionOK = false
		return
	case mtime != a.loadedMtime:
		s, m, ok, err := a.Files.Session()
		a.loadedMtime = mtime
		if err != nil || !ok {
			a.Log.Warn("ITMO session file unreadable", "err", err)
			a.sessionOK = false
			return
		}
		a.loadedMtime = m
		if a.st.SessionClosedMtime != 0 {
			a.st.SessionClosedMtime = 0
			a.saveState()
			a.Log.Info("new ITMO session file, resuming")
		}
		a.ITMO.SetSession(&s)
		a.sessionOK = true
	}
	if s := a.ITMO.Session(); a.sessionOK && s != nil && a.Now().Sub(s.RefreshedAt) >= keepAlive {
		if err := a.ITMO.Refresh(ctx); errors.Is(err, itmo.ErrSessionClosed) {
			a.closeSession(ctx)
		} else if err != nil {
			a.Log.Warn("ITMO keep-alive refresh failed", "err", err)
		}
	}
}

// closeSession: stop MyITMO calls, notify once, poll with itmo=none until a new session file appears.
func (a *Agent) closeSession(ctx context.Context) {
	if !a.sessionOK {
		return
	}
	mtime, _ := a.Files.SessionMtime()
	a.st.SessionClosedMtime = max(mtime, 1)
	a.saveState()
	a.sessionOK = false
	a.ITMO.SetSession(nil)
	a.Log.Warn("ITMO session closed; waiting for a new session (pair --session)")
	if err := a.Notify.Send(ctx, "ITMO.Widgets", "Сессия ИТМО закрыта, переподключите с телефона: itmo-widgets-agent pair --session"); err != nil {
		a.Log.Warn("notification failed", "err", err)
	}
}

func (a *Agent) handleEvent(ctx context.Context, ev backend.Event) error {
	log := a.Log.With("type", ev.Type, "seq", ev.Seq)
	var queue string
	switch ev.Type {
	case "SPORT_FREE_SIGN_LESSONS_PAYLOAD":
		queue = "free"
	case "SPORT_AUTO_SIGN_LESSONS_PAYLOAD":
		queue = "auto"
	case "FRIENDSHIP_EVENT_PAYLOAD":
		if a.Features["friends"] {
			a.friendship(ctx, ev.Payload)
		}
		return nil
	default:
		log.Info("event type ignored")
		return nil
	}
	if !a.Features["sport"] {
		return nil
	}
	if !a.sessionOK {
		log.Info("sport event skipped: no working ITMO session")
		return nil
	}
	if !ev.ExpiresAt.IsZero() && !a.Now().Before(ev.ExpiresAt) {
		log.Info("sport event expired, skipped")
		return nil
	}
	err := a.Sport.Handle(ctx, queue, ev.Payload)
	if errors.Is(err, itmo.ErrSessionClosed) {
		a.closeSession(ctx)
		return nil
	}
	return err
}

func (a *Agent) friendship(ctx context.Context, payload json.RawMessage) {
	var p struct {
		Event string `json:"event"`
		User  struct {
			Name string `json:"name"`
		} `json:"user"`
	}
	if json.Unmarshal(payload, &p) != nil {
		a.Log.Warn("malformed friendship payload skipped")
		return
	}
	var title string
	switch p.Event {
	case "REQUEST_RECEIVED":
		title = "Новая заявка в друзья"
	case "REQUEST_ACCEPTED":
		title = "Заявка в друзья принята"
	default:
		return
	}
	if err := a.Notify.Send(ctx, title, p.User.Name); err != nil {
		a.Log.Warn("notification failed", "err", err)
	}
}

func (a *Agent) saveState() {
	if err := a.Files.SaveState(a.st); err != nil {
		a.Log.Error("saving state.json failed", "err", err)
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Health is nil when the last poll succeeded within 2 minutes and the ITMO session is not closed.
func Health(files state.Files, now time.Time) error {
	st, err := files.State()
	if err != nil {
		return err
	}
	if now.Sub(st.LastPollAt) > healthyPoll {
		return errors.New("no successful poll in the last 2 minutes")
	}
	if m, ok := files.SessionMtime(); ok && st.SessionClosedMtime != 0 && m == st.SessionClosedMtime {
		return errors.New("ITMO session closed")
	}
	return nil
}
