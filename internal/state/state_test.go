package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFilesRoundTripWithPrivateModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new")
	f := Files{Dir: dir, TokenPath: filepath.Join(dir, "agent-token")}
	if err := f.SaveToken("iwa_test"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.SaveSession(Session{Isu: 1, RefreshToken: "r"}); err != nil {
		t.Fatal(err)
	}
	if err := f.SaveState(State{Cursor: 7}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{f.TokenPath, f.SessionPath(), f.StatePath()} {
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: mode %v err %v", p, fi.Mode(), err)
		}
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", fi.Mode())
	}
	if tok, _ := f.Token(); tok != "iwa_test" {
		t.Fatalf("token %q", tok)
	}
	if s, _, ok, err := f.Session(); !ok || err != nil || s.Isu != 1 {
		t.Fatalf("session %v %v", ok, err)
	}
	if st, _ := f.State(); st.Cursor != 7 {
		t.Fatalf("cursor %d", st.Cursor)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 3 {
		t.Fatalf("temporary files left: %v", entries)
	}
	if err := f.Wipe(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Token(); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("token not wiped")
	}
	if _, _, ok, _ := f.Session(); ok {
		t.Fatal("session not wiped")
	}
	if st, _ := f.State(); st.Cursor != 7 {
		t.Fatal("state.json must survive a wipe")
	}
}

func TestDamagedSession(t *testing.T) {
	f := Files{Dir: t.TempDir()}
	os.WriteFile(f.SessionPath(), []byte(`{"isu":1}`), 0o600)
	if _, _, ok, err := f.Session(); ok || err == nil {
		t.Fatal("a session without a refresh token must be damaged")
	}
}

func TestSecretsNeverPrinted(t *testing.T) {
	s := Session{RefreshToken: "SECRET", AccessToken: "SECRET"}
	if out := fmt.Sprintf("%v %+v %#v %s", s, s, s, s); strings.Contains(out, "SECRET") {
		t.Fatalf("printed %q", out)
	}
}

func TestHistoryKeepsTheNewest(t *testing.T) {
	var st State
	for i := range MaxSeen + 5 {
		st.MarkSeen(fmt.Sprint(i))
	}
	for i := range MaxHistory + 3 {
		st.AddEvent(EventRecord{Seq: int64(i)})
		st.AddSport(SportAttempt{LessonID: int64(i), At: time.Unix(int64(i), 0)})
	}
	if len(st.Seen) != MaxSeen || st.HasSeen("4") || !st.HasSeen("5") {
		t.Fatalf("seen kept %d, oldest %s", len(st.Seen), st.Seen[0])
	}
	if len(st.Events) != MaxHistory || st.Events[0].Seq != 3 || len(st.Sport) != MaxHistory || st.Sport[MaxHistory-1].LessonID != MaxHistory+2 {
		t.Fatalf("history %+v %+v", st.Events, st.Sport)
	}
}
