package control

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/alllexey-dev/itmo-widgets-agent/internal/agent"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/backend"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/config"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/fake"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/state"
)

// shortDir: t.TempDir on macOS can pass the 104-byte limit of a socket path.
func shortDir(t *testing.T) string {
	dir, err := os.MkdirTemp("", "iwc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

type rig struct {
	loop *agent.Loop
	b    *fake.Backend
	dir  string
	ctx  context.Context
}

// newRig is a paired agent with an ITMO session, serving its socket; the poll loop is not started.
func newRig(t *testing.T) *rig {
	r := &rig{b: fake.NewBackend(t), dir: shortDir(t)}
	f := fake.NewITMO(t)
	cfg := config.Config{BackendURL: r.b.Srv.URL, StateDir: r.dir, TokenFile: filepath.Join(r.dir, "agent-token"),
		Name: "nas", Features: map[string]bool{"sport": true}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := agent.Build(cfg, backend.BuildInfo{Version: "test"}, log, time.Now)
	a.ITMO.TokenURL, a.ITMO.BaseURL = f.TokenURL(), f.Srv.URL
	if err := a.Files.SaveToken(fake.AgentToken); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Files.SaveSession(state.Session{Isu: 100001, RefreshToken: "SECRET-REFRESH-0", AccessToken: "SECRET-ACCESS-0",
		RefreshedAt: time.Now(), RefreshExpiresAt: time.Now().Add(time.Hour), AccessExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// Unredacted on purpose: the socket must mask what slipped past the logger.
	r.loop = &agent.Loop{Agent: a, Version: "test", Name: "nas", Lines: func() []string { return []string{"leak " + fake.AgentToken} }}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r.ctx = ctx
	ln, err := Listen(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	go Serve(ctx, ln, r.loop, log)
	return r
}

func (r *rig) dial(t *testing.T) (*Client, net.Conn) {
	c, err := Dial(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c, c.conn
}

// next waits for a message that satisfies ok.
func next(t *testing.T, c *Client, ok func(Message) bool) Message {
	c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		m, err := c.Next()
		if err != nil {
			t.Fatal(err)
		}
		if ok(m) {
			return m
		}
	}
}

func TestSocketIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no file modes; the profile folder's ACL applies")
	}
	dir := shortDir(t)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{dir: 0o700, Path(dir): 0o600} {
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != want {
			t.Fatalf("%s: %v %v, want %v", path, fi.Mode().Perm(), err, want)
		}
	}
	ln.Close()
	if _, err := os.Stat(Path(dir)); !os.IsNotExist(err) {
		t.Fatalf("socket left after close: %v", err)
	}
}

func TestStaleSocketIsReplacedAndALiveOneIsNot(t *testing.T) {
	dir := shortDir(t)
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: Path(dir), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false) // as after a crash
	stale.Close()
	ln, err := Listen(dir)
	if err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	if _, err := Listen(dir); err != ErrRunning {
		t.Fatalf("second Listen = %v, want ErrRunning", err)
	}
}

func TestSnapshotThenUpdatesWithoutSecrets(t *testing.T) {
	r := newRig(t)
	c, _ := r.dial(t)
	var stream strings.Builder
	record := func(m Message) Message {
		stream.WriteString(m.Status.LastError + strings.Join(m.Status.Log, "") + m.Status.Dir)
		return m
	}
	snap := record(next(t, c, func(m Message) bool { return m.Status != nil }))
	if !snap.Status.HasToken || snap.Status.Session != agent.SessionOK || snap.Status.Isu != 100001 || snap.Status.Name != "nas" {
		t.Fatalf("snapshot %+v", snap.Status)
	}
	st := state.State{LastPollAt: time.Now(), LastError: "backend said Bearer " + fake.AgentToken + " eyJSECRETSECRETSECRET"}
	st.AddEvent(state.EventRecord{At: time.Now(), Seq: 7, Type: "SPORT_FREE_SIGN_LESSONS_PAYLOAD"})
	if err := r.loop.Agent.Files.SaveState(st); err != nil {
		t.Fatal(err)
	}
	up := record(next(t, c, func(m Message) bool { return m.Status != nil && len(m.Status.Events) > 0 }))
	if up.Status.Events[0].Seq != 7 {
		t.Fatalf("update %+v", up.Status.Events)
	}
	for _, secret := range []string{"SECRET", fake.AgentToken} {
		if strings.Contains(stream.String(), secret) {
			t.Fatalf("stream contains %q: %s", secret, stream.String())
		}
	}
	if !strings.Contains(stream.String(), "iwa_[redacted]") {
		t.Fatalf("the planted token did not reach the stream masked: %s", stream.String())
	}
}

func TestRawStreamHasNoSecrets(t *testing.T) {
	r := newRig(t)
	st := state.State{LastError: "Bearer " + fake.AgentToken}
	r.loop.Agent.Files.SaveState(st)
	_, conn := r.dial(t)
	conn.SetReadDeadline(time.Now().Add(2500 * time.Millisecond))
	raw, _ := io.ReadAll(conn)
	if !strings.Contains(string(raw), `"status"`) {
		t.Fatalf("no status in %q", raw)
	}
	for _, secret := range []string{"SECRET", fake.AgentToken, "refreshToken", "accessToken"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("stream contains %q: %s", secret, raw)
		}
	}
}

func TestUnpairCommand(t *testing.T) {
	r := newRig(t)
	done := make(chan error, 1)
	go func() { done <- r.loop.Run(r.ctx) }()
	c, _ := r.dial(t)
	next(t, c, func(m Message) bool { return m.Status != nil && m.Status.Running })

	if err := c.Send("nope"); err != nil {
		t.Fatal(err)
	}
	if m := next(t, c, func(m Message) bool { return m.Reply != nil }); m.Reply.Cmd != "nope" || m.Reply.Error == "" {
		t.Fatalf("unknown command reply %+v", m.Reply)
	}
	if err := c.Send(CmdUnpair); err != nil {
		t.Fatal(err)
	}
	m := next(t, c, func(m Message) bool { return m.Reply != nil })
	if m.Reply.Cmd != CmdUnpair || m.Reply.Error != "" || !strings.Contains(m.Reply.Message, "сервер отозвал токен") || r.b.SelfRevokes != 1 {
		t.Fatalf("unpair reply %+v, revokes %d", m.Reply, r.b.SelfRevokes)
	}
	if _, err := r.loop.Agent.Files.Token(); !os.IsNotExist(err) {
		t.Fatalf("token left: %v", err)
	}
	// The loop is back, waiting for a new pairing.
	next(t, c, func(m Message) bool { return m.Status != nil && m.Status.Running && !m.Status.HasToken })
	select {
	case err := <-done:
		t.Fatalf("loop ended: %v", err)
	default:
	}
}

func TestDialWithoutAgent(t *testing.T) {
	if _, err := Dial(shortDir(t)); err != ErrNotRunning {
		t.Fatalf("Dial = %v", err)
	}
}
