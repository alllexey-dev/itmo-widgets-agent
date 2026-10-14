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
  itmo-widgets-agent unpair         удалить токен агента и сессию ИТМО с этой машины
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
	case !slices.Contains([]string{"run", "pair", "login", "status", "unpair", "health"}, cmd):
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
	screen := (cmd == "run" || cmd == "pair") && (cfg.UI == "tui" || cfg.UI == "auto" && tui.IsTerminal())
	ring := &tui.Ring{}
	logOut := stderr
	if screen {
		logOut = ring
	}
	log := newLogger(logOut, screen, cfg.LogLevel)
	a := agent.Build(cfg, version, log, time.Now)
	app := &tui.App{Cfg: cfg, Version: version, Log: log, Ring: ring, Now: time.Now}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch {
	case cmd == "run" && screen:
		err = app.Run(ctx, a)
	case cmd == "run":
		err = a.Run(ctx)
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
		err = agent.Status(a.Files, time.Now(), stdout)
	case cmd == "unpair":
		if err = a.Files.Wipe(); err == nil {
			fmt.Fprintln(stdout, "Токен агента и сессия ИТМО удалены с этой машины. Чтобы закрыть агенту доступ к серверу, "+
				"удалите его в приложении: Профиль -> «Подключённые устройства».")
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

// newLogger writes JSON lines, or short text lines for the screen's log panel; both mask tokens.
func newLogger(w io.Writer, text bool, level slog.Level) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level, ReplaceAttr: redact.Attr}
	if !text {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	opts.ReplaceAttr = func(groups []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey && len(groups) == 0 {
			return slog.String(a.Key, a.Value.Time().Format("15:04:05"))
		}
		return redact.Attr(groups, a)
	}
	return slog.New(slog.NewTextHandler(w, opts))
}
