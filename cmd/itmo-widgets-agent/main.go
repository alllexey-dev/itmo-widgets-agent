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
	"syscall"
	"time"

	"github.com/alllexey-dev/itmo-widgets-agent/internal/agent"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/config"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/pair"
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
  itmo-widgets-agent health         код выхода 0, если агент работает
  itmo-widgets-agent version        показать версию
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cmd := "run"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	sessionOnly := fs.Bool("session", false, "только новая сессия ИТМО для уже подключённого агента")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 || (*sessionOnly && cmd != "pair") {
		fmt.Fprintf(stderr, usage, version)
		return 2
	}
	switch cmd {
	case "run", "pair", "login", "status", "health":
	case "version":
		fmt.Fprintln(stdout, version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprintf(stdout, usage, version)
		return 0
	default:
		fmt.Fprintf(stderr, usage, version)
		return 2
	}

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fmt.Fprintln(stderr, "настройки:", err)
		return 2
	}
	log := slog.New(slog.NewJSONHandler(stderr, nil))
	a := agent.Build(cfg, version, log, time.Now)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch cmd {
	case "run":
		err = a.Run(ctx)
	case "pair":
		err = a.PairFlow(cfg.Name, version, stdout).Pair(ctx, *sessionOnly)
	case "login":
		err = a.PairFlow(cfg.Name, version, stdout).Login(ctx, stdin)
	case "status":
		err = agent.Status(a.Files, time.Now(), stdout)
	case "health":
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
