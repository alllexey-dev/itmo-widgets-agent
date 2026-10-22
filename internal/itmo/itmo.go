// Package itmo is the agent's minimal client for ITMO.ID and My ITMO: token refresh, logout of its own session, chosen
// sport lessons and the sport sign-up. Nothing else. MyItmoApi (github.com/alllexey123/MyItmoApi) is the reference implementation; the
// wire details below mirror its ItmoIdClient, ItmoTransport and SportApi.
package itmo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alllexey-dev/itmo-widgets-agent/internal/state"
)

const (
	DefaultTokenURL = "https://id.itmo.ru/auth/realms/itmo/protocol/openid-connect/token"
	DefaultBaseURL  = "https://my.itmo.ru"
	clientID        = "student-personal-cabinet"
	// NoCapacityMessage is MyITMO's legacy "no free places" refusal; the server appends student and lesson context.
	NoCapacityMessage = "нельзя записать студента: нет свободных мест на занятии"
	accessSkew        = 30 * time.Second
	maxBody           = 4 << 20
)

var (
	// ErrSessionClosed means ITMO.ID ended the session: an OAuth error from the token endpoint, or MyITMO still
	// answering 401 after a refresh. Never retried.
	ErrSessionClosed = errors.New("ITMO session closed")
	ErrNoSession     = errors.New("no ITMO session")
)

// APIError is MyITMO's error envelope (error_code or code not 0), which may come with any HTTP status.
type APIError struct {
	Status  int
	Code    int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("MyITMO error code %d (HTTP %d)", e.Code, e.Status)
}

type HTTPError struct{ Status int }

func (e *HTTPError) Error() string { return fmt.Sprintf("ITMO HTTP %d", e.Status) }

type Client struct {
	HTTP      *http.Client
	TokenURL  string
	BaseURL   string
	UserAgent string
	Now       func() time.Time
	// Save persists a rotated session; nil keeps it in memory only.
	Save func(state.Session) error

	session *state.Session
}

func (c *Client) SetSession(s *state.Session) { c.session = s }

// Session returns the current session; nil when there is none.
func (c *Client) Session() *state.Session { return c.session }

// Refresh exchanges the refresh token for a new token set and stores it.
func (c *Client) Refresh(ctx context.Context) error {
	if c.session == nil {
		return ErrNoSession
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {c.session.RefreshToken},
		"client_id":     {clientID},
		// The 1.x wire form: plural "scopes", as MyItmoApi sends it.
		"scopes": {"openid profile"},
	}
	status, body, err := c.postForm(ctx, c.TokenURL, form)
	if err != nil {
		return err
	}
	if status >= 500 {
		return &HTTPError{status}
	}
	var wire struct {
		Error            string `json:"error"`
		AccessToken      string `json:"access_token"`
		ExpiresIn        int64  `json:"expires_in"`
		RefreshToken     string `json:"refresh_token"`
		RefreshExpiresIn int64  `json:"refresh_expires_in"`
		IDToken          string `json:"id_token"`
	}
	parseErr := json.Unmarshal(body, &wire)
	if strings.TrimSpace(wire.Error) != "" {
		return ErrSessionClosed
	}
	if status/100 != 2 {
		return &HTTPError{status}
	}
	if parseErr != nil || wire.AccessToken == "" || wire.RefreshToken == "" || wire.IDToken == "" ||
		wire.ExpiresIn < 0 || wire.RefreshExpiresIn < 0 {
		return errors.New("ITMO.ID token answer is malformed")
	}
	now := c.Now()
	next := state.Session{
		Isu:              c.session.Isu,
		Sid:              c.session.Sid,
		ExpiresAt:        c.session.ExpiresAt,
		RefreshToken:     wire.RefreshToken,
		RefreshExpiresAt: now.Add(time.Duration(wire.RefreshExpiresIn) * time.Second),
		AccessToken:      wire.AccessToken,
		AccessExpiresAt:  now.Add(time.Duration(wire.ExpiresIn) * time.Second),
		RefreshedAt:      now,
	}
	c.session = &next
	if c.Save != nil {
		return c.Save(next)
	}
	return nil
}

// Logout ends this session at ITMO.ID: the OIDC logout with its refresh token ends exactly its sid, other sessions of
// the account live. An OAuth error answer means the session had already ended, which is success too.
func (c *Client) Logout(ctx context.Context, refreshToken string) error {
	form := url.Values{"client_id": {clientID}, "refresh_token": {refreshToken}}
	status, body, err := c.postForm(ctx, strings.TrimSuffix(c.TokenURL, "/token")+"/logout", form)
	if err != nil || status/100 == 2 {
		return err
	}
	var wire struct {
		Error string `json:"error"`
	}
	if status < 500 && json.Unmarshal(body, &wire) == nil && strings.TrimSpace(wire.Error) != "" {
		return nil
	}
	return &HTTPError{status}
}

func (c *Client) postForm(ctx context.Context, endpoint string, form url.Values) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", c.UserAgent)
	return c.send(req)
}

// ChosenLesson is a lesson the student is signed up for.
type ChosenLesson struct {
	ID  int64
	End time.Time
}

// Chosen reads GET /api/sport/sign/chosen: result[].lesson_groups[].lessons[].
func (c *Client) Chosen(ctx context.Context) ([]ChosenLesson, error) {
	var result []struct {
		LessonGroups []struct {
			Lessons []struct {
				ID      int64  `json:"id"`
				DateEnd string `json:"date_end"`
			} `json:"lessons"`
		} `json:"lesson_groups"`
	}
	if err := c.call(ctx, http.MethodGet, "/api/sport/sign/chosen", nil, &result); err != nil {
		return nil, err
	}
	var out []ChosenLesson
	for _, section := range result {
		for _, group := range section.LessonGroups {
			for _, l := range group.Lessons {
				end, _ := time.Parse(time.RFC3339, l.DateEnd)
				out = append(out, ChosenLesson{ID: l.ID, End: end})
			}
		}
	}
	return out, nil
}

// Outcome of a sign-up, mapped like the app's SportSignOutcome.
type Outcome int

const (
	RetryLater Outcome = iota
	SignedIn
	NoCapacity
	Rejected
)

func (o Outcome) String() string {
	return [...]string{"RETRY_LATER", "SIGNED_IN", "NO_CAPACITY", "REJECTED"}[o]
}

// SignIn posts POST /api/sport/sign/schedule/lessons [id]. The error is returned too, so callers can tell a closed
// session from other retriable failures.
func (c *Client) SignIn(ctx context.Context, lessonID int64) (Outcome, error) {
	err := c.call(ctx, http.MethodPost, "/api/sport/sign/schedule/lessons", []int64{lessonID}, nil)
	return Classify(err), err
}

// Classify: only MyITMO's own refusal on a 2xx answer decides about the lesson; anything else is retried later.
func Classify(err error) Outcome {
	if err == nil {
		return SignedIn
	}
	var api *APIError
	if !errors.As(err, &api) || api.Status/100 != 2 {
		return RetryLater
	}
	rest := strings.ReplaceAll(api.Message, NoCapacityMessage, "")
	if strings.Contains(api.Message, NoCapacityMessage) && utf8.RuneCountInString(rest) > 20 {
		return NoCapacity
	}
	return Rejected
}

func (c *Client) call(ctx context.Context, method, path string, in, out any) error {
	for attempt := 0; ; attempt++ {
		if c.session == nil {
			return ErrNoSession
		}
		if c.session.AccessToken == "" || !c.Now().Add(accessSkew).Before(c.session.AccessExpiresAt) {
			if err := c.Refresh(ctx); err != nil {
				return err
			}
		}
		status, env, err := c.request(ctx, method, path, in)
		if err != nil {
			return err
		}
		if code, msg := env.code(); code != 0 {
			return &APIError{Status: status, Code: code, Message: msg}
		}
		if status == http.StatusUnauthorized {
			if attempt == 0 {
				c.session.AccessToken = ""
				continue
			}
			return ErrSessionClosed
		}
		if status/100 != 2 {
			return &HTTPError{status}
		}
		if out == nil {
			return nil
		}
		if env == nil || json.Unmarshal(env.Result, out) != nil {
			return errors.New("MyITMO answer is malformed")
		}
		return nil
	}
}

type envelope struct {
	ErrorCode    *int            `json:"error_code"`
	ErrorMessage any             `json:"error_message"`
	Code         *int            `json:"code"`
	Message      any             `json:"message"`
	Result       json.RawMessage `json:"result"`
}

func (e *envelope) code() (int, string) {
	if e == nil {
		return 0, ""
	}
	if e.ErrorCode != nil {
		msg, _ := e.ErrorMessage.(string)
		return *e.ErrorCode, msg
	}
	if e.Code != nil {
		msg, _ := e.Message.(string)
		return *e.Code, msg
	}
	return 0, ""
}

func (c *Client) request(ctx context.Context, method, path string, in any) (int, *envelope, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.session.AccessToken)
	req.Header.Set("Accept-Language", "ru")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.UserAgent)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	status, raw, err := c.send(req)
	if err != nil {
		return 0, nil, err
	}
	var env envelope
	if json.Unmarshal(raw, &env) != nil {
		return status, nil, nil
	}
	return status, &env, nil
}

func (c *Client) send(req *http.Request) (int, []byte, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// url.Error keeps the URL only; tokens travel in headers and bodies, never in URLs.
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	return resp.StatusCode, body, err
}

// NoRedirects is the CheckRedirect for ITMO clients: MyITMO answers are never followed.
func NoRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
