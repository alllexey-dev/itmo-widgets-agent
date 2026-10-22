// Package backend is the client for the ITMO.Widgets Backend routes an agent connection may call
// (Backend docs/contracts/connections.md).
package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// APIError is a non-success answer: HTTP status and the envelope's error code.
type APIError struct {
	Status int
	Code   string
}

func (e *APIError) Error() string { return fmt.Sprintf("backend HTTP %d %s", e.Status, e.Code) }

// StatusOf returns the HTTP status of an APIError, or 0.
func StatusOf(err error) int {
	var api *APIError
	if errors.As(err, &api) {
		return api.Status
	}
	return 0
}

// CodeOf returns the envelope error code of an APIError, or "".
func CodeOf(err error) string {
	var api *APIError
	if errors.As(err, &api) {
		return api.Code
	}
	return ""
}

type Client struct {
	BaseURL   string
	Token     string
	UserAgent string
	// XClient is the X-Client header value, see BuildInfo.XClient; empty sends none.
	XClient string
	HTTP    *http.Client
}

// BuildInfo is the build identity set through -ldflags.
type BuildInfo struct {
	Version, Commit, Channel, Source string
}

// XClient is the X-Client header (ADR 0034 in ITMO.Widgets): key=value pairs in a fixed order, empty values left
// out. Each value keeps only [A-Za-z0-9._/+-] and at most 64 characters, so the header stays under 512.
func (b BuildInfo) XClient() string {
	pairs := [][2]string{{"kind", "agent"}, {"version", b.Version}, {"channel", b.Channel}, {"source", b.Source},
		{"commit", b.Commit}, {"os", runtime.GOOS + "-" + runtime.GOARCH}}
	var out []string
	for _, p := range pairs {
		v := strings.Map(func(r rune) rune {
			if 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' || strings.ContainsRune("._/+-", r) {
				return r
			}
			return -1
		}, p[1])
		if v = v[:min(len(v), 64)]; v != "" {
			out = append(out, p[0]+"="+v)
		}
	}
	return strings.Join(out, "; ")
}

// WithToken returns a copy that authenticates with token.
func (c *Client) WithToken(token string) *Client {
	cp := *c
	cp.Token = token
	return &cp
}

// ClientMe is GET /api/client/me.
type ClientMe struct {
	Isu        int64      `json:"isu"`
	Connection Connection `json:"connection"`
}

// Connection is the part of ConnectionInfo the agent shows.
type Connection struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Scopes []struct {
		Scope struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"scope"`
		Granted bool `json:"granted"`
	} `json:"scopes"`
	ItmoStatus string `json:"itmoStatus"`
}

func (c *Client) Me(ctx context.Context) (ClientMe, error) {
	var out ClientMe
	return out, c.do(ctx, http.MethodGet, "/api/client/me", 0, nil, &out)
}

// RevokeSelf sends DELETE /api/client/self: Backend revokes the calling connection, as «Отключить» in the app does.
func (c *Client) RevokeSelf(ctx context.Context) error {
	return c.do(ctx, http.MethodDelete, "/api/client/self", 0, nil, nil)
}

// ReportStatus sends PUT /api/client/status: the ITMO session state ("ok", "closed" or "none") and the version.
func (c *Client) ReportStatus(ctx context.Context, itmo, version string) error {
	return c.do(ctx, http.MethodPut, "/api/client/status", 0, map[string]string{"itmo": itmo, "version": version}, nil)
}

type Event struct {
	Seq        int64           `json:"seq"`
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	OccurredAt time.Time       `json:"occurredAt"`
	ExpiresAt  time.Time       `json:"expiresAt"`
	Payload    json.RawMessage `json:"payload"`
}

type Page struct {
	Events     []Event `json:"events"`
	NextCursor int64   `json:"nextCursor"`
	HasMore    bool    `json:"hasMore"`
	Gap        bool    `json:"gap"`
}

// Events long-polls GET /api/client/events; itmo is "ok", "closed" or "none". The read timeout is wait + 15 s, as the
// contract asks.
func (c *Client) Events(ctx context.Context, after int64, wait int, itmo string) (Page, error) {
	q := url.Values{"after": {strconv.FormatInt(after, 10)}, "wait": {strconv.Itoa(wait)}, "itmo": {itmo}}
	var out Page
	err := c.do(ctx, http.MethodGet, "/api/client/events?"+q.Encode(), time.Duration(wait+15)*time.Second, nil, &out)
	return out, err
}

// Entry is a sport queue entry from entry/my; free entries match lessonId, auto entries realLessonId.
type Entry struct {
	Type         string `json:"type"`
	LessonID     *int64 `json:"lessonId"`
	RealLessonID *int64 `json:"realLessonId"`
	Status       string `json:"status"`
	IsCancelled  bool   `json:"isCancelled"`
}

// Entries reads GET /api/sport/{queue}-sign/entry/my; queue is "free" or "auto".
func (c *Client) Entries(ctx context.Context, queue string) ([]Entry, error) {
	var out []Entry
	return out, c.do(ctx, http.MethodGet, "/api/sport/"+queue+"-sign/entry/my", 0, nil, &out)
}

// Report posts POST /api/sport/{queue}-sign/lesson/{id}/{action}; action is "mark-satisfied" or "cancel".
func (c *Client) Report(ctx context.Context, queue string, lessonID int64, action string) error {
	path := fmt.Sprintf("/api/sport/%s-sign/lesson/%d/%s", queue, lessonID, action)
	return c.do(ctx, http.MethodPost, path, 0, nil, nil)
}

// SyncSport posts the future confirmed lesson ids as a bare array to POST /api/sport/sign/sync.
func (c *Client) SyncSport(ctx context.Context, lessonIDs []int64) error {
	if lessonIDs == nil {
		lessonIDs = []int64{}
	}
	return c.do(ctx, http.MethodPost, "/api/sport/sign/sync", 0, lessonIDs, nil)
}

// ChallengeRequest is CreateChallengeRequest; Kind is always CONNECT here.
type ChallengeRequest struct {
	Kind          string   `json:"kind"`
	Client        string   `json:"client"`
	KeyCommitment string   `json:"keyCommitment"`
	Name          string   `json:"name"`
	Version       string   `json:"version,omitempty"`
	Platform      string   `json:"platform,omitempty"`
	Scopes        []string `json:"scopes,omitempty"`
}

// Challenge is CreatedChallenge. ClaimSecret proves the claim and is never shown.
type Challenge struct {
	ID          string    `json:"challengeId"`
	Code        string    `json:"code"`
	ClaimSecret string    `json:"claimSecret"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

// CreatePairing posts POST /api/pair/challenges (anonymous).
func (c *Client) CreatePairing(ctx context.Context, req ChallengeRequest) (Challenge, error) {
	var out Challenge
	return out, c.WithToken("").do(ctx, http.MethodPost, "/api/pair/challenges", 0, req, &out)
}

// RenewPairing posts POST /api/client/pair/renew (connection token) for kind RENEW.
func (c *Client) RenewPairing(ctx context.Context, keyCommitment string) (Challenge, error) {
	var out Challenge
	return out, c.do(ctx, http.MethodPost, "/api/client/pair/renew", 0, map[string]string{"keyCommitment": keyCommitment}, &out)
}

// Claim is PairingClaimResponse: Status PENDING or OPENED until the phone approves, then CLAIMED once.
type Claim struct {
	Status       string `json:"status"`
	Token        string `json:"token"`
	SealedBundle string `json:"sealedBundle"`
}

// ClaimPairing posts POST /api/pair/challenges/{id}/claim (anonymous) with the claim secret.
func (c *Client) ClaimPairing(ctx context.Context, id, claimSecret string) (Claim, error) {
	var out Claim
	return out, c.WithToken("").do(ctx, http.MethodPost, "/api/pair/challenges/"+url.PathEscape(id)+"/claim", 0,
		map[string]string{"claimSecret": claimSecret}, &out)
}

func (c *Client) do(ctx context.Context, method, path string, timeout time.Duration, in, out any) error {
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.UserAgent)
	if c.XClient != "" {
		req.Header.Set("X-Client", c.XClient)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	var env struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
		Error   *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	parseErr := json.Unmarshal(raw, &env)
	code := ""
	if env.Error != nil {
		code = env.Error.Code
	}
	if resp.StatusCode/100 != 2 || (parseErr == nil && !env.Success) {
		return &APIError{Status: resp.StatusCode, Code: code}
	}
	if parseErr != nil {
		return errors.New("backend answer is not JSON")
	}
	if out != nil {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("backend answer has an unexpected shape: %w", err)
		}
	}
	return nil
}
