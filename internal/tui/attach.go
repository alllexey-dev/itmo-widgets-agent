package tui

import (
	"context"
	"fmt"
	"time"

	"github.com/alllexey-dev/itmo-widgets-agent/internal/agent"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/control"
)

// Attach shows the status screen of the agent behind c, starting from its snapshot live, until q; the agent keeps
// running. [s] pairs a new ITMO session right here, like `pair --session`, and [p] the agent itself when it has no
// token; the agent picks up the new files. [u] asks the agent to unpair.
func (a *App) Attach(ctx context.Context, c *control.Client, live agent.Live) (err error) {
	if a.scr, err = openScreen(); err != nil {
		return err
	}
	defer a.scr.close()
	msgs, gone, done := make(chan control.Message, 16), make(chan error, 1), make(chan struct{})
	defer close(done)
	go func() {
		for {
			m, err := c.Next()
			if err != nil {
				gone <- err
				return
			}
			select {
			case msgs <- m:
			case <-done:
				return
			}
		}
	}()
	confirm := false
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		a.scr.draw(a.statusFrame(live, confirm, true))
		select {
		case <-ctx.Done():
			return nil
		case err := <-gone:
			return fmt.Errorf("связь с агентом потеряна: %w", err)
		case m := <-msgs:
			if m.Status != nil {
				live = *m.Status
			}
			if r := m.Reply; r != nil {
				a.notice = r.Message + r.Error
			}
		case <-tick.C:
		case k := <-a.scr.keys:
			switch {
			case k == 'q':
				return nil
			case live.Revoked:
			case !live.HasToken:
				if k == 'p' {
					a.notice = ""
					if _, err := a.pair(ctx, false); err == nil {
						a.notice = "Подключено: агент подхватит токен в течение 10 секунд."
					}
				}
			case k == 's' && !confirm:
				a.newSession(ctx)
			case k == 'u' && !confirm:
				confirm = true
			case k == 'y' && confirm:
				a.notice, confirm = "Отключаем агент на сервере…", false
				if err := c.Send(control.CmdUnpair); err != nil {
					return err
				}
			default:
				confirm = false
			}
		}
	}
}
