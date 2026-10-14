package agent

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Live is a running agent's state: the status screen draws it and the control socket sends it. It holds no secret.
type Live struct {
	Report
	ReadError string    `json:"readError,omitempty"`
	Version   string    `json:"version"`
	Name      string    `json:"name"`
	Started   time.Time `json:"started"`
	Running   bool      `json:"running"`
	// Stopped is why the poll loop ended; Revoked: it ended on a revoked token.
	Stopped string   `json:"stopped,omitempty"`
	Revoked bool     `json:"revoked,omitempty"`
	Log     []string `json:"log,omitempty"`
}

// Loop runs Agent.Run and tracks it for the status screen and the control socket. Unpair may come from either while
// the loop runs: it stops the loop, unpairs, and the loop starts again and waits for a new token.
type Loop struct {
	Agent   *Agent
	Version string
	Name    string
	// Lines returns the last log lines, already redacted.
	Lines func() []string

	work    sync.Mutex // held by Unpair; Run waits for it before each start
	mu      sync.Mutex
	cancel  context.CancelFunc
	stopped chan struct{}
	started time.Time
	running bool
	err     error
}

// Run polls until ctx ends or the token is revoked.
func (l *Loop) Run(ctx context.Context) error {
	for {
		l.work.Lock()
		runCtx, cancel := context.WithCancel(ctx)
		stopped := make(chan struct{})
		l.mu.Lock()
		l.cancel, l.stopped, l.started, l.running, l.err = cancel, stopped, l.Agent.Now(), true, nil
		l.mu.Unlock()
		l.work.Unlock()

		err := l.Agent.Run(runCtx)
		halted := runCtx.Err() != nil && ctx.Err() == nil
		cancel()
		l.mu.Lock()
		l.running, l.err = false, err
		l.mu.Unlock()
		close(stopped)
		if !halted {
			return err
		}
	}
}

// Unpair stops the running loop, then does Agent.Unpair; Run starts the loop again afterwards.
func (l *Loop) Unpair(ctx context.Context) (Unpaired, error) {
	l.work.Lock()
	defer l.work.Unlock()
	l.mu.Lock()
	cancel, stopped := l.cancel, l.stopped
	l.mu.Unlock()
	if cancel != nil {
		cancel()
		<-stopped
	}
	return l.Agent.Unpair(ctx)
}

// Live reads the files and adds the loop's own state.
func (l *Loop) Live() Live {
	r, err := ReadReport(l.Agent.Files)
	l.mu.Lock()
	v := Live{Report: r, Version: l.Version, Name: l.Name, Started: l.started, Running: l.running,
		Revoked: errors.Is(l.err, ErrTokenRevoked)}
	if l.err != nil {
		v.Stopped = l.err.Error()
	}
	l.mu.Unlock()
	if err != nil {
		v.ReadError = err.Error()
	}
	if l.Lines != nil {
		v.Log = l.Lines()
	}
	return v
}
