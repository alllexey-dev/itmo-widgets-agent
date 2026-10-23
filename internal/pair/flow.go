package pair

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/alllexey-dev/itmo-widgets-agent/internal/backend"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/itmo"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/state"
)

var (
	// ErrSessionRejected: ITMO.ID refused the new session at its first refresh (exit code 4).
	ErrSessionRejected = errors.New("ИТМО не принял новую сессию: запустите команду ещё раз")
	ErrExpired         = errors.New("время подключения истекло или его отменили на телефоне, запустите команду ещё раз")
	ErrOtherAccount    = errors.New("сессия ИТМО от другого аккаунта, чем подключение: запустите команду ещё раз")
	ErrPaired          = errors.New("агент уже подключён: renew обновит вход в ИТМО, unpair отключит агент")
)

// Flow runs `pair` and `renew`.
type Flow struct {
	Backend *backend.Client
	// ITMO is a template for the one refresh of the new session; its Save is not used.
	ITMO    *itmo.Client
	Files   state.Files
	Origin  string
	Name    string
	Version string
	// Scopes asked for at CONNECT; none means the agent's defaults.
	Scopes []string
	View   View
	Log    *slog.Logger
	Now    func() time.Time
	Sleep  func(context.Context, time.Duration) error
}

// Pair connects this agent (CONNECT) or, with renew, gets a new ITMO session for its connection (RENEW).
func (f *Flow) Pair(ctx context.Context, renew bool) error {
	t, err := NewTarget()
	if err != nil {
		return err
	}
	commitment := Commitment(t.PK, t.Secret)
	token, tokenErr := f.Files.Token()
	kind := KindConnect
	var ch backend.Challenge
	switch {
	case renew && tokenErr != nil:
		return fmt.Errorf("агент не подключён, сначала выполните pair: %w", tokenErr)
	case renew:
		kind = KindRenew
		ch, err = f.Backend.WithToken(token).RenewPairing(ctx, commitment)
	case tokenErr == nil:
		return ErrPaired
	default:
		ch, err = f.Backend.CreatePairing(ctx, backend.ChallengeRequest{Kind: KindConnect, Client: "agent",
			KeyCommitment: commitment, Name: f.Name, Version: f.Version, Platform: Platform(), Scopes: f.Scopes})
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

	claim, err := f.wait(ctx, ch, SAS(id, t.PK, t.Secret))
	if err != nil {
		return err
	}
	newToken := ""
	if !renew {
		if !strings.HasPrefix(claim.Token, "iwc_") {
			return errors.New("сервер не выдал токен подключения")
		}
		token, newToken = claim.Token, claim.Token
	}
	bundle, err := t.Open(claim.SealedBundle, ch.ID, kind, f.Now())
	if err != nil {
		f.abandon(ctx, newToken, "")
		return err
	}
	c := *f.ITMO
	c.Save = nil
	c.SetSession(&state.Session{Isu: bundle.Isu, Sid: bundle.Itmo.Sid, ExpiresAt: bundle.Itmo.ExpiresAt,
		RefreshToken: bundle.Itmo.RefreshToken, RefreshExpiresAt: bundle.Itmo.RefreshExpiresAt})
	if err := f.finish(ctx, token, newToken != "", &c); err != nil {
		f.abandon(ctx, newToken, c.Session().RefreshToken)
		return err
	}
	return nil
}

// Platform is the challenge's platform; the phone picks the browser label of the ITMO.ID sign-in by it.
func Platform() string {
	if runtime.GOOS == "darwin" {
		return "macos"
	}
	return runtime.GOOS
}

// wait claims the challenge every 2 seconds until the phone approves it; Backend answers 404 once it expired.
func (f *Flow) wait(ctx context.Context, ch backend.Challenge, sas string) (backend.Claim, error) {
	deadline := f.Now().Add(15 * time.Minute)
	shown := false
	for f.Now().Before(deadline) {
		if err := f.Sleep(ctx, 2*time.Second); err != nil {
			return backend.Claim{}, err
		}
		c, err := f.Backend.ClaimPairing(ctx, ch.ID, ch.ClaimSecret)
		if backend.StatusOf(err) == http.StatusNotFound {
			return c, ErrExpired
		}
		if err != nil {
			f.Log.Warn("pairing claim failed", "err", err)
			continue
		}
		switch c.Status {
		case "OPENED":
			if !shown {
				f.View.Opened(sas)
				shown = true
			}
		case "CLAIMED":
			return c, nil
		}
	}
	return backend.Claim{}, ErrExpired
}

// finish checks the token and the account, refreshes the new session once and only then writes anything. A renewal
// ends the replaced session too, in case the phone could not.
func (f *Flow) finish(ctx context.Context, token string, saveToken bool, c *itmo.Client) error {
	me, err := f.Backend.WithToken(token).Me(ctx)
	if err != nil {
		return fmt.Errorf("сервер не принял токен подключения: %w", err)
	}
	if c.Session().Isu != me.Isu {
		return ErrOtherAccount
	}
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
	old, _, hadOld, _ := f.Files.Session()
	if _, err := f.Files.SaveSession(*c.Session()); err != nil {
		return err
	}
	if hadOld && old.Sid != "" && old.Sid != c.Session().Sid {
		if err := c.Logout(ctx, old.RefreshToken); err != nil {
			f.Log.Warn("ending the replaced ITMO session failed; the phone ends it", "err", err)
		}
	}
	f.Log.Info("paired", "isu", me.Isu)
	f.View.Paired(me.Isu)
	return nil
}

// abandon undoes a claimed pairing that failed its checks, best effort: the new ITMO session ends, and a new
// connection is revoked so it does not hold one of the five places.
func (f *Flow) abandon(ctx context.Context, newToken, refresh string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if refresh != "" {
		if err := f.ITMO.Logout(ctx, refresh); err != nil {
			f.Log.Warn("ending the new ITMO session failed", "err", err)
		}
	}
	if newToken != "" {
		if err := f.Backend.WithToken(newToken).RevokeSelf(ctx); err != nil {
			f.Log.Warn("revoking the new connection failed", "err", err)
		}
	}
}

// View shows the pairing to the user: Text prints it for logs and pipes, the TUI draws a screen.
type View interface {
	// Link shows the QR and the link; expiresAt may be zero.
	Link(link string, expiresAt time.Time) error
	// Opened shows the verification code once the phone opened the link.
	Opened(sas string)
	Paired(isu int64)
}

// Text is the plain View: the QR and the link go to W, which may be a log. Renew words it for `renew`.
type Text struct {
	W     io.Writer
	Renew bool
}

func (t Text) Link(link string, _ time.Time) error {
	where := "Профиль -> «Устройства» -> «Подключить устройство»"
	if t.Renew {
		where = "Профиль -> «Устройства» -> этот агент -> «Обновить вход»"
	}
	fmt.Fprintf(t.W, "Откройте ITMO.Widgets на телефоне: %s и отсканируйте QR:\n\n", where)
	if err := WriteQR(t.W, link); err != nil {
		return err
	}
	_, err := fmt.Fprintf(t.W, "\nИли вставьте ссылку в приложении:\n%s\n\nСсылка действует 2 минуты, не пересылайте её.\n", link)
	return err
}

func (t Text) Opened(sas string) {
	fmt.Fprintf(t.W, "\nКод проверки: %s — сверьте его с экраном телефона и войдите там в ИТМО.\n", sas)
}

func (t Text) Paired(isu int64) {
	if t.Renew {
		fmt.Fprintf(t.W, "\nВход в ИТМО обновлён (ISU %d). Запущенный агент подхватит его сам.\n", isu)
		return
	}
	fmt.Fprintf(t.W, "\nПодключено (ISU %d). Запустите агент: itmo-widgets-agent run\n", isu)
}
