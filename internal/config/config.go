// Package config reads the agent settings from the environment and an optional KEY=VALUE file.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	DefaultBackendURL = "https://widgets.alllexey.dev"
	FileName          = "agent.env"
)

// Features the agent understands in IW_FEATURES.
var knownFeatures = map[string]bool{"sport": true, "friends": true}

type Config struct {
	BackendURL string
	StateDir   string
	TokenFile  string
	NtfyURL    string
	Name       string
	Features   map[string]bool
}

// Load reads the settings. getenv is os.Getenv in production; values from the environment win over
// the file $IW_STATE_DIR/agent.env.
func Load(getenv func(string) string) (Config, error) {
	dir := getenv("IW_STATE_DIR")
	if dir == "" {
		dir = DefaultStateDir()
	}
	file, err := readFile(filepath.Join(dir, FileName))
	if err != nil {
		return Config{}, err
	}
	get := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		if v := file[key]; v != "" {
			return v
		}
		return def
	}
	host, _ := os.Hostname()
	cfg := Config{
		BackendURL: strings.TrimRight(get("IW_BACKEND_URL", DefaultBackendURL), "/"),
		StateDir:   dir,
		TokenFile:  get("IW_AGENT_TOKEN_FILE", filepath.Join(dir, "agent-token")),
		NtfyURL:    get("IW_NOTIFY_NTFY_URL", ""),
		Name:       get("IW_AGENT_NAME", host),
	}
	if err := checkBackendURL(cfg.BackendURL); err != nil {
		return Config{}, err
	}
	if cfg.NtfyURL != "" {
		if u, err := url.Parse(cfg.NtfyURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return Config{}, errors.New("IW_NOTIFY_NTFY_URL must be an http(s) URL")
		}
	}
	if cfg.Name, err = cleanName(cfg.Name); err != nil {
		return Config{}, err
	}
	if cfg.Features, err = parseFeatures(get("IW_FEATURES", "sport,friends")); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// DefaultStateDir is /data in the container, else the user's config directory.
func DefaultStateDir() string {
	if fi, err := os.Stat("/data"); err == nil && fi.IsDir() {
		return "/data"
	}
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "itmo-widgets-agent")
	}
	return "data"
}

func readFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseFile(f)
}

func parseFile(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(strings.TrimPrefix(key, "export "))
		if !ok || key == "" {
			return nil, fmt.Errorf("%s line %d: expected KEY=VALUE", FileName, n)
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		out[key] = value
	}
	return out, sc.Err()
}

func parseFeatures(s string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, f := range strings.Split(s, ",") {
		f = strings.ToLower(strings.TrimSpace(f))
		if f == "" {
			continue
		}
		if !knownFeatures[f] {
			return nil, fmt.Errorf("IW_FEATURES: unknown feature %q (known: sport, friends)", f)
		}
		out[f] = true
	}
	return out, nil
}

// checkBackendURL allows plain http only for loopback, so the agent token never crosses a network in the clear.
func checkBackendURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.RawQuery != "" || u.User != nil {
		return errors.New("IW_BACKEND_URL must be an https URL without query or credentials")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if h := u.Hostname(); h == "localhost" || h == "127.0.0.1" || h == "::1" {
			return nil
		}
	}
	return errors.New("IW_BACKEND_URL must use https")
}

func cleanName(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		s = "agent"
	}
	if strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return "", errors.New("IW_AGENT_NAME must not contain control characters")
	}
	for utf8.RuneCountInString(s) > 60 {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return strings.TrimSpace(s), nil
}
