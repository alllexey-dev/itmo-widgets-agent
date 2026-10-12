package sport_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alllexey-dev/itmo-widgets-agent/internal/backend"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/fake"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/itmo"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/notify"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/sport"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/state"
)

const (
	start = "2026-10-07T10:00+03:00"
	end   = "2026-10-07T11:30+03:00"
)

var noCapacity = `{"error_code":137,"error_message":"[` + itmo.NoCapacityMessage + ` (student 123456, lesson 9001)]","result":null}`

func TestHandle(t *testing.T) {
	tests := []struct {
		name        string
		queue       string
		entries     string
		chosen      []int64
		signStatus  int
		signBody    string
		myStatus    int
		lessons     []string
		wantSignIns []int64
		wantReports []string
		wantNotes   int
		wantSync    bool
		wantErr     error
	}{
		{
			name: "booked: satisfied, notified, synced", queue: "free",
			entries: "[" + fake.FreeEntry(9001, "NOTIFIED", false) + "]", lessons: []string{fake.Lesson(9001, start, end)},
			wantSignIns: []int64{9001}, wantReports: []string{"free/9001/mark-satisfied"}, wantNotes: 1, wantSync: true,
		},
		{
			name: "auto queue matches realLessonId", queue: "auto",
			entries: "[" + fake.AutoEntry(9001, "WAITING") + "]", lessons: []string{fake.Lesson(9001, start, end)},
			wantSignIns: []int64{9001}, wantReports: []string{"auto/9001/mark-satisfied"}, wantNotes: 1, wantSync: true,
		},
		{
			name: "REJECTED cancels the entry and notifies", queue: "free",
			entries: "[" + fake.FreeEntry(9001, "WAITING", false) + "]", lessons: []string{fake.Lesson(9001, start, end)},
			signBody:    `{"error_code":137,"error_message":"[cannot enroll student: [daily limit]]","result":null}`,
			wantSignIns: []int64{9001}, wantReports: []string{"free/9001/cancel"}, wantNotes: 1,
		},
		{
			name: "NO_CAPACITY keeps the entry", queue: "free",
			entries: "[" + fake.FreeEntry(9001, "WAITING", false) + "]", lessons: []string{fake.Lesson(9001, start, end)},
			signBody: noCapacity, wantSignIns: []int64{9001},
		},
		{
			name: "server error is retried later", queue: "free",
			entries: "[" + fake.FreeEntry(9001, "WAITING", false) + "]", lessons: []string{fake.Lesson(9001, start, end)},
			signStatus: 502, signBody: "bad gateway", wantSignIns: []int64{9001},
		},
		{
			name: "refusal envelope on a non-2xx status is retried later", queue: "free",
			entries: "[" + fake.FreeEntry(9001, "WAITING", false) + "]", lessons: []string{fake.Lesson(9001, start, end)},
			signStatus: 400, signBody: `{"error_code":137,"error_message":"[cannot enroll]"}`, wantSignIns: []int64{9001},
		},
		{
			name: "already booked only reports satisfied", queue: "free",
			entries: "[" + fake.FreeEntry(9001, "NOTIFIED", false) + "]", chosen: []int64{9001},
			lessons: []string{fake.Lesson(9001, start, end)}, wantReports: []string{"free/9001/mark-satisfied"}, wantSync: true,
		},
		{
			name: "ended lesson skipped", queue: "free",
			entries: "[" + fake.FreeEntry(9001, "WAITING", false) + "]",
			lessons: []string{fake.Lesson(9001, "2026-10-07T07:00:00+03:00", "2026-10-07T08:30:00+03:00")},
		},
		{
			name: "cancelled entry is not booked", queue: "free",
			entries: "[" + fake.FreeEntry(9001, "WAITING", true) + "]", lessons: []string{fake.Lesson(9001, start, end)},
		},
		{
			name: "satisfied entry is not booked", queue: "free",
			entries: "[" + fake.FreeEntry(9001, "SATISFIED", false) + "]", lessons: []string{fake.Lesson(9001, start, end)},
		},
		{
			name: "free event does not book an auto entry", queue: "free",
			entries: "[" + fake.AutoEntry(9001, "WAITING") + "]", lessons: []string{fake.Lesson(9001, start, end)},
		},
		{
			name: "malformed and repeated lessons are skipped", queue: "free",
			entries: "[" + fake.FreeEntry(9001, "WAITING", false) + "]",
			lessons: []string{`{"id":"x"}`, fake.Lesson(0, start, end), `{"id":9002,"sectionName":"","start":"` + start + `","end":"` + end + `"}`,
				fake.Lesson(9001, start, end), fake.Lesson(9001, start, end)},
			wantSignIns: []int64{9001}, wantReports: []string{"free/9001/mark-satisfied"}, wantNotes: 1, wantSync: true,
		},
		{
			name: "closed session stops the event", queue: "free",
			entries: "[" + fake.FreeEntry(9001, "WAITING", false) + "]", lessons: []string{fake.Lesson(9001, start, end)},
			myStatus: 401, wantErr: itmo.ErrSessionClosed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := fake.NewBackend(t)
			b.Entries[tt.queue] = tt.entries
			f := fake.NewITMO(t)
			f.Chosen, f.SignStatus, f.SignBody, f.MyStatus = tt.chosen, tt.signStatus, tt.signBody, tt.myStatus
			n := fake.NewNtfy(t)
			now := func() time.Time { return fake.Now }
			client := &itmo.Client{HTTP: &http.Client{}, TokenURL: f.TokenURL(), BaseURL: f.Srv.URL, UserAgent: "t", Now: now}
			client.SetSession(&state.Session{RefreshToken: "R0"})
			e := &sport.Executor{
				Backend: &backend.Client{BaseURL: b.Srv.URL, Token: fake.AgentToken, UserAgent: "t", HTTP: &http.Client{}},
				ITMO:    client,
				Notify:  &notify.Ntfy{URL: n.Srv.URL, HTTP: &http.Client{}},
				Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
				Now:     now,
			}
			err := e.Handle(context.Background(), tt.queue, fake.Payload(tt.lessons...))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			signIns, _ := f.Snapshot()
			reports, syncs, _ := b.Calls()
			if !slices.Equal(signIns, tt.wantSignIns) {
				t.Errorf("sign-ins %v, want %v", signIns, tt.wantSignIns)
			}
			if !slices.Equal(reports, tt.wantReports) {
				t.Errorf("reports %v, want %v", reports, tt.wantReports)
			}
			if notes := n.All(); len(notes) != tt.wantNotes {
				t.Errorf("notifications %q, want %d", notes, tt.wantNotes)
			} else if len(notes) == 1 && !strings.Contains(notes[0], "Плавание, 07.10 10:00") {
				t.Errorf("notification %q lacks the lesson", notes[0])
			}
			if (len(syncs) == 1 && slices.Contains(syncs[0], 9001)) != tt.wantSync {
				t.Errorf("syncs %v, want sync %v", syncs, tt.wantSync)
			}
		})
	}
}
