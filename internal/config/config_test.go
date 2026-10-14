package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		file    string
		check   func(t *testing.T, c Config)
		wantErr string
	}{
		{
			name: "defaults",
			check: func(t *testing.T, c Config) {
				if c.BackendURL != DefaultBackendURL || !c.Features["sport"] || !c.Features["friends"] || c.NtfyURL != "" {
					t.Fatalf("unexpected defaults: %+v", c)
				}
				if c.UI != "auto" || c.LogLevel != slog.LevelInfo {
					t.Fatalf("ui %q level %v", c.UI, c.LogLevel)
				}
				if c.TokenFile != filepath.Join(c.StateDir, "agent-token") {
					t.Fatalf("token file %q", c.TokenFile)
				}
			},
		},
		{
			name: "file values, comments and quotes",
			file: "# comment\n\nexport IW_FEATURES = sport\nIW_NOTIFY_NTFY_URL=\"https://ntfy.example/topic\"\nIW_AGENT_NAME='my nas'\n",
			check: func(t *testing.T, c Config) {
				if len(c.Features) != 1 || !c.Features["sport"] || c.NtfyURL != "https://ntfy.example/topic" || c.Name != "my nas" {
					t.Fatalf("file not applied: %+v", c)
				}
			},
		},
		{
			name: "environment wins over the file",
			env:  map[string]string{"IW_FEATURES": "friends", "IW_BACKEND_URL": "https://dev.widgets.alllexey.dev/"},
			file: "IW_FEATURES=sport\nIW_BACKEND_URL=https://other.example\n",
			check: func(t *testing.T, c Config) {
				if !c.Features["friends"] || c.Features["sport"] || c.BackendURL != "https://dev.widgets.alllexey.dev" {
					t.Fatalf("env did not win: %+v", c)
				}
			},
		},
		{
			name: "loopback http is allowed",
			env:  map[string]string{"IW_BACKEND_URL": "http://127.0.0.1:8080"},
			check: func(t *testing.T, c Config) {
				if c.BackendURL != "http://127.0.0.1:8080" {
					t.Fatal(c.BackendURL)
				}
			},
		},
		{
			name: "long name is cut to 60 characters",
			env:  map[string]string{"IW_AGENT_NAME": strings.Repeat("я", 70)},
			check: func(t *testing.T, c Config) {
				if c.Name != strings.Repeat("я", 60) {
					t.Fatal(c.Name)
				}
			},
		},
		{
			name: "ui and log level",
			env:  map[string]string{"IW_UI": "Plain", "IW_LOG_LEVEL": "warn"},
			check: func(t *testing.T, c Config) {
				if c.UI != "plain" || c.LogLevel != slog.LevelWarn {
					t.Fatalf("ui %q level %v", c.UI, c.LogLevel)
				}
			},
		},
		{name: "bad ui", env: map[string]string{"IW_UI": "fancy"}, wantErr: "IW_UI"},
		{name: "bad log level", env: map[string]string{"IW_LOG_LEVEL": "loud"}, wantErr: "IW_LOG_LEVEL"},
		{name: "remote http is refused", env: map[string]string{"IW_BACKEND_URL": "http://widgets.alllexey.dev"}, wantErr: "https"},
		{name: "unknown feature", env: map[string]string{"IW_FEATURES": "sport,marks"}, wantErr: "marks"},
		{name: "bad file line", file: "IW_FEATURES\n", wantErr: "line 1"},
		{name: "bad ntfy url", env: map[string]string{"IW_NOTIFY_NTFY_URL": "ntfy.sh/topic"}, wantErr: "NTFY"},
		{name: "control characters in name", env: map[string]string{"IW_AGENT_NAME": "a\x07b"}, wantErr: "control"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.file != "" {
				if err := os.WriteFile(filepath.Join(dir, FileName), []byte(tt.file), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			env := map[string]string{"IW_STATE_DIR": dir}
			for k, v := range tt.env {
				env[k] = v
			}
			c, err := Load(func(k string) string { return env[k] })
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error with %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.StateDir != dir {
				t.Fatalf("state dir %q", c.StateDir)
			}
			tt.check(t, c)
		})
	}
}
