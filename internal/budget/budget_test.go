package budget

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestBudgetCeiling(t *testing.T) {
	var served atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served.Add(1) }))
	defer srv.Close()
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer other.Close()

	now := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	client := &http.Client{Transport: New(http.DefaultTransport, 60, time.Hour, func() time.Time { return now })}
	get := func(url string) error {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}

	tests := []struct {
		name    string
		advance time.Duration
		url     string
		wantErr bool
	}{
		{name: "61st request within the hour is refused", url: srv.URL, wantErr: true},
		{name: "another host has its own budget", url: other.URL},
		{name: "still refused 59 minutes later", advance: 59 * time.Minute, url: srv.URL, wantErr: true},
		{name: "allowed once the window slid past the first requests", advance: time.Minute, url: srv.URL},
	}
	for i := 0; i < 60; i++ {
		if err := get(srv.URL); err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now = now.Add(tt.advance)
			before := served.Load()
			err := get(tt.url)
			if tt.wantErr {
				if !errors.Is(err, ErrExceeded) {
					t.Fatalf("want ErrExceeded, got %v", err)
				}
				if served.Load() != before {
					t.Fatal("a refused request reached the server")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
