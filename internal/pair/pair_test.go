package pair_test

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/hpke"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alllexey-dev/itmo-widgets-agent/internal/backend"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/fake"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/itmo"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/pair"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/state"
)

// The phone's side, written independently of package pair from the hand-off plan's formulas.

var raw = base64.RawURLEncoding

func uuidBytes(t testing.TB, id string) []byte {
	b, err := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	if err != nil || len(b) != 16 {
		t.Fatalf("bad uuid %q", id)
	}
	return b
}

func senderCommitment(pk, s []byte) string {
	sum := sha256.Sum256(bytes.Join([][]byte{[]byte("iw-pair-commit-v1"), pk, s}, nil))
	return raw.EncodeToString(sum[:])
}

func senderSAS(id, pk, s []byte) string {
	sum := sha256.Sum256(bytes.Join([][]byte{[]byte("iw-pair-sas-v1"), id, pk, s}, nil))
	v := fmt.Sprintf("%06d", (binary.BigEndian.Uint32(sum[:4])>>12)%1000000)
	return v[:3] + " " + v[3:]
}

type sealArgs struct {
	pk, s       []byte
	infoID      string // challenge id bound in info
	infoKind    string
	bundleID    string // challenge id inside the plaintext
	bundleKind  string
	isu         int64
	issuedAt    time.Time
	version     int
	corruptLast bool
}

func seal(t testing.TB, a sealArgs) string {
	info := []byte("itmo-widgets pair v1\x00")
	info = append(info, uuidBytes(t, a.infoID)...)
	info = append(info, []byte(a.infoKind+"\x00")...)
	info = append(info, a.s...)
	plain, _ := json.Marshal(map[string]any{
		"v": a.version, "challengeId": a.bundleID, "kind": a.bundleKind, "isu": a.isu,
		"refreshToken": "SECRET-R0", "refreshExpiresAt": "2026-11-06T06:00:00Z", "issuedAt": a.issuedAt.Format(time.RFC3339),
	})
	pub, err := hpke.DHKEM(ecdh.X25519()).NewPublicKey(a.pk)
	if err != nil {
		t.Fatal(err)
	}
	ct, err := hpke.Seal(pub, hpke.HKDFSHA256(), hpke.ChaCha20Poly1305(), info, plain)
	if err != nil {
		t.Fatal(err)
	}
	if a.corruptLast {
		ct[len(ct)-1] ^= 1
	}
	return raw.EncodeToString(ct)
}

func TestCommitmentAndSAS(t *testing.T) {
	for i := 0; i < 50; i++ {
		target, err := pair.NewTarget()
		if err != nil {
			t.Fatal(err)
		}
		var id [16]byte
		rand.Read(id[:])
		if got, want := pair.Commitment(target.PK, target.Secret), senderCommitment(target.PK, target.Secret); got != want {
			t.Fatalf("commitment %s, want %s", got, want)
		}
		if got, want := pair.SAS(id, target.PK, target.Secret), senderSAS(id[:], target.PK, target.Secret); got != want {
			t.Fatalf("SAS %s, want %s", got, want)
		}
	}
}

func TestOpen(t *testing.T) {
	const other = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
	otherS := make([]byte, 16)
	rand.Read(otherS)
	tests := []struct {
		name    string
		mutate  func(a *sealArgs)
		openAs  string
		wantErr bool
	}{
		{name: "round trip", mutate: func(*sealArgs) {}},
		{name: "wrong pairing secret", mutate: func(a *sealArgs) { a.s = otherS }, wantErr: true},
		{name: "sealed for another challenge", mutate: func(a *sealArgs) { a.infoID, a.bundleID = other, other }, wantErr: true},
		{name: "plaintext names another challenge", mutate: func(a *sealArgs) { a.bundleID = other }, wantErr: true},
		{name: "sealed for another kind", mutate: func(a *sealArgs) { a.infoKind, a.bundleKind = pair.KindAgentSession, pair.KindAgentSession }, wantErr: true},
		{name: "plaintext names another kind", mutate: func(a *sealArgs) { a.bundleKind = pair.KindAgentSession }, wantErr: true},
		{name: "issued 16 minutes ago", mutate: func(a *sealArgs) { a.issuedAt = fake.Now.Add(-16 * time.Minute) }, wantErr: true},
		{name: "unknown version", mutate: func(a *sealArgs) { a.version = 2 }, wantErr: true},
		{name: "tampered ciphertext", mutate: func(a *sealArgs) { a.corruptLast = true }, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, err := pair.NewTarget()
			if err != nil {
				t.Fatal(err)
			}
			a := sealArgs{pk: target.PK, s: target.Secret, infoID: fake.ChallengeID, infoKind: pair.KindAgent,
				bundleID: fake.ChallengeID, bundleKind: pair.KindAgent, isu: 100001, issuedAt: fake.Now.Add(-time.Minute), version: 1}
			tt.mutate(&a)
			b, err := target.Open(seal(t, a), fake.ChallengeID, pair.KindAgent, fake.Now)
			if tt.wantErr {
				if !errors.Is(err, pair.ErrBundle) {
					t.Fatalf("want ErrBundle, got %v", err)
				}
				return
			}
			if err != nil || b.RefreshToken != "SECRET-R0" || b.Isu != 100001 {
				t.Fatalf("open failed: %v", err)
			}
			if s := fmt.Sprintf("%v %+v %#v", b, b, b); strings.Contains(s, "SECRET") {
				t.Fatal("bundle formatting leaks the token")
			}
		})
	}
}

// syncBuffer lets the fake phone read what the agent printed.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

var linkRE = regexp.MustCompile(`/pair#v1\.([A-Z0-9]{8})\.([A-Za-z0-9_-]{43})\.([A-Za-z0-9_-]{22})`)

type world struct {
	b     *fake.Backend
	f     *fake.ITMO
	out   *syncBuffer
	logs  *syncBuffer
	files state.Files
	flow  *pair.Flow
}

func newWorld(t *testing.T) *world {
	w := &world{b: fake.NewBackend(t), f: fake.NewITMO(t), out: &syncBuffer{}, logs: &syncBuffer{}}
	dir := t.TempDir()
	w.files = state.Files{Dir: dir, TokenPath: filepath.Join(dir, "agent-token")}
	now := func() time.Time { return fake.Now }
	w.flow = &pair.Flow{
		Backend: &backend.Client{BaseURL: w.b.Srv.URL, UserAgent: "t", HTTP: &http.Client{}},
		ITMO:    &itmo.Client{HTTP: &http.Client{}, TokenURL: w.f.TokenURL(), BaseURL: w.f.Srv.URL, UserAgent: "t", Now: now},
		Files:   w.files, Origin: "https://widgets.alllexey.dev", Name: "nas", Version: "test",
		View: pair.Text{W: w.out}, Log: slog.New(slog.NewJSONHandler(w.logs, nil)), Now: now,
		Sleep: func(ctx context.Context, d time.Duration) error { return ctx.Err() },
	}
	return w
}

// phone scripts the poll answers: PENDING, OPENED, then APPROVED with a bundle sealed from the printed link.
func (w *world) phone(t *testing.T, kind string, isu int64, final string) {
	w.b.Poll = func(n int) (int, string) {
		switch n {
		case 1:
			return 200, `{"status":"PENDING"}`
		case 2:
			return 429, ""
		case 3:
			return 200, `{"status":"OPENED"}`
		}
		if final != "" {
			return 200, `{"status":"` + final + `"}`
		}
		m := linkRE.FindStringSubmatch(w.out.String())
		if m == nil {
			t.Error("no pairing link printed")
			return 200, `{"status":"EXPIRED"}`
		}
		pk, _ := raw.DecodeString(m[2])
		s, _ := raw.DecodeString(m[3])
		if got := w.b.Created[0]["keyCommitment"]; got != senderCommitment(pk, s) {
			t.Errorf("commitment %q does not match the link", got)
		}
		if !strings.Contains(w.out.String(), senderSAS(uuidBytes(t, fake.ChallengeID), pk, s)) {
			t.Error("verification code not shown")
		}
		sealed := seal(t, sealArgs{pk: pk, s: s, infoID: fake.ChallengeID, infoKind: kind, bundleID: fake.ChallengeID,
			bundleKind: kind, isu: isu, issuedAt: fake.Now, version: 1})
		token := `null`
		if kind == pair.KindAgent {
			token = `{"token":"` + fake.AgentToken + `","info":{"id":"t1"}}`
		}
		return 200, `{"status":"APPROVED","sealedBundle":"` + sealed + `","agentToken":` + token + `}`
	}
}

func TestPair(t *testing.T) {
	tests := []struct {
		name        string
		session     bool
		isu         int64
		final       string
		tokenStatus int
		wantErr     error
		wantFiles   bool
	}{
		{name: "new agent", isu: 100001, wantFiles: true},
		{name: "new session for a connected agent", session: true, isu: 100001, wantFiles: true},
		{name: "ITMO rejects the handed-over token: nothing written", isu: 100001, tokenStatus: 400, wantErr: pair.ErrSessionRejected},
		{name: "session of another account: nothing written", isu: 100002, wantErr: pair.ErrOtherAccount},
		{name: "rejected on the phone", isu: 100001, final: "REJECTED", wantErr: pair.ErrRejected},
		{name: "expired", isu: 100001, final: "EXPIRED", wantErr: pair.ErrExpired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			kind := pair.KindAgent
			if tt.session {
				kind = pair.KindAgentSession
				if err := w.files.SaveToken(fake.AgentToken); err != nil {
					t.Fatal(err)
				}
			}
			if tt.tokenStatus != 0 {
				w.f.TokenStatus, w.f.TokenBody = tt.tokenStatus, `{"error":"invalid_grant"}`
			}
			w.phone(t, kind, tt.isu, tt.final)
			err := w.flow.Pair(context.Background(), tt.session)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("want %v, got %v", tt.wantErr, err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			_, _, hasSession, _ := w.files.Session()
			if hasSession != tt.wantFiles {
				t.Fatalf("session written = %v, want %v", hasSession, tt.wantFiles)
			}
			_, tokenErr := w.files.Token()
			if wantToken := tt.wantFiles || tt.session; (tokenErr == nil) != wantToken {
				t.Fatalf("token present = %v, want %v", tokenErr == nil, wantToken)
			}
			if tt.wantFiles {
				s, _, _, _ := w.files.Session()
				if s.RefreshToken != "SECRET-REFRESH-1" || s.Isu != 100001 || w.f.RefreshTokens[0] != "SECRET-R0" {
					t.Fatal("the handed-over token was not refreshed before saving")
				}
				if want := map[bool]string{false: "AGENT", true: ""}[tt.session]; w.b.Created[0]["kind"] != want {
					t.Fatalf("created %v", w.b.Created[0])
				}
				fi, _ := os.Stat(w.files.SessionPath())
				if fi.Mode().Perm() != 0o600 {
					t.Fatalf("session mode %v", fi.Mode())
				}
			}
			if strings.Contains(w.logs.String(), "SECRET") || strings.Contains(w.logs.String(), fake.AgentToken) {
				t.Fatalf("logs leak a token: %s", w.logs.String())
			}
		})
	}
}

func TestLogin(t *testing.T) {
	tests := []struct {
		name        string
		stdin       string
		tokenFile   bool
		tokenStatus int
		wantErr     error
	}{
		{name: "refresh and agent token from stdin", stdin: "\n" + fake.AgentToken + "\nSECRET-R0\n"},
		{name: "refresh only, token already saved", stdin: "SECRET-R0\n", tokenFile: true},
		{name: "no refresh token", stdin: fake.AgentToken + "\n", wantErr: pair.ErrNoRefreshToken},
		{name: "no agent token anywhere", stdin: "SECRET-R0\n", wantErr: os.ErrNotExist},
		{name: "ITMO rejects the token", stdin: "SECRET-R0\n", tokenFile: true, tokenStatus: 401, wantErr: pair.ErrSessionRejected},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			if tt.tokenFile {
				w.files.SaveToken(fake.AgentToken)
			}
			if tt.tokenStatus != 0 {
				w.f.TokenStatus, w.f.TokenBody = tt.tokenStatus, `{"error":"invalid_grant"}`
			}
			err := w.flow.Login(context.Background(), strings.NewReader(tt.stdin))
			_, _, hasSession, _ := w.files.Session()
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("want %v, got %v", tt.wantErr, err)
				}
				if hasSession {
					t.Fatal("session written after a failure")
				}
				return
			}
			if err != nil || !hasSession {
				t.Fatalf("login failed: %v", err)
			}
			if tok, _ := w.files.Token(); tok != fake.AgentToken {
				t.Fatal("agent token not saved")
			}
		})
	}
}

func TestQR(t *testing.T) {
	var out bytes.Buffer
	link := "https://widgets.alllexey.dev/pair#v1.ABCD2345." + strings.Repeat("A", 43) + "." + strings.Repeat("B", 22)
	if err := pair.WriteQR(&out, link); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) > 30 || !strings.HasPrefix(lines[0], "\x1b[30;107m") {
		t.Fatalf("QR has %d rows or no explicit colours", len(lines))
	}
}
