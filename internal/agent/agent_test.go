package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alllexey-dev/itmo-widgets-agent/internal/backend"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/config"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/fake"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/state"
)

type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type harness struct {
	a      *Agent
	b      *fake.Backend
	f      *fake.ITMO
	n      *fake.Ntfy
	clock  *fake.Clock
	logs   *logBuffer
	sleeps []time.Duration
	ctx    context.Context
}

func newHarness(t *testing.T, withSession bool) *harness {
	h := &harness{b: fake.NewBackend(t), f: fake.NewITMO(t), n: fake.NewNtfy(t), clock: fake.NewClock(), logs: &logBuffer{}}
	dir := t.TempDir()
	cfg := config.Config{
		BackendURL: h.b.Srv.URL, StateDir: dir, TokenFile: filepath.Join(dir, "agent-token"), NtfyURL: h.n.Srv.URL,
		Name: "nas", Features: map[string]bool{"sport": true, "friends": true},
	}
	h.a = Build(cfg, "test", slog.New(slog.NewJSONHandler(h.logs, nil)), h.clock.Now)
	h.a.ITMO.TokenURL, h.a.ITMO.BaseURL = h.f.TokenURL(), h.f.Srv.URL
	h.a.jitter = func(d time.Duration) time.Duration { return d }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h.ctx = ctx
	h.b.OnDrained = cancel
	h.a.Sleep = func(ctx context.Context, d time.Duration) error {
		h.sleeps = append(h.sleeps, d)
		return ctx.Err()
	}
	if err := h.a.Files.SaveToken(fake.AgentToken); err != nil {
		t.Fatal(err)
	}
	if withSession {
		h.saveSession(t, "SECRET-ACCESS-0", 0)
	}
	return h
}

func (h *harness) saveSession(t *testing.T, access string, mtimeShift time.Duration) {
	s := state.Session{Isu: 100001, RefreshToken: "SECRET-REFRESH-0", RefreshExpiresAt: fake.Now.Add(30 * 24 * time.Hour),
		AccessToken: access, AccessExpiresAt: h.clock.Now().Add(20 * time.Minute), RefreshedAt: h.clock.Now()}
	if _, err := h.a.Files.SaveSession(s); err != nil {
		t.Fatal(err)
	}
	if mtimeShift != 0 {
		m := time.Now().Add(mtimeShift)
		if err := os.Chtimes(h.a.Files.SessionPath(), m, m); err != nil {
			t.Fatal(err)
		}
	}
}

func sportEvent(seq int64, id string, lessonID int64) backend.Event {
	return backend.Event{Seq: seq, ID: id, Type: "SPORT_FREE_SIGN_LESSONS_PAYLOAD", ExpiresAt: fake.Now.Add(time.Hour),
		Payload: fake.Payload(fake.Lesson(lessonID, "2026-10-07T10:00+03:00", "2026-10-07T11:30+03:00"))}
}

func page(next int64, events ...backend.Event) fake.Response {
	return fake.Response{Page: backend.Page{Events: events, NextCursor: next}}
}

func TestNextBackoff(t *testing.T) {
	tests := []struct{ in, want time.Duration }{
		{0, time.Second}, {time.Second, 2 * time.Second}, {4 * time.Minute, 5 * time.Minute}, {5 * time.Minute, 5 * time.Minute},
	}
	for _, tt := range tests {
		if got := NextBackoff(tt.in); got != tt.want {
			t.Errorf("NextBackoff(%v) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestJitter(t *testing.T) {
	for _, d := range []time.Duration{time.Second, 5 * time.Minute} {
		for range 100 {
			if got := Jitter(d); got < d/2 || got > d {
				t.Fatalf("Jitter(%v) = %v, want within [%v, %v]", d, got, d/2, d)
			}
		}
	}
}

func TestPollPauses(t *testing.T) {
	h := newHarness(t, false)
	h.b.Responses = []fake.Response{
		{Status: 500}, {Status: 502}, {Status: 429, Code: "rate_limited"}, {Status: 503},
		{Status: 403, Code: "permission_denied"}, page(0),
	}
	if err := h.a.Run(h.ctx); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{time.Second, 2 * time.Second, time.Minute, 4 * time.Second, time.Hour, time.Second}
	if !slices.Equal(h.sleeps[:len(want)], want) {
		t.Fatalf("pauses %v, want prefix %v", h.sleeps, want)
	}
	if st, _ := h.a.Files.State(); st.LastError != "" || !st.LastPollAt.Equal(fake.Now) {
		t.Fatalf("state after recovery: %+v", st)
	}
}

func TestCursorSavedOnlyAfterHandling(t *testing.T) {
	tests := []struct {
		name       string
		crashOn    string
		wantCursor int64
		wantSeen   []string
	}{
		{name: "all handled", wantCursor: 7, wantSeen: []string{"a", "b"}},
		{name: "crash while handling the second event", crashOn: "b", wantCursor: 3, wantSeen: []string{"a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, false)
			h.b.Responses = []fake.Response{page(7, backend.Event{Seq: 3, ID: "a"}, backend.Event{Seq: 5, ID: "b"})}
			var cursors []int64
			h.a.handle = func(_ context.Context, ev backend.Event) error {
				st, _ := h.a.Files.State()
				cursors = append(cursors, st.Cursor)
				if ev.ID == tt.crashOn {
					panic("crash")
				}
				return nil
			}
			func() {
				defer func() { _ = recover() }()
				_ = h.a.Run(h.ctx)
			}()
			st, _ := h.a.Files.State()
			if st.Cursor != tt.wantCursor || !slices.Equal(st.Seen, tt.wantSeen) {
				t.Fatalf("saved cursor %d seen %v, want %d %v", st.Cursor, st.Seen, tt.wantCursor, tt.wantSeen)
			}
			if !slices.Equal(cursors, []int64{0, 3}) {
				t.Fatalf("cursor on disk while handling: %v", cursors)
			}
		})
	}
}

func TestTokenRevokedWipesFiles(t *testing.T) {
	h := newHarness(t, true)
	h.b.Responses = []fake.Response{{Status: 401, Code: "unauthorized"}}
	if err := h.a.Run(h.ctx); !errors.Is(err, ErrTokenRevoked) {
		t.Fatalf("want ErrTokenRevoked, got %v", err)
	}
	for _, p := range []string{h.a.Files.TokenPath, h.a.Files.SessionPath()} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s not wiped", p)
		}
	}
	if _, refreshes := h.f.Snapshot(); refreshes != 0 || h.f.MyCalls != 0 {
		t.Fatal("ITMO was called after a 401")
	}
}

func TestSameEventNeverBooksTwice(t *testing.T) {
	h := newHarness(t, true)
	h.b.Entries["free"] = "[" + fake.FreeEntry(9001, "WAITING", false) + "]"
	ev := sportEvent(1, "sport-free:11:1", 9001)
	again := ev
	again.Seq = 2
	h.b.Responses = []fake.Response{page(1, ev), page(2, again)}
	if err := h.a.Run(h.ctx); err != nil {
		t.Fatal(err)
	}
	// A restarted agent gets the same event once more.
	h2 := newHarnessFrom(t, h)
	h2.b.Responses = []fake.Response{page(2, ev)}
	if err := h2.a.Run(h2.ctx); err != nil {
		t.Fatal(err)
	}
	if signIns, _ := h.f.Snapshot(); !slices.Equal(signIns, []int64{9001}) {
		t.Fatalf("sign-ins %v, want one", signIns)
	}
	st, _ := h.a.Files.State()
	if len(st.Sport) != 1 || st.Sport[0].LessonID != 9001 || st.Sport[0].Outcome != "SIGNED_IN" || len(st.Events) != 1 {
		t.Fatalf("history: sport %+v events %+v", st.Sport, st.Events)
	}
}

// newHarnessFrom restarts the agent on the same files and fakes.
func newHarnessFrom(t *testing.T, old *harness) *harness {
	h := *old
	cfg := config.Config{BackendURL: h.b.Srv.URL, StateDir: old.a.Files.Dir, TokenFile: old.a.Files.TokenPath,
		NtfyURL: h.n.Srv.URL, Name: "nas", Features: old.a.Features}
	h.a = Build(cfg, "test", slog.New(slog.NewJSONHandler(h.logs, nil)), h.clock.Now)
	h.a.ITMO.TokenURL, h.a.ITMO.BaseURL = h.f.TokenURL(), h.f.Srv.URL
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h.ctx, h.b.OnDrained = ctx, cancel
	h.a.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	return &h
}

func TestRedeliveredEventAfterCrashOnlyReportsSatisfied(t *testing.T) {
	h := newHarness(t, true)
	h.b.Entries["free"] = "[" + fake.FreeEntry(9001, "NOTIFIED", false) + "]"
	h.f.Chosen = []int64{9001} // booked before the crash, not reported
	h.b.Responses = []fake.Response{page(9, sportEvent(9, "sport-free:11:2", 9001))}
	if err := h.a.Run(h.ctx); err != nil {
		t.Fatal(err)
	}
	reports, _, _ := h.b.Calls()
	if signIns, _ := h.f.Snapshot(); len(signIns) != 0 || !slices.Equal(reports, []string{"free/9001/mark-satisfied"}) {
		t.Fatalf("sign-ins %v reports %v", signIns, reports)
	}
	if len(h.n.All()) != 0 {
		t.Fatal("a repeated booking was notified")
	}
}

func TestSessionClosed(t *testing.T) {
	h := newHarness(t, true)
	h.b.Entries["free"] = "[" + fake.FreeEntry(9001, "WAITING", false) + "]"
	h.f.Set(func(f *fake.ITMO) { f.MyStatus, f.TokenStatus, f.TokenBody = 401, 400, `{"error":"invalid_grant"}` })
	polls := 0
	h.b.BeforeEvents = func(string) {
		if polls++; polls == 3 {
			h.saveSession(t, "SECRET-ACCESS-NEW", time.Hour) // pair --session wrote a new file
		}
	}
	h.b.Responses = []fake.Response{page(1, sportEvent(1, "e1", 9001)), page(2, sportEvent(2, "e2", 9001)), page(2)}
	if err := h.a.Run(h.ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, itmo := h.b.Calls(); !slices.Equal(itmo, []string{"ok", "none", "none", "ok"}) {
		t.Fatalf("itmo params %v", itmo)
	}
	notes := h.n.All()
	if len(notes) != 1 || !strings.Contains(notes[0], "Сессия ИТМО закрыта") {
		t.Fatalf("notifications %q", notes)
	}
	signIns, refreshes := h.f.Snapshot()
	if len(signIns) != 0 || refreshes != 1 {
		t.Fatalf("after closing: sign-ins %v, refreshes %d (want no retry)", signIns, refreshes)
	}
	if st, _ := h.a.Files.State(); st.SessionClosedMtime != 0 {
		t.Fatal("closed mark not cleared by the new session file")
	}
}

func TestSessionStaysClosedAcrossRestart(t *testing.T) {
	h := newHarness(t, true)
	m, _ := h.a.Files.SessionMtime()
	if err := h.a.Files.SaveState(state.State{SessionClosedMtime: m, LastPollAt: fake.Now}); err != nil {
		t.Fatal(err)
	}
	if err := Health(h.a.Files, fake.Now); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("health with a closed session: %v", err)
	}
	h.b.Responses = []fake.Response{page(0)}
	if err := h.a.Run(h.ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, itmo := h.b.Calls(); itmo[0] != "none" {
		t.Fatalf("itmo params %v", itmo)
	}
}

func TestHealth(t *testing.T) {
	tests := []struct {
		name    string
		poll    time.Duration
		wantErr bool
	}{
		{"polled a minute ago", -time.Minute, false},
		{"no poll for 3 minutes", -3 * time.Minute, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := state.Files{Dir: t.TempDir()}
			files.SaveState(state.State{LastPollAt: fake.Now.Add(tt.poll)})
			if err := Health(files, fake.Now); (err != nil) != tt.wantErr {
				t.Fatalf("health: %v", err)
			}
		})
	}
}

func TestLogsNeverContainTokens(t *testing.T) {
	h := newHarness(t, true)
	h.saveSession(t, "", 0) // forces a refresh, so new tokens pass through
	h.b.Entries["free"] = "[" + fake.FreeEntry(9001, "WAITING", false) + "]"
	friend, _ := json.Marshal(map[string]any{"event": "REQUEST_RECEIVED", "user": map[string]any{"isu": 2, "name": "Друг Первый"}})
	h.b.Responses = []fake.Response{
		{Status: 500},
		page(2, sportEvent(1, "e1", 9001), backend.Event{Seq: 2, ID: "f1", Type: "FRIENDSHIP_EVENT_PAYLOAD", Payload: friend}),
		page(3, backend.Event{Seq: 3, ID: "x", Type: "SOMETHING_NEW"}),
		{Status: 401, Code: "unauthorized"},
	}
	if err := h.a.Run(h.ctx); !errors.Is(err, ErrTokenRevoked) {
		t.Fatal(err)
	}
	var status strings.Builder
	Status(h.a.Files, nil, fake.Now, &status)
	logs := h.logs.String() + status.String()
	if signIns, refreshes := h.f.Snapshot(); len(signIns) != 1 || refreshes != 1 {
		t.Fatalf("scenario did not book with a refresh: %v %d", signIns, refreshes)
	}
	if n := h.n.All(); len(n) != 2 || !strings.Contains(n[1], "Друг Первый") {
		t.Fatalf("notifications %q", n)
	}
	for _, secret := range []string{"SECRET", fake.AgentToken, "iwa_", "Друг", "Плавание"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("logs contain %q:\n%s", secret, logs)
		}
	}
}

func TestStatusShowsHistory(t *testing.T) {
	files := state.Files{Dir: t.TempDir()}
	files.TokenPath = filepath.Join(files.Dir, "agent-token")
	files.SaveToken(fake.AgentToken)
	files.SaveSession(state.Session{Isu: 100001, RefreshToken: "SECRET-R", RefreshedAt: fake.Now,
		RefreshExpiresAt: fake.Now.Add(30 * 24 * time.Hour)})
	st := state.State{LastPollAt: fake.Now.Add(-time.Minute)}
	st.AddEvent(state.EventRecord{At: fake.Now, Seq: 4, Type: "SPORT_AUTO_SIGN_LESSONS_PAYLOAD"})
	st.AddSport(state.SportAttempt{At: fake.Now, Queue: "auto", LessonID: 9001, Start: fake.Now, Outcome: "NO_CAPACITY"})
	files.SaveState(st)
	var out strings.Builder
	if err := Status(files, nil, fake.Now, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"на связи", "ISU 100001", "действует до", "автозапись", "занятие 9001", "мест нет"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "SECRET") || strings.Contains(out.String(), fake.AgentToken) {
		t.Fatalf("status prints a secret:\n%s", out.String())
	}
}

func TestStatusPrefersTheRunningAgent(t *testing.T) {
	files := state.Files{Dir: t.TempDir()}
	live := &Live{Report: Report{HasToken: true, Cursor: 42}, Running: true, Started: fake.Now, Log: []string{"msg=\"agent started\""}}
	var out strings.Builder
	if err := Status(files, live, fake.Now, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Запущен: да", "attach", "Курсор событий: 42", "Журнал:", "agent started"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status lacks %q:\n%s", want, out.String())
		}
	}
}
