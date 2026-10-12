package itmo_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/alllexey-dev/itmo-widgets-agent/internal/fake"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/itmo"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/state"
)

func TestClassify(t *testing.T) {
	long := "[" + itmo.NoCapacityMessage + " (student 123456, lesson 9001)]"
	tests := []struct {
		name string
		err  error
		want itmo.Outcome
	}{
		{"success", nil, itmo.SignedIn},
		{"no capacity with context", &itmo.APIError{Status: 200, Code: 137, Message: long}, itmo.NoCapacity},
		{"bare capacity message is a refusal", &itmo.APIError{Status: 200, Code: 137, Message: itmo.NoCapacityMessage}, itmo.Rejected},
		{"other refusal", &itmo.APIError{Status: 200, Code: 137, Message: "[cannot enroll student: [daily limit]]"}, itmo.Rejected},
		{"envelope on non-2xx", &itmo.APIError{Status: 400, Code: 100}, itmo.RetryLater},
		{"http error", &itmo.HTTPError{Status: 502}, itmo.RetryLater},
		{"session closed", itmo.ErrSessionClosed, itmo.RetryLater},
		{"transport", errors.New("connection reset"), itmo.RetryLater},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := itmo.Classify(tt.err); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func newClient(f *fake.ITMO) *itmo.Client {
	return &itmo.Client{
		HTTP: &http.Client{CheckRedirect: itmo.NoRedirects}, TokenURL: f.TokenURL(), BaseURL: f.Srv.URL,
		UserAgent: "test", Now: func() time.Time { return fake.Now },
	}
}

func TestRefresh(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantClosed bool
		wantErr    bool
	}{
		{name: "success rotates the token"},
		{name: "invalid_grant closes the session", status: 400, body: `{"error":"invalid_grant","error_description":"Session not active"}`, wantClosed: true},
		{name: "oauth error with 200 closes too", status: 200, body: `{"error":"invalid_grant"}`, wantClosed: true},
		{name: "5xx is transient even with an error field", status: 503, body: `{"error":"temporarily_unavailable"}`, wantErr: true},
		{name: "non-JSON 4xx is transient", status: 403, body: `<html>`, wantErr: true},
		{name: "missing fields", status: 200, body: `{"access_token":"a"}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := fake.NewITMO(t)
			f.TokenStatus, f.TokenBody = tt.status, tt.body
			c := newClient(f)
			var saved []state.Session
			c.Save = func(s state.Session) error { saved = append(saved, s); return nil }
			c.SetSession(&state.Session{Isu: 1, RefreshToken: "R0"})
			err := c.Refresh(context.Background())
			switch {
			case tt.wantClosed:
				if !errors.Is(err, itmo.ErrSessionClosed) {
					t.Fatalf("want closed, got %v", err)
				}
			case tt.wantErr:
				if err == nil || errors.Is(err, itmo.ErrSessionClosed) {
					t.Fatalf("want a transient error, got %v", err)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
				s := c.Session()
				if s.RefreshToken != "SECRET-REFRESH-1" || s.AccessToken != "SECRET-ACCESS-1" || s.Isu != 1 ||
					!s.AccessExpiresAt.Equal(fake.Now.Add(30*time.Minute)) || len(saved) != 1 {
					t.Fatalf("session not rotated and saved: %d saves", len(saved))
				}
			}
			if (tt.wantClosed || tt.wantErr) && len(saved) != 0 {
				t.Fatal("a failed refresh saved a session")
			}
		})
	}
}

func TestUnauthorized(t *testing.T) {
	tests := []struct {
		name       string
		always     int
		once       int
		wantClosed bool
		wantErr    bool
	}{
		{name: "401 once is fixed by a refresh", once: 401},
		{name: "401 after a refresh closes the session", always: 401, wantClosed: true},
		{name: "403 never closes the session", always: 403, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := fake.NewITMO(t)
			f.MyStatus, f.MyStatusOnce, f.Chosen = tt.always, tt.once, []int64{9001}
			c := newClient(f)
			c.SetSession(&state.Session{Isu: 1, RefreshToken: "R0", AccessToken: "SECRET-ACCESS-0", AccessExpiresAt: fake.Now.Add(time.Hour)})
			chosen, err := c.Chosen(context.Background())
			switch {
			case tt.wantClosed:
				if !errors.Is(err, itmo.ErrSessionClosed) {
					t.Fatalf("want closed, got %v", err)
				}
			case tt.wantErr:
				if err == nil || errors.Is(err, itmo.ErrSessionClosed) {
					t.Fatalf("want a plain error, got %v", err)
				}
			default:
				if err != nil || len(chosen) != 1 || chosen[0].ID != 9001 {
					t.Fatalf("got %v, %v", chosen, err)
				}
			}
		})
	}
}

func TestAccessRefreshedBeforeExpiry(t *testing.T) {
	f := fake.NewITMO(t)
	c := newClient(f)
	c.SetSession(&state.Session{RefreshToken: "R0", AccessToken: "SECRET-ACCESS-0", AccessExpiresAt: fake.Now.Add(20 * time.Second)})
	if _, err := c.Chosen(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, refreshes := f.Snapshot(); refreshes != 1 {
		t.Fatalf("want a refresh 30 s before expiry, got %d", refreshes)
	}
}
