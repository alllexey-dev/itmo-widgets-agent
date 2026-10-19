package backend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

func TestXClient(t *testing.T) {
	osArch := "os=" + runtime.GOOS + "-" + runtime.GOARCH
	long := strings.Repeat("a", 70)
	for _, c := range []struct {
		name string
		in   BuildInfo
		want string
	}{
		{"all keys in order", BuildInfo{Version: "1.2.0", Commit: "abc123", Channel: "github", Source: "alllexey-dev/itmo-widgets-agent"},
			"kind=agent; version=1.2.0; channel=github; source=alllexey-dev/itmo-widgets-agent; commit=abc123; " + osArch},
		{"empty values left out", BuildInfo{Version: "dev", Channel: "custom", Source: "local"},
			"kind=agent; version=dev; channel=custom; source=local; " + osArch},
		{"bad characters dropped", BuildInfo{Version: "1.0 beta;x=1", Channel: "\u043c\u043e\u0439", Source: "a b"},
			"kind=agent; version=1.0betax1; source=ab; " + osArch},
		{"truncated to 64", BuildInfo{Version: long},
			"kind=agent; version=" + long[:64] + "; " + osArch},
	} {
		if got := c.in.XClient(); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestRequestsCarryXClient(t *testing.T) {
	var ua, xc string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua, xc = r.Header.Get("User-Agent"), r.Header.Get("X-Client")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success":true,"data":{"isu":1}}`))
	}))
	defer srv.Close()
	want := BuildInfo{Version: "1.0.0"}.XClient()
	c := &Client{BaseURL: srv.URL, UserAgent: "itmo-widgets-agent/1.0.0", XClient: want, HTTP: srv.Client()}
	if _, err := c.Me(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ua != "itmo-widgets-agent/1.0.0" || xc != want {
		t.Fatalf("User-Agent %q, X-Client %q", ua, xc)
	}
}
