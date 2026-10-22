// Package pair implements the client side of a connection pairing: the ephemeral key, the QR and link, the claim and
// the checks of the sealed ITMO session (ITMO.Widgets connected-clients plan, section 5; Backend connections.md).
// The cryptography is the app's PairingCrypto byte for byte.
package pair

import (
	"crypto/ecdh"
	"crypto/hpke"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	KindConnect  = "CONNECT"
	KindRenew    = "RENEW"
	maxBundleAge = 15 * time.Minute
)

var b64 = base64.RawURLEncoding

// Suite: DHKEM(X25519, HKDF-SHA256) / HKDF-SHA256 / ChaCha20-Poly1305 (RFC 9180 ids 0x0020, 0x0001, 0x0003).
var (
	kem  = hpke.DHKEM(ecdh.X25519())
	kdf  = hpke.HKDFSHA256()
	aead = hpke.ChaCha20Poly1305()
)

// Target holds the ephemeral pairing secrets; they live in memory only.
type Target struct {
	key    hpke.PrivateKey
	PK     []byte
	Secret []byte
}

func NewTarget() (*Target, error) {
	key, err := kem.GenerateKey()
	if err != nil {
		return nil, err
	}
	pk := key.PublicKey().Bytes()
	s := make([]byte, 16)
	if _, err := rand.Read(s); err != nil {
		return nil, err
	}
	return &Target{key: key, PK: pk, Secret: s}, nil
}

// Commitment = SHA-256("iw-pair-commit-v1" || pk || s), base64url without padding.
func Commitment(pk, s []byte) string {
	h := sha256.New()
	h.Write([]byte("iw-pair-commit-v1"))
	h.Write(pk)
	h.Write(s)
	return b64.EncodeToString(h.Sum(nil))
}

// SAS = first 20 bits of SHA-256("iw-pair-sas-v1" || challengeId || pk || s) mod 10^6, as "482 913".
func SAS(challengeID [16]byte, pk, s []byte) string {
	h := sha256.New()
	h.Write([]byte("iw-pair-sas-v1"))
	h.Write(challengeID[:])
	h.Write(pk)
	h.Write(s)
	d := h.Sum(nil)
	n := (uint32(d[0])<<12 | uint32(d[1])<<4 | uint32(d[2])>>4) % 1_000_000
	v := fmt.Sprintf("%06d", n)
	return v[:3] + " " + v[3:]
}

// Info = "itmo-widgets pair v1" || 0x00 || challengeId || kind || 0x00 || s.
func Info(challengeID [16]byte, kind string, s []byte) []byte {
	out := append([]byte("itmo-widgets pair v1"), 0)
	out = append(out, challengeID[:]...)
	out = append(out, kind...)
	out = append(out, 0)
	return append(out, s...)
}

// Link is https://<host>/pair#v1.CODE.pk.s; everything after # stays on the device that opens it.
func Link(origin, code string, pk, s []byte) string {
	return fmt.Sprintf("%s/pair#v1.%s.%s.%s", origin, code, b64.EncodeToString(pk), b64.EncodeToString(s))
}

// ParseUUID turns a challenge UUID into its 16 bytes.
func ParseUUID(s string) ([16]byte, error) {
	var out [16]byte
	h := strings.ReplaceAll(s, "-", "")
	if len(h) != 32 || len(s) != 36 {
		return out, errors.New("challenge id is not a UUID")
	}
	_, err := hex.Decode(out[:], []byte(h))
	return out, err
}

// Bundle is the plaintext the phone seals: the agent's own ITMO.ID session. It is secret: String never prints it.
type Bundle struct {
	V           int       `json:"v"`
	ChallengeID string    `json:"challengeId"`
	Kind        string    `json:"kind"`
	Isu         int64     `json:"isu"`
	IssuedAt    time.Time `json:"issuedAt"`
	Itmo        struct {
		Sid              string    `json:"sid"`
		ExpiresAt        time.Time `json:"expiresAt"`
		RefreshToken     string    `json:"refreshToken"`
		RefreshExpiresAt time.Time `json:"refreshExpiresAt"`
	} `json:"itmo"`
}

func (Bundle) String() string   { return "Bundle[redacted]" }
func (Bundle) GoString() string { return "Bundle[redacted]" }

var ErrBundle = errors.New("the sealed session does not match this pairing")

// Open decrypts the sealed bundle (enc || ciphertext, base64url) and checks that it belongs to this challenge.
func (t *Target) Open(sealed, challengeID, kind string, now time.Time) (Bundle, error) {
	var b Bundle
	id, err := ParseUUID(challengeID)
	if err != nil {
		return b, err
	}
	raw, err := b64.DecodeString(strings.TrimRight(sealed, "="))
	if err != nil || len(raw) < 48 || len(raw) > 16<<10 {
		return b, ErrBundle
	}
	plain, err := hpke.Open(t.key, kdf, aead, Info(id, kind, t.Secret), raw)
	if err != nil {
		return b, ErrBundle
	}
	if json.Unmarshal(plain, &b) != nil || b.V != 1 || !strings.EqualFold(b.ChallengeID, challengeID) ||
		b.Kind != kind || b.Itmo.RefreshToken == "" || b.Itmo.Sid == "" || b.Isu <= 0 {
		return Bundle{}, ErrBundle
	}
	if age := now.Sub(b.IssuedAt); age > maxBundleAge || age < -maxBundleAge {
		return Bundle{}, fmt.Errorf("%w: issued too long ago", ErrBundle)
	}
	return b, nil
}
