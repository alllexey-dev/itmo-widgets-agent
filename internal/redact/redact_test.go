package redact

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestString(t *testing.T) {
	tests := []struct{ in, want string }{
		{"token iwa_AbCdEfGhIjKlMnOp-_12 end", "token iwa_[redacted] end"},
		{"jwt eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiIxIn0.c2ln and eyJabcdefghij", "jwt eyJ[redacted] and eyJ[redacted]"},
		{"plain iwa_... and eyJ text", "plain iwa_... and eyJ text"},
		{"backend HTTP 500 internal_error", "backend HTTP 500 internal_error"},
	}
	for _, tt := range tests {
		if got := String(tt.in); got != tt.want {
			t.Errorf("String(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestAttrInLogs(t *testing.T) {
	var out bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&out, &slog.HandlerOptions{ReplaceAttr: Attr}))
	log.With("ctx", "iwa_SECRETSECRETSECRET").Warn("failed iwa_SECRETSECRETSECRET",
		"err", errors.New("Bearer eyJSECRETSECRET.SECRET"), "n", 5)
	if strings.Contains(out.String(), "SECRET") {
		t.Fatalf("not redacted: %s", out.String())
	}
}
