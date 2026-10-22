package main

import (
	"os"
	"strings"
	"testing"
)

func TestAttachWithoutAgent(t *testing.T) {
	dir, err := os.MkdirTemp("", "iwm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("IW_STATE_DIR", dir)
	for _, cmd := range []string{"attach", "watch"} {
		var out, errOut strings.Builder
		if code := run([]string{cmd}, &out, &errOut); code != 1 {
			t.Fatalf("%s: exit %d, stderr %q", cmd, code, errOut.String())
		}
		for _, want := range []string{"агент не запущен", "itmo-widgets-agent status", "itmo-widgets-agent run"} {
			if !strings.Contains(errOut.String(), want) {
				t.Fatalf("%s: stderr lacks %q: %q", cmd, want, errOut.String())
			}
		}
	}
	var out strings.Builder
	if code := run([]string{"status"}, &out, &out); code != 0 || !strings.Contains(out.String(), "не отвечает") {
		t.Fatalf("status: exit %d, %q", code, out.String())
	}
}
