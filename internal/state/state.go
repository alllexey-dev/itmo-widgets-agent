// Package state keeps the agent's files: the agent token, the ITMO session and the poll state.
package state

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// MaxSeen is how many handled event ids the agent remembers.
const MaxSeen = 1000

type Files struct {
	Dir       string
	TokenPath string
}

func (f Files) SessionPath() string { return filepath.Join(f.Dir, "session.json") }
func (f Files) StatePath() string   { return filepath.Join(f.Dir, "state.json") }

// Session is the ITMO.ID session. It is secret: String and LogValue never print it.
type Session struct {
	Isu              int64     `json:"isu"`
	RefreshToken     string    `json:"refreshToken"`
	RefreshExpiresAt time.Time `json:"refreshExpiresAt"`
	AccessToken      string    `json:"accessToken,omitempty"`
	AccessExpiresAt  time.Time `json:"accessExpiresAt"`
	RefreshedAt      time.Time `json:"refreshedAt"`
}

func (Session) String() string       { return "Session[redacted]" }
func (Session) GoString() string     { return "Session[redacted]" }
func (Session) LogValue() slog.Value { return slog.StringValue("[redacted]") }
func (s Session) Valid() bool        { return s.RefreshToken != "" }

// State is what the poll loop persists between runs.
type State struct {
	Cursor     int64     `json:"cursor"`
	Seen       []string  `json:"seen"`
	LastPollAt time.Time `json:"lastPollAt"`
	LastError  string    `json:"lastError,omitempty"`
	// SessionClosedMtime is the session file's mtime (Unix ns) when ITMO closed that session; 0 when open.
	SessionClosedMtime int64 `json:"sessionClosedMtime,omitempty"`
}

func (s *State) HasSeen(id string) bool { return slices.Contains(s.Seen, id) }

func (s *State) MarkSeen(id string) {
	s.Seen = append(s.Seen, id)
	if over := len(s.Seen) - MaxSeen; over > 0 {
		s.Seen = slices.Delete(s.Seen, 0, over)
	}
}

// WriteAtomic writes through a temporary file and a rename, so readers never see half a file.
func WriteAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (f Files) Token() (string, error) {
	b, err := os.ReadFile(f.TokenPath)
	if err != nil {
		return "", err
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", errors.New("agent token file is empty")
	}
	return t, nil
}

func (f Files) SaveToken(token string) error {
	return WriteAtomic(f.TokenPath, []byte(token+"\n"), 0o600)
}

// Session returns the stored session and the file's mtime, or ok=false when there is none.
func (f Files) Session() (s Session, mtime int64, ok bool, err error) {
	b, err := os.ReadFile(f.SessionPath())
	if errors.Is(err, os.ErrNotExist) {
		return Session{}, 0, false, nil
	}
	if err != nil {
		return Session{}, 0, false, err
	}
	if err := json.Unmarshal(b, &s); err != nil || !s.Valid() {
		return Session{}, 0, false, errors.New("session.json is damaged")
	}
	mtime, _ = f.SessionMtime()
	return s, mtime, true, nil
}

func (f Files) SessionMtime() (int64, bool) {
	fi, err := os.Stat(f.SessionPath())
	if err != nil {
		return 0, false
	}
	return fi.ModTime().UnixNano(), true
}

func (f Files) SaveSession(s Session) (int64, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return 0, err
	}
	if err := WriteAtomic(f.SessionPath(), b, 0o600); err != nil {
		return 0, err
	}
	m, _ := f.SessionMtime()
	return m, nil
}

// Wipe deletes the agent token and the ITMO session; state.json holds no secret and stays.
func (f Files) Wipe() error {
	var errs []error
	for _, p := range []string{f.TokenPath, f.SessionPath()} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (f Files) State() (State, error) {
	var s State
	b, err := os.ReadFile(f.StatePath())
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

func (f Files) SaveState(s State) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return WriteAtomic(f.StatePath(), b, 0o600)
}
