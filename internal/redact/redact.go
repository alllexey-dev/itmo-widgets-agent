// Package redact is the last line of defence for logs: it masks anything that looks like a token.
package redact

import (
	"log/slog"
	"strings"
)

// prefixes start secrets the agent handles: agent tokens and ITMO.ID JWTs (header and refresh token alike).
var prefixes = []string{"iwa_", "eyJ"}

// String masks every agent token or JWT in s.
func String(s string) string {
	for _, p := range prefixes {
		for from := 0; ; {
			i := strings.Index(s[from:], p)
			if i < 0 {
				break
			}
			start := from + i + len(p)
			end := start
			for end < len(s) && isTokenByte(s[end]) {
				end++
			}
			if end-start < 8 { // too short to be a token: "eyJ" in a word, "iwa_..." in help text
				from = start
				continue
			}
			s = s[:start] + "[redacted]" + s[end:]
			from = start + len("[redacted]")
		}
	}
	return s
}

func isTokenByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.'
}

// Attr is a slog ReplaceAttr that masks tokens in string and error values.
func Attr(_ []string, a slog.Attr) slog.Attr {
	switch v := a.Value; {
	case v.Kind() == slog.KindString:
		a.Value = slog.StringValue(String(v.String()))
	case v.Kind() == slog.KindAny:
		if err, ok := v.Any().(error); ok {
			a.Value = slog.StringValue(String(err.Error()))
		}
	}
	return a
}
