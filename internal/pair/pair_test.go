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
	"reflect"
	"regexp"
	"slices"
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

// The phone's side, written independently of package pair from the hand-off plan's formulas (the app's PairingCrypto).

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
		"v": a.version, "challengeId": a.bundleID, "kind": a.bundleKind, "isu": a.isu, "issuedAt": a.issuedAt.Format(time.RFC3339),
		"itmo": map[string]any{"sid": "sid-new", "expiresAt": "2027-01-05T06:00:00Z", "refreshToken": "SECRET-R0",
			"refreshExpiresAt": "2026-11-06T06:00:00Z"},
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
		{name: "sealed for another kind", mutate: func(a *sealArgs) { a.infoKind, a.bundleKind = pair.KindRenew, pair.KindRenew }, wantErr: true},
		{name: "plaintext names another kind", mutate: func(a *sealArgs) { a.bundleKind = pair.KindRenew }, wantErr: true},
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
			a := sealArgs{pk: target.PK, s: target.Secret, infoID: fake.ChallengeID, infoKind: pair.KindConnect,
				bundleID: fake.ChallengeID, bundleKind: pair.KindConnect, isu: 100001, issuedAt: fake.Now.Add(-time.Minute), version: 1}
			tt.mutate(&a)
			b, err := target.Open(seal(t, a), fake.ChallengeID, pair.KindConnect, fake.Now)
			if tt.wantErr {
				if !errors.Is(err, pair.ErrBundle) {
					t.Fatalf("want ErrBundle, got %v", err)
				}
				return
			}
			if err != nil || b.Itmo.RefreshToken != "SECRET-R0" || b.Itmo.Sid != "sid-new" || b.Isu != 100001 ||
				!b.Itmo.ExpiresAt.Equal(time.Date(2027, 1, 5, 6, 0, 0, 0, time.UTC)) {
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
		Scopes: []string{"sport", "friends:read"}, View: pair.Text{W: w.out}, Log: slog.New(slog.NewJSONHandler(w.logs, nil)), Now: now,
		Sleep: func(ctx context.Context, d time.Duration) error { return ctx.Err() },
	}
	return w
}

// phone scripts the claim answers: PENDING, a 429, OPENED, then CLAIMED with a bundle sealed from the printed link.
// expired answers 404 instead of the CLAIMED one.
func (w *world) phone(t *testing.T, kind string, isu int64, expired bool) {
	w.b.Claim = func(n int) (int, string) {
		switch n {
		case 1:
			return 202, `{"status":"PENDING","token":null,"connection":null,"sealedBundle":null}`
		case 2:
			return 429, ""
		case 3:
			return 202, `{"status":"OPENED","token":null,"connection":null,"sealedBundle":null}`
		}
		if expired {
			return 404, ""
		}
		m := linkRE.FindStringSubmatch(w.out.String())
		if m == nil {
			t.Error("no pairing link printed")
			return 404, ""
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
		if kind == pair.KindConnect {
			token = `"` + fake.AgentToken + `"`
		}
		return 200, `{"status":"CLAIMED","token":` + token + `,"connection":{"id":"c1"},"sealedBundle":"` + sealed + `"}`
	}
}

func TestPair(t *testing.T) {
	tests := []struct {
		name        string
		renew       bool
		paired      bool
		isu         int64
		expired     bool
		tokenStatus int
		wantErr     error
		wantFiles   bool
		// wantRevoked: a failed CONNECT revokes its new connection; wantLogouts: ITMO sessions ended.
		wantRevoked bool
		wantLogouts []string
	}{
		{name: "new agent", isu: 100001, wantFiles: true},
		{name: "renew ends the replaced session", renew: true, paired: true, isu: 100001, wantFiles: true,
			wantLogouts: []string{"SECRET-OLD"}},
		{name: "ITMO rejects the new session: nothing written, connection revoked", isu: 100001, tokenStatus: 400,
			wantErr: pair.ErrSessionRejected, wantRevoked: true, wantLogouts: []string{"SECRET-R0"}},
		{name: "session of another account: nothing written, both ended", isu: 100002, wantErr: pair.ErrOtherAccount,
			wantRevoked: true, wantLogouts: []string{"SECRET-R0"}},
		{name: "renew of another account keeps the old files", renew: true, paired: true, isu: 100002,
			wantErr: pair.ErrOtherAccount, wantLogouts: []string{"SECRET-R0"}},
		{name: "expired", isu: 100001, expired: true, wantErr: pair.ErrExpired},
		{name: "pair when already paired", paired: true, isu: 100001, wantErr: pair.ErrPaired},
		{name: "renew without a connection", renew: true, isu: 100001, wantErr: os.ErrNotExist},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			kind := pair.KindConnect
			if tt.renew {
				kind = pair.KindRenew
			}
			if tt.paired {
				if err := w.files.SaveToken(fake.AgentToken); err != nil {
					t.Fatal(err)
				}
				if _, err := w.files.SaveSession(state.Session{Isu: 100001, Sid: "sid-old", RefreshToken: "SECRET-OLD"}); err != nil {
					t.Fatal(err)
				}
			}
			if tt.tokenStatus != 0 {
				w.f.TokenStatus, w.f.TokenBody = tt.tokenStatus, `{"error":"invalid_grant"}`
			}
			w.phone(t, kind, tt.isu, tt.expired)
			err := w.flow.Pair(context.Background(), tt.renew)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("want %v, got %v", tt.wantErr, err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			s, _, hasSession, _ := w.files.Session()
			if hasSession != (tt.wantFiles || tt.paired) || (hasSession && !tt.wantFiles && s.Sid != "sid-old") {
				t.Fatalf("session %v (sid %q), want new %v", hasSession, s.Sid, tt.wantFiles)
			}
			_, tokenErr := w.files.Token()
			if wantToken := tt.wantFiles || tt.paired; (tokenErr == nil) != wantToken {
				t.Fatalf("token present = %v, want %v", tokenErr == nil, wantToken)
			}
			if (w.b.SelfRevokes == 1) != tt.wantRevoked {
				t.Fatalf("revokes %d, want revoked %v", w.b.SelfRevokes, tt.wantRevoked)
			}
			if got := w.f.LoggedOut(); !slices.Equal(got, tt.wantLogouts) {
				t.Fatalf("logged out %v, want %v", got, tt.wantLogouts)
			}
			if tt.wantFiles {
				if s.RefreshToken != "SECRET-REFRESH-1" || s.Isu != 100001 || s.Sid != "sid-new" || w.f.RefreshTokens[0] != "SECRET-R0" ||
					!s.ExpiresAt.Equal(time.Date(2027, 1, 5, 6, 0, 0, 0, time.UTC)) {
					t.Fatal("the new session was not refreshed before saving")
				}
				want := map[string]any{"kind": "CONNECT", "client": "agent", "keyCommitment": w.b.Created[0]["keyCommitment"],
					"name": "nas", "version": "test", "platform": pair.Platform(), "scopes": []any{"sport", "friends:read"}}
				if tt.renew {
					want = map[string]any{"keyCommitment": w.b.Created[0]["keyCommitment"]}
				}
				if !reflect.DeepEqual(w.b.Created[0], want) {
					t.Fatalf("created %v, want %v", w.b.Created[0], want)
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
