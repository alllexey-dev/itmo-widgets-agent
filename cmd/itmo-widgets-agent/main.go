// Command itmo-widgets-agent is the self-hosted ITMO.Widgets agent.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/alllexey-dev/itmo-widgets-agent/internal/agent"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/config"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/control"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/pair"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/redact"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/tui"
)

// version is set by the release build: -ldflags "-X main.version=1.0.0".
var version = "dev"

const usage = `itmo-widgets-agent %s

Использование:
  itmo-widgets-agent [run]          работать: получать события и записывать на физкультуру
  itmo-widgets-agent pair           подключить агент с телефона (QR или ссылка)
  itmo-widgets-agent pair --session выдать агенту новую сессию ИТМО
  itmo-widgets-agent login          запасной вход: refresh-токен ITMO.ID из stdin
  itmo-widgets-agent status         показать состояние
  itmo-widgets-agent attach         экран запущенного агента (службы); q - закрыть, агент продолжит работу.
                                    То же: watch
  itmo-widgets-agent unpair         отключить агент: отозвать токен на сервере, удалить токен и сессию ИТМО
  itmo-widgets-agent health         код выхода 0, если агент работает
  itmo-widgets-agent version        показать версию (или --version)

Флаг run и pair:
  --ui=auto|tui|plain  экран в терминале или обычные логи; auto (по умолчанию) рисует экран,
                       только если запущен в терминале. То же задаёт IW_UI.

Настройки - переменные IW_* или файл agent.env, см. README.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cmd := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	has := func(flags ...string) bool {
		return slices.ContainsFunc(args, func(a string) bool { return slices.Contains(flags, a) })
	}
	switch {
	case cmd == "help" || has("-h", "-help", "--help"):
		fmt.Fprintf(stdout, usage, version)
		return 0
	case cmd == "version" || has("-v", "-version", "--version"):
		fmt.Fprintln(stdout, version)
		return 0
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	sessionOnly := fs.Bool("session", false, "")
	uiFlag := fs.String("ui", "", "")
	err := fs.Parse(args)
	switch {
	case err != nil:
	case !slices.Contains([]string{"run", "pair", "login", "status", "attach", "watch", "unpair", "health"}, cmd):
		err = fmt.Errorf("неизвестная команда %q", cmd)
	case fs.NArg() > 0 || (*sessionOnly && cmd != "pair") || (*uiFlag != "" && cmd != "run" && cmd != "pair"):
		err = errors.New("лишние аргументы")
	}
	if err != nil {
		fmt.Fprintf(stderr, "%v\n\n", err)
		fmt.Fprintf(stderr, usage, version)
		return 2
	}

	cfg, err := config.Load(os.Getenv)
	if err == nil && *uiFlag != "" {
		cfg.UI, err = config.ParseUI(*uiFlag)
	}
	if err != nil {
		fmt.Fprintln(stderr, "настройки:", err)
		return 2
	}
	attach := cmd == "attach" || cmd == "watch"
	screen := attach || (cmd == "run" || cmd == "pair") && (cfg.UI == "tui" || cfg.UI == "auto" && tui.IsTerminal())
	ring := &tui.Ring{}
	jsonOut := stderr
	if screen {
		jsonOut = nil
	}
	log := newLogger(jsonOut, ring, cfg.LogLevel)
	a := agent.Build(cfg, version, log, time.Now)
	loop := &agent.Loop{Agent: a, Version: version, Name: cfg.Name, Lines: func() []string { return ring.Last(10) }}
	app := &tui.App{Cfg: cfg, Version: version, Log: log, Now: time.Now}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch {
	case cmd == "run" && screen:
		err = serveAndRun(ctx, cfg.StateDir, loop, log, func() error { return app.Run(ctx, loop) })
	case cmd == "run":
		err = serveAndRun(ctx, cfg.StateDir, loop, log, func() error { return loop.Run(ctx) })
	case attach:
		err = attachTo(ctx, cfg.StateDir, app)
	case cmd == "pair" && screen:
		var isu int64
		if isu, err = app.Pair(ctx, *sessionOnly); err == nil {
			pair.Text{W: stdout}.Paired(isu)
		}
	case cmd == "pair":
		err = a.PairFlow(cfg.Name, version, pair.Text{W: stdout}).Pair(ctx, *sessionOnly)
	case cmd == "login":
		err = a.PairFlow(cfg.Name, version, pair.Text{W: stdout}).Login(ctx, stdin)
	case cmd == "status":
		err = agent.Status(a.Files, snapshot(cfg.StateDir), time.Now(), stdout)
	case cmd == "unpair":
		var u agent.Unpaired
		if u, err = a.Unpair(ctx); err == nil {
			out := stdout
			if u.Err != nil {
				out = stderr
			}
			fmt.Fprintln(out, u.Message())
		}
	case cmd == "health":
		err = agent.Health(a.Files, time.Now())
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, agent.ErrTokenRevoked):
		fmt.Fprintln(stderr, err)
		return 3
	case errors.Is(err, pair.ErrSessionRejected):
		fmt.Fprintln(stderr, err)
		return 4
	default:
		fmt.Fprintln(stderr, err)
		return 1
	}
}

// serveAndRun runs the agent with the control socket for attach and status; the socket goes away when it returns.
func serveAndRun(ctx context.Context, dir string, loop *agent.Loop, log *slog.Logger, run func() error) error {
	ln, err := control.Listen(dir)
	switch {
	case errors.Is(err, control.ErrRunning):
		return err
	case err != nil:
		log.Warn("control socket unavailable: attach and live status are off", "err", err)
	default:
		defer ln.Close()
		go control.Serve(ctx, ln, loop, log)
	}
	return run()
}

func attachTo(ctx context.Context, dir string, app *tui.App) error {
	c, err := control.Dial(dir)
	if err != nil {
		return fmt.Errorf("агент не запущен: никто не отвечает на %s.\n"+
			"Последнее сохранённое состояние: itmo-widgets-agent status\n"+
			"Запустить: включите службу в установщике или выполните itmo-widgets-agent run", control.Path(dir))
	}
	defer c.Close()
	live, err := c.Snapshot()
	if err != nil {
		return err
	}
	if !tui.IsTerminal() {
		return errors.New("для attach нужен терминал (в Docker: docker compose exec, без -T); без него: itmo-widgets-agent status")
	}
	return app.Attach(ctx, c, live)
}

// snapshot is the running agent's state, nil when none answers.
func snapshot(dir string) *agent.Live {
	c, err := control.Dial(dir)
	if err != nil {
		return nil
	}
	defer c.Close()
	if live, err := c.Snapshot(); err == nil {
		return &live
	}
	return nil
}

// newLogger writes JSON lines to jsonOut (none when nil) and short text lines into ring, which the screen, attach and
// status show; both mask tokens.
func newLogger(jsonOut io.Writer, ring io.Writer, level slog.Level) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level, ReplaceAttr: redact.Attr}
	short := slog.NewTextHandler(ring, &slog.HandlerOptions{Level: level, ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey && len(groups) == 0 {
			return slog.String(a.Key, a.Value.Time().Format("15:04:05"))
		}
		return redact.Attr(groups, a)
	}})
	if jsonOut == nil {
		return slog.New(short)
	}
	return slog.New(slog.NewMultiHandler(slog.NewJSONHandler(jsonOut, opts), short))
}
