// Package backend is the client for the ITMO.Widgets Backend routes an agent may call
// (Backend docs/contracts/agent.md and pairing.md).
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
	"strconv"
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
	HTTP      *http.Client
}

// WithToken returns a copy that authenticates with token.
func (c *Client) WithToken(token string) *Client {
	cp := *c
	cp.Token = token
	return &cp
}

type Identity struct {
	Isu     int64    `json:"isu"`
	TokenID string   `json:"tokenId"`
	Name    string   `json:"name"`
	Scopes  []string `json:"scopes"`
}

func (c *Client) Me(ctx context.Context) (Identity, error) {
	var out Identity
	return out, c.do(ctx, http.MethodGet, "/api/agent/me", 0, nil, &out)
}

// RevokeSelf sends DELETE /api/agent/self: Backend revokes the calling token, as removing the device in the app does.
func (c *Client) RevokeSelf(ctx context.Context) error {
	return c.do(ctx, http.MethodDelete, "/api/agent/self", 0, nil, nil)
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

// Events long-polls GET /api/agent/events; the read timeout is wait + 15 s, as the contract asks.
func (c *Client) Events(ctx context.Context, after int64, wait int, itmoOK bool) (Page, error) {
	itmo := "none"
	if itmoOK {
		itmo = "ok"
	}
	q := url.Values{"after": {strconv.FormatInt(after, 10)}, "wait": {strconv.Itoa(wait)}, "itmo": {itmo}}
	var out Page
	err := c.do(ctx, http.MethodGet, "/api/agent/events?"+q.Encode(), time.Duration(wait+15)*time.Second, nil, &out)
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

type Challenge struct {
	ID         string    `json:"id"`
	Code       string    `json:"code"`
	PollSecret string    `json:"pollSecret"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

// CreatePairing posts POST /api/pair/challenges (anonymous) for kind AGENT.
func (c *Client) CreatePairing(ctx context.Context, keyCommitment, name, version string) (Challenge, error) {
	body := map[string]string{"kind": "AGENT", "keyCommitment": keyCommitment, "name": name, "version": version}
	var out Challenge
	return out, c.WithToken("").do(ctx, http.MethodPost, "/api/pair/challenges", 0, body, &out)
}

// CreateSessionPairing posts POST /api/agent/pair/session (agent token) for kind AGENT_SESSION.
func (c *Client) CreateSessionPairing(ctx context.Context, keyCommitment string) (Challenge, error) {
	var out Challenge
	return out, c.do(ctx, http.MethodPost, "/api/agent/pair/session", 0, map[string]string{"keyCommitment": keyCommitment}, &out)
}

type Poll struct {
	Status       string `json:"status"`
	SealedBundle string `json:"sealedBundle"`
	AgentToken   *struct {
		Token string `json:"token"`
	} `json:"agentToken"`
}

// PollPairing reads GET /api/pair/challenges/{id} with X-Poll-Secret.
func (c *Client) PollPairing(ctx context.Context, id, pollSecret string) (Poll, error) {
	var out Poll
	return out, c.WithToken("").doWith(ctx, http.MethodGet, "/api/pair/challenges/"+url.PathEscape(id), 0, nil, &out,
		map[string]string{"X-Poll-Secret": pollSecret})
}

func (c *Client) do(ctx context.Context, method, path string, timeout time.Duration, in, out any) error {
	return c.doWith(ctx, method, path, timeout, in, out, nil)
}

func (c *Client) doWith(ctx context.Context, method, path string, timeout time.Duration, in, out any, headers map[string]string) error {
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
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
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
