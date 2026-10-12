// Package budget caps requests per host in the HTTP layer, so no code path can exceed the ITMO budget.
package budget

import (
	"errors"
	"net/http"
	"sync"
	"time"
)

// ErrExceeded is returned instead of sending a request over the budget.
var ErrExceeded = errors.New("request budget exceeded")

// Transport allows at most Limit requests per host within any Window.
type Transport struct {
	Base   http.RoundTripper
	Limit  int
	Window time.Duration
	Now    func() time.Time

	mu   sync.Mutex
	sent map[string][]time.Time
}

func New(base http.RoundTripper, limit int, window time.Duration, now func() time.Time) *Transport {
	return &Transport{Base: base, Limit: limit, Window: window, Now: now}
}

func (t *Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !t.take(r.URL.Host) {
		if r.Body != nil {
			r.Body.Close()
		}
		return nil, ErrExceeded
	}
	return t.Base.RoundTrip(r)
}

func (t *Transport) take(host string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sent == nil {
		t.sent = map[string][]time.Time{}
	}
	now := t.Now()
	times := t.sent[host]
	i := 0
	for i < len(times) && now.Sub(times[i]) >= t.Window {
		i++
	}
	times = times[i:]
	if len(times) >= t.Limit {
		t.sent[host] = times
		return false
	}
	t.sent[host] = append(times, now)
	return true
}
