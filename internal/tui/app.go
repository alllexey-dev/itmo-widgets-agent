package tui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/alllexey-dev/itmo-widgets-agent/internal/agent"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/config"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/pair"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/state"
)

// ErrCancelled: the user left the pairing screen.
var ErrCancelled = errors.New("подключение отменено")

// App draws the screens; Log writes into Ring.
type App struct {
	Cfg     config.Config
	Version string
	Log     *slog.Logger
	Ring    *Ring
	Now     func() time.Time

	scr    *screen
	notice string
}

// Pair is `pair` on a terminal: the pairing screen until it succeeds or the user quits.
func (a *App) Pair(ctx context.Context, sessionOnly bool) (isu int64, err error) {
	if a.scr, err = openScreen(); err != nil {
		return 0, err
	}
	defer a.scr.close()
	return a.pair(ctx, sessionOnly)
}

// Run is `run` on a terminal: pairing when there is no token, then the agent with the status screen.
func (a *App) Run(ctx context.Context, ag *agent.Agent) (err error) {
	if a.scr, err = openScreen(); err != nil {
		return err
	}
	defer a.scr.close()
	for ctx.Err() == nil {
		if _, err := ag.Files.Token(); err != nil {
			if _, err := a.pair(ctx, false); errors.Is(err, ErrCancelled) || ctx.Err() != nil {
				return nil
			} else if err != nil {
				return err
			}
		}
		if again, err := a.status(ctx, ag); !again {
			return err
		}
	}
	return nil
}

func (a *App) header(title string) string {
	return bold("ITMO.Widgets agent") + dim(fmt.Sprintf(" %s · %s", a.Version, a.Cfg.Name)) + "\n" +
		dim(strings.Repeat("─", 60)) + "\n" + bold(title) + "\n"
}

// pairView is the pair.View of the screen; the flow calls it from its own goroutine.
type pairView struct {
	mu      sync.Mutex
	link    string
	expires time.Time
	sas     string
	isu     int64
	changed chan struct{}
}

func (v *pairView) set(f func()) {
	v.mu.Lock()
	f()
	v.mu.Unlock()
	select {
	case v.changed <- struct{}{}:
	default:
	}
}

func (v *pairView) Link(link string, exp time.Time) error {
	v.set(func() { v.link, v.expires = link, exp })
	return nil
}
func (v *pairView) Opened(sas string) { v.set(func() { v.sas = sas }) }
func (v *pairView) Paired(isu int64)  { v.set(func() { v.isu = isu }) }

// pair shows the pairing screen; on a failure it offers another try.
func (a *App) pair(ctx context.Context, sessionOnly bool) (int64, error) {
	for {
		isu, err := a.pairOnce(ctx, sessionOnly)
		if err == nil || errors.Is(err, ErrCancelled) || ctx.Err() != nil {
			return isu, err
		}
		a.scr.draw(a.pairFrame(&pairView{}, sessionOnly, err, 0))
		if !a.waitKey(ctx, 'p') {
			return 0, err
		}
	}
}

// waitKey waits for yes (true) or q and the end of ctx (false).
func (a *App) waitKey(ctx context.Context, yes rune) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case k := <-a.scr.keys:
			if k == yes || k == 'q' {
				return k == yes
			}
		}
	}
}

func (a *App) pairOnce(ctx context.Context, sessionOnly bool) (int64, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	v := &pairView{changed: make(chan struct{}, 1)}
	// Own clients: the agent loop may run next to this flow.
	flow := agent.Build(a.Cfg, a.Version, a.Log, a.Now).PairFlow(a.Cfg.Name, a.Version, v)
	done := make(chan error, 1)
	go func() { done <- flow.Pair(ctx, sessionOnly) }()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for frame := 0; ; frame++ {
		a.scr.draw(a.pairFrame(v, sessionOnly, nil, frame))
		select {
		case err := <-done:
			v.mu.Lock()
			defer v.mu.Unlock()
			return v.isu, err
		case k := <-a.scr.keys:
			if k == 'q' {
				cancel()
				<-done
				return 0, ErrCancelled
			}
		case <-v.changed:
		case <-tick.C:
		case <-ctx.Done():
			<-done
			return 0, ctx.Err()
		}
	}
}

var spinner = []string{"◐", "◓", "◑", "◒"}

func (a *App) pairFrame(v *pairView, sessionOnly bool, failed error, frame int) string {
	v.mu.Lock()
	defer v.mu.Unlock()
	title := "Подключение агента"
	if sessionOnly {
		title = "Новая сессия ИТМО"
	}
	var b strings.Builder
	b.WriteString(a.header(title))
	if a.notice != "" {
		b.WriteString(yellow(a.notice) + "\n")
	}
	b.WriteString("\n")
	switch {
	case failed != nil:
		b.WriteString(red("✗ "+failed.Error()) + "\n\n")
		b.WriteString(dim("[p] ещё раз   [q] выйти"))
		return b.String()
	case v.link == "":
		b.WriteString(spinner[frame%len(spinner)] + " Создаём подключение…\n")
	case v.isu != 0:
		b.WriteString(green(fmt.Sprintf("✓ Подключено (ISU %d)", v.isu)) + "\n")
	case v.sas != "":
		b.WriteString("Телефон открыл ссылку. Код проверки:\n\n")
		b.WriteString("    " + bold(cyan(v.sas)) + "\n\n")
		b.WriteString("Сверьте его с экраном телефона и нажмите там «Подключить».\n")
		b.WriteString(spinner[frame%len(spinner)] + " Ждём подтверждения…\n")
	default:
		b.WriteString("На телефоне: ITMO.Widgets -> Профиль -> «Подключить устройство»,\nотсканируйте QR или вставьте ссылку.\n\n")
		var qr strings.Builder
		if pair.WriteQR(&qr, v.link) == nil {
			b.WriteString(qr.String())
		}
		b.WriteString("\n" + cyan(v.link) + "\n")
		left := "Не пересылайте ссылку."
		if !v.expires.IsZero() {
			if d := v.expires.Sub(a.Now()); d > 0 {
				left += fmt.Sprintf(" Действует ещё %d:%02d.", int(d.Minutes()), int(d.Seconds())%60)
			} else {
				left = "Ссылка истекла, нажмите [q] и подключите заново."
			}
		}
		b.WriteString(dim(left) + "\n\n")
		b.WriteString(spinner[frame%len(spinner)] + " Ждём телефон…\n")
	}
	b.WriteString("\n" + dim("[q] отмена"))
	return b.String()
}

// status runs the agent under the status screen. It returns true to go back to pairing, false to quit; after a 401 it
// stays on an «отключено» screen and quitting returns agent.ErrTokenRevoked (exit code 3).
func (a *App) status(ctx context.Context, ag *agent.Agent) (bool, error) {
	loopCtx, stop := context.WithCancel(ctx)
	defer stop()
	started := a.Now()
	done := make(chan error, 1)
	go func() { done <- ag.Run(loopCtx) }()
	running, confirm := true, false
	var loopErr error
	halt := func() {
		stop()
		if running {
			loopErr, running = <-done, false
		}
	}
	revoked := func() error {
		if errors.Is(loopErr, agent.ErrTokenRevoked) {
			return loopErr
		}
		return nil
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		a.scr.draw(a.statusFrame(ag.Files, started, running, loopErr, confirm))
		select {
		case <-ctx.Done():
			halt()
			return false, revoked()
		case loopErr = <-done:
			running = false
			if revoked() != nil {
				a.notice = "Агент отключён на сервере: токен и сессия ИТМО удалены с этой машины."
			}
		case <-tick.C:
		case k := <-a.scr.keys:
			gone := revoked() != nil
			switch {
			case k == 'q':
				halt()
				return false, revoked()
			case gone && k == 'p':
				a.notice = ""
				return true, nil
			case gone:
			case k == 's' && !confirm:
				a.notice = ""
				if _, err := a.pair(ctx, true); err == nil {
					a.notice = "Новая сессия ИТМО сохранена."
				}
			case k == 'u' && !confirm:
				confirm = true
			case k == 'y' && confirm:
				halt()
				a.notice, confirm = "Отключаем агент на сервере…", false
				a.scr.draw(a.statusFrame(ag.Files, started, running, loopErr, confirm))
				u, err := ag.Unpair(ctx)
				if err != nil {
					a.notice = "Не удалось удалить файлы: " + err.Error()
					continue
				}
				a.notice = u.Message()
				return true, nil
			default:
				confirm = false
			}
		}
	}
}

func (a *App) statusFrame(files state.Files, started time.Time, running bool, loopErr error, confirm bool) string {
	now := a.Now()
	var b strings.Builder
	b.WriteString(a.header("Состояние"))
	if a.notice != "" {
		b.WriteString(yellow(a.notice) + "\n")
	}
	b.WriteString("\n")
	r, err := agent.ReadReport(files)
	if err != nil {
		b.WriteString(red("state.json не читается: "+err.Error()) + "\n")
	}
	conn := green("● на связи")
	switch {
	case errors.Is(loopErr, agent.ErrTokenRevoked):
		conn = red("● отключено") + dim(": сервер отозвал токен агента")
	case !running && loopErr != nil:
		conn = red("● остановлен: " + loopErr.Error())
	case !running:
		conn = dim("● остановлен")
	case r.LastError != "":
		conn = red("● нет связи") + dim(": "+r.LastError)
	case r.LastPoll.Before(started) || !r.Online(now):
		conn = yellow("● подключаемся…")
	}
	row(&b, "Сервер", conn)
	switch r.Session {
	case agent.SessionOK:
		s := green("действует")
		if !r.Expires.IsZero() {
			s += fmt.Sprintf(" до %s (ещё %s)", r.Expires.Local().Format("02.01 15:04"), days(r.Expires.Sub(now)))
		}
		row(&b, "Сессия ИТМО", s+dim(fmt.Sprintf(", ISU %d, обновлена %s", r.Isu, ago(now, r.Refreshed))))
	case agent.SessionClosed:
		row(&b, "Сессия ИТМО", red("закрыта ИТМО")+" - нажмите [s]")
	case agent.SessionDamaged:
		row(&b, "Сессия ИТМО", red("файл повреждён")+" - нажмите [s]")
	default:
		row(&b, "Сессия ИТМО", yellow("нет")+" - нажмите [s]")
	}
	row(&b, "Последний опрос", ago(now, r.LastPoll))

	b.WriteString("\n" + bold("Последние события") + "\n")
	if len(r.Events) == 0 {
		b.WriteString(dim("  пока нет") + "\n")
	}
	for _, e := range r.Events[:min(len(r.Events), 5)] {
		fmt.Fprintf(&b, "  %s  %s\n", dim(e.At.Local().Format("02.01 15:04")), agent.EventLabel(e.Type))
	}
	b.WriteString("\n" + bold("Запись на физкультуру") + "\n")
	if len(r.Sport) == 0 {
		b.WriteString(dim("  попыток пока не было") + "\n")
	}
	for _, s := range r.Sport[:min(len(r.Sport), 5)] {
		label := agent.OutcomeLabel(s.Outcome)
		switch s.Outcome {
		case "SIGNED_IN", "ALREADY_BOOKED":
			label = green(label)
		case "REJECTED":
			label = red(label)
		default:
			label = yellow(label)
		}
		fmt.Fprintf(&b, "  %s  занятие %s, %s  %s\n", dim(s.At.Local().Format("02.01 15:04")),
			s.Start.Local().Format("02.01 15:04"), agent.QueueLabel(s.Queue), label)
	}
	if lines := a.Ring.last(4); len(lines) > 0 {
		b.WriteString("\n" + bold("Журнал") + "\n")
		for _, l := range lines {
			b.WriteString(dim("  "+cut(l, 100)) + "\n")
		}
	}
	b.WriteString("\n")
	switch {
	case errors.Is(loopErr, agent.ErrTokenRevoked):
		b.WriteString(dim("[p] подключить заново   [q] выйти"))
	case confirm:
		b.WriteString(yellow("Отключить агент? Сервер отзовёт токен, токен и сессия ИТМО будут удалены с этой машины.") + "\n" +
			dim("[y] да   любая клавиша - нет"))
	default:
		b.WriteString(dim("[s] новая сессия ИТМО   [u] отключить   [q] выйти"))
	}
	return b.String()
}

func row(b *strings.Builder, name, value string) {
	b.WriteString(name + strings.Repeat(" ", max(1, 18-utf8.RuneCountInString(name))) + value + "\n")
}

func ago(now, t time.Time) string {
	if t.IsZero() {
		return "никогда"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d с назад", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d мин назад", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d ч назад", int(d.Hours()))
	}
	return days(d) + " назад"
}

func days(d time.Duration) string {
	if d < 24*time.Hour {
		return fmt.Sprintf("%d ч", max(0, int(d.Hours())))
	}
	return fmt.Sprintf("%d дн.", int(d.Hours()/24))
}

func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
