// Package control is the running agent's local endpoint for attach and status: a Unix socket in the state directory,
// one JSON object per line. Access control is the file permissions (directory 0700, socket 0600): the same user only.
//
// The agent sends {"status":{...}} with the whole agent.Live on connect and again whenever it changes (checked once a
// second), and {"reply":{...}} after a command. The client sends {"cmd":"unpair"}. A new ITMO session needs no
// command: attach runs the pairing on its own terminal and the agent picks up the new session.json, as with
// `pair --session`. Nothing on the socket carries the agent token or ITMO tokens; every line also passes redact.
package control

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/alllexey-dev/itmo-widgets-agent/internal/agent"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/redact"
)

const (
	SocketName   = "agent.sock"
	CmdUnpair    = "unpair"
	writeTimeout = 5 * time.Second
	readTimeout  = 3 * time.Second
)

var (
	// ErrRunning: another agent answers on the socket of this state directory.
	ErrRunning = errors.New("агент с этой папкой уже запущен; посмотреть: itmo-widgets-agent attach")
	// ErrNotRunning: nothing answers on the socket.
	ErrNotRunning = errors.New("агент не запущен")
)

type Message struct {
	Status *agent.Live `json:"status,omitempty"`
	Reply  *Reply      `json:"reply,omitempty"`
}

type Reply struct {
	Cmd     string `json:"cmd"`
	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`
}

type Command struct {
	Cmd string `json:"cmd"`
}

func Path(dir string) string { return filepath.Join(dir, SocketName) }

// Listen opens the socket. A socket file nobody answers on is left from a crash and is replaced.
func Listen(dir string) (net.Listener, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// Windows has no modes: the socket inherits the ACL of the user's profile folder.
	unix := runtime.GOOS != "windows"
	// 0700 first, so nobody else can reach the socket before its own chmod.
	if unix {
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, err
		}
	}
	p := Path(dir)
	if c, err := net.DialTimeout("unix", p, time.Second); err == nil {
		c.Close()
		return nil, ErrRunning
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", p)
	if err != nil {
		return nil, err
	}
	if unix {
		// Best effort: the 0700 directory already keeps others out, and some file systems (Docker Desktop bind
		// mounts) refuse a chmod on a socket.
		os.Chmod(p, 0o600)
	}
	return ln, nil
}

// Serve answers clients until ctx ends, then closes ln, which removes the socket file.
func Serve(ctx context.Context, ln net.Listener, loop *agent.Loop, log *slog.Logger) {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() == nil {
				log.Warn("control socket stopped", "err", err)
			}
			return
		}
		go serve(ctx, c, loop, log)
	}
}

func serve(ctx context.Context, c net.Conn, loop *agent.Loop, log *slog.Logger) {
	defer c.Close()
	agentCtx := ctx // unpair outlives a client that leaves halfway
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	replies := make(chan Reply)
	go func() {
		defer cancel()
		sc := bufio.NewScanner(c)
		for sc.Scan() {
			var cmd Command
			r := Reply{Error: "неизвестная команда"}
			if json.Unmarshal(sc.Bytes(), &cmd) == nil && cmd.Cmd == CmdUnpair {
				r = unpair(agentCtx, loop, log)
			}
			r.Cmd = cmd.Cmd
			select {
			case replies <- r:
			case <-ctx.Done():
				return
			}
		}
	}()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var last []byte
	for {
		live := loop.Live()
		if b, _ := json.Marshal(Message{Status: &live}); !bytes.Equal(b, last) {
			if send(c, b) != nil {
				return
			}
			last = b
		}
		select {
		case <-ctx.Done():
			return
		case r := <-replies:
			if b, _ := json.Marshal(Message{Reply: &r}); send(c, b) != nil {
				return
			}
		case <-tick.C:
		}
	}
}

func unpair(ctx context.Context, loop *agent.Loop, log *slog.Logger) Reply {
	log.Info("unpair requested over the control socket")
	u, err := loop.Unpair(ctx)
	if err != nil {
		return Reply{Error: "не удалось удалить файлы: " + err.Error()}
	}
	return Reply{Message: u.Message()}
}

func send(c net.Conn, b []byte) error {
	c.SetWriteDeadline(time.Now().Add(writeTimeout))
	_, err := c.Write(append([]byte(redact.String(string(b))), '\n'))
	return err
}

// Client is attach's and status's end of the socket.
type Client struct {
	conn net.Conn
	sc   *bufio.Scanner
}

// Dial connects to the agent of dir; ErrNotRunning when nothing answers.
func Dial(dir string) (*Client, error) {
	c, err := net.DialTimeout("unix", Path(dir), time.Second)
	if err != nil {
		return nil, ErrNotRunning
	}
	sc := bufio.NewScanner(c)
	sc.Buffer(nil, 1<<20)
	return &Client{conn: c, sc: sc}, nil
}

// Next waits for the next message.
func (c *Client) Next() (Message, error) {
	var m Message
	if !c.sc.Scan() {
		if err := c.sc.Err(); err != nil {
			return m, err
		}
		return m, errors.New("агент закрыл соединение")
	}
	return m, json.Unmarshal(c.sc.Bytes(), &m)
}

// Snapshot is the first status, which the agent sends on connect.
func (c *Client) Snapshot() (agent.Live, error) {
	c.conn.SetReadDeadline(time.Now().Add(readTimeout))
	defer c.conn.SetReadDeadline(time.Time{})
	m, err := c.Next()
	if err == nil && m.Status == nil {
		err = errors.New("агент не прислал состояние")
	}
	if err != nil {
		return agent.Live{}, err
	}
	return *m.Status, nil
}

func (c *Client) Send(cmd string) error {
	b, _ := json.Marshal(Command{Cmd: cmd})
	c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	_, err := c.conn.Write(append(b, '\n'))
	return err
}

func (c *Client) Close() error { return c.conn.Close() }
