package pair

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/alllexey-dev/itmo-widgets-agent/internal/backend"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/itmo"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/state"
)

var (
	// ErrSessionRejected: ITMO.ID refused the handed-over refresh token at the first refresh (exit code 4).
	ErrSessionRejected = errors.New("ИТМО не принял сессию. Возможно, ИТМО запретил повторное использование токенов — подключите заново позже")
	ErrRejected        = errors.New("подключение отклонено на телефоне")
	ErrExpired         = errors.New("время подключения истекло, запустите pair ещё раз")
	ErrOtherAccount    = errors.New("сессия ИТМО от другого аккаунта, чем токен агента: отключите устройство в приложении и подключите заново")
	ErrNoRefreshToken  = errors.New("в stdin нет refresh-токена ITMO.ID")
)

// Flow runs `pair` and `login`.
type Flow struct {
	Backend *backend.Client
	// ITMO is a template for the one refresh of the handed-over token; its Save is not used.
	ITMO    *itmo.Client
	Files   state.Files
	Origin  string
	Name    string
	Version string
	View    View
	Log     *slog.Logger
	Now     func() time.Time
	Sleep   func(context.Context, time.Duration) error
}

// Pair connects a new agent (kind AGENT) or, with sessionOnly, gets a new ITMO session for this agent's token.
func (f *Flow) Pair(ctx context.Context, sessionOnly bool) error {
	t, err := NewTarget()
	if err != nil {
		return err
	}
	commitment := Commitment(t.PK, t.Secret)
	kind, token := KindAgent, ""
	var ch backend.Challenge
	if sessionOnly {
		kind = KindAgentSession
		if token, err = f.Files.Token(); err != nil {
			return fmt.Errorf("нет токена агента, сначала выполните pair: %w", err)
		}
		ch, err = f.Backend.WithToken(token).CreateSessionPairing(ctx, commitment)
	} else {
		ch, err = f.Backend.CreatePairing(ctx, commitment, f.Name, f.Version)
	}
	if err != nil {
		return fmt.Errorf("сервер не создал подключение: %w", err)
	}
	id, err := ParseUUID(ch.ID)
	if err != nil {
		return err
	}
	if err := f.View.Link(Link(f.Origin, ch.Code, t.PK, t.Secret), ch.ExpiresAt); err != nil {
		return err
	}

	poll, err := f.wait(ctx, ch, SAS(id, t.PK, t.Secret))
	if err != nil {
		return err
	}
	bundle, err := t.Open(poll.SealedBundle, ch.ID, kind, f.Now())
	if err != nil {
		return err
	}
	if kind == KindAgent {
		if poll.AgentToken == nil || !strings.HasPrefix(poll.AgentToken.Token, "iwa_") {
			return errors.New("сервер не выдал токен агента")
		}
		token = poll.AgentToken.Token
	}
	return f.finish(ctx, token, kind == KindAgent, bundle.Isu, bundle.RefreshToken)
}

func (f *Flow) wait(ctx context.Context, ch backend.Challenge, sas string) (backend.Poll, error) {
	deadline := f.Now().Add(15 * time.Minute)
	shown := false
	for f.Now().Before(deadline) {
		if err := f.Sleep(ctx, 2*time.Second); err != nil {
			return backend.Poll{}, err
		}
		p, err := f.Backend.PollPairing(ctx, ch.ID, ch.PollSecret)
		if backend.StatusOf(err) == http.StatusNotFound {
			return p, ErrExpired
		}
		if err != nil {
			f.Log.Warn("pairing poll failed", "err", err)
			continue
		}
		switch p.Status {
		case "OPENED":
			if !shown {
				f.View.Opened(sas)
				shown = true
			}
		case "APPROVED":
			return p, nil
		case "REJECTED":
			return p, ErrRejected
		case "EXPIRED":
			return p, ErrExpired
		}
	}
	return backend.Poll{}, ErrExpired
}

// Login is the fallback without a QR: stdin carries the ITMO.ID refresh token and, if the agent has none yet, the
// agent token (a line starting with iwa_). Tokens never come from argv or the environment.
func (f *Flow) Login(ctx context.Context, in io.Reader) error {
	var token, refresh string
	sc := bufio.NewScanner(io.LimitReader(in, 64<<10))
	sc.Buffer(make([]byte, 16<<10), 64<<10)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "":
		case strings.HasPrefix(line, "iwa_"):
			token = line
		default:
			refresh = line
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if refresh == "" {
		return ErrNoRefreshToken
	}
	saveToken := token != ""
	if !saveToken {
		var err error
		if token, err = f.Files.Token(); err != nil {
			return fmt.Errorf("нет токена агента: передайте его строкой iwa_... в stdin: %w", err)
		}
	}
	return f.finish(ctx, token, saveToken, 0, refresh)
}

// finish checks the token, refreshes the handed-over refresh token once and only then writes anything.
func (f *Flow) finish(ctx context.Context, token string, saveToken bool, bundleIsu int64, refresh string) error {
	me, err := f.Backend.WithToken(token).Me(ctx)
	if err != nil {
		return fmt.Errorf("сервер не принял токен агента: %w", err)
	}
	if bundleIsu != 0 && bundleIsu != me.Isu {
		return ErrOtherAccount
	}
	c := *f.ITMO
	c.Save = nil
	c.SetSession(&state.Session{Isu: me.Isu, RefreshToken: refresh})
	if err := c.Refresh(ctx); err != nil {
		if errors.Is(err, itmo.ErrSessionClosed) {
			return ErrSessionRejected
		}
		return fmt.Errorf("ITMO.ID недоступен, попробуйте позже: %w", err)
	}
	if saveToken {
		if err := f.Files.SaveToken(token); err != nil {
			return err
		}
	}
	if _, err := f.Files.SaveSession(*c.Session()); err != nil {
		return err
	}
	f.Log.Info("paired", "isu", me.Isu)
	f.View.Paired(me.Isu)
	return nil
}

// View shows the pairing to the user: Text prints it for logs and pipes, the TUI draws a screen.
type View interface {
	// Link shows the QR and the link; expiresAt may be zero.
	Link(link string, expiresAt time.Time) error
	// Opened shows the verification code once the phone opened the link.
	Opened(sas string)
	Paired(isu int64)
}

// Text is the plain View: the QR and the link go to W, which may be a log.
type Text struct{ W io.Writer }

func (t Text) Link(link string, _ time.Time) error {
	fmt.Fprintln(t.W, "Откройте ITMO.Widgets на телефоне -> Профиль -> «Подключить устройство» и отсканируйте QR:")
	fmt.Fprintln(t.W)
	if err := WriteQR(t.W, link); err != nil {
		return err
	}
	_, err := fmt.Fprintf(t.W, "\nИли вставьте ссылку в приложении:\n%s\n\nСсылка действует 2 минуты, не пересылайте её.\n", link)
	return err
}

func (t Text) Opened(sas string) {
	fmt.Fprintf(t.W, "\nКод проверки: %s — сверьте его с экраном телефона и нажмите там «Подключить».\n", sas)
}

func (t Text) Paired(isu int64) {
	fmt.Fprintf(t.W, "\nПодключено (ISU %d). Запустите агент: itmo-widgets-agent run\n", isu)
}
