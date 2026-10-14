// Package fake has httptest stand-ins for Backend, ITMO.ID, My ITMO and ntfy. Tests only.
package fake

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alllexey-dev/itmo-widgets-agent/internal/backend"
)

// Now is the tests' clock: 7 Oct 2026 09:00 Moscow time, before the fixture lessons.
var Now = time.Date(2026, 10, 7, 6, 0, 0, 0, time.UTC)

// Clock is a settable clock.
type Clock struct {
	mu sync.Mutex
	T  time.Time
}

func NewClock() *Clock { return &Clock{T: Now} }

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.T
}

func (c *Clock) Add(d time.Duration) {
	c.mu.Lock()
	c.T = c.T.Add(d)
	c.mu.Unlock()
}

const AgentToken = "iwa_SECRET-AGENT-TOKEN-0123456789abcdefghijklmnopq"

// Response is one scripted answer to GET /api/agent/events.
type Response struct {
	Status int
	Code   string
	Page   backend.Page
}

type Backend struct {
	Srv   *httptest.Server
	Token string
	Isu   int64

	mu        sync.Mutex
	Responses []Response
	// OnDrained runs when the scripted event answers are used up; the answer is then an empty page.
	OnDrained func()
	// BeforeEvents runs before each events answer, with the poll's itmo parameter.
	BeforeEvents func(itmo string)
	Entries      map[string]string
	ItmoParams   []string
	Afters       []string
	Reports      []string
	Syncs        [][]int64
	Created      []map[string]string
	// SelfStatus, SelfCode answer DELETE /api/agent/self when set; otherwise the token is revoked and later calls get 401.
	SelfStatus  int
	SelfCode    string
	SelfRevokes int
	// Poll answers GET /api/pair/challenges/{id}; n counts polls from 1.
	Poll  func(n int) (int, string)
	polls int
}

const ChallengeID = "0f8fad5b-d9cb-469f-a165-70867728950e"

func NewBackend(t testing.TB) *Backend {
	b := &Backend{Token: AgentToken, Isu: 100001, Entries: map[string]string{}}
	b.Srv = httptest.NewServer(http.HandlerFunc(b.serve))
	t.Cleanup(b.Srv.Close)
	return b
}

func ok(w http.ResponseWriter, data string) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"success":true,"data":%s,"error":null}`, data)
}

func fail(w http.ResponseWriter, status int, code string) {
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"success":false,"data":null,"error":{"code":%q}}`, code)
}

func (b *Backend) serve(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p := r.URL.Path
	anonymous := p == "/api/pair/challenges" || strings.HasPrefix(p, "/api/pair/challenges/")
	if !anonymous && r.Header.Get("Authorization") != "Bearer "+b.Token {
		fail(w, 401, "unauthorized")
		return
	}
	body, _ := io.ReadAll(r.Body)
	parts := strings.Split(strings.Trim(p, "/"), "/")
	switch {
	case p == "/api/agent/events":
		q := r.URL.Query()
		b.ItmoParams = append(b.ItmoParams, q.Get("itmo"))
		b.Afters = append(b.Afters, q.Get("after"))
		if b.BeforeEvents != nil {
			b.mu.Unlock()
			b.BeforeEvents(q.Get("itmo"))
			b.mu.Lock()
		}
		if len(b.Responses) == 0 {
			if b.OnDrained != nil {
				b.OnDrained()
			}
			ok(w, fmt.Sprintf(`{"events":[],"nextCursor":%s,"hasMore":false,"gap":false}`, q.Get("after")))
			return
		}
		resp := b.Responses[0]
		b.Responses = b.Responses[1:]
		if resp.Status != 0 && resp.Status != 200 {
			fail(w, resp.Status, resp.Code)
			return
		}
		if resp.Page.Events == nil {
			resp.Page.Events = []backend.Event{}
		}
		data, _ := json.Marshal(resp.Page)
		ok(w, string(data))
	case p == "/api/agent/self" && r.Method == http.MethodDelete:
		if b.SelfStatus != 0 {
			fail(w, b.SelfStatus, b.SelfCode)
			return
		}
		b.SelfRevokes++
		b.Token = ""
		ok(w, `{}`)
	case p == "/api/agent/me":
		ok(w, fmt.Sprintf(`{"isu":%d,"tokenId":"t1","name":"nas","scopes":["sport"]}`, b.Isu))
	case len(parts) == 5 && parts[1] == "sport" && parts[3] == "entry" && parts[4] == "my":
		queue := strings.TrimSuffix(parts[2], "-sign")
		data := b.Entries[queue]
		if data == "" {
			data = "[]"
		}
		ok(w, data)
	case len(parts) == 6 && parts[1] == "sport" && parts[3] == "lesson":
		b.Reports = append(b.Reports, strings.TrimSuffix(parts[2], "-sign")+"/"+parts[4]+"/"+parts[5])
		ok(w, `"ok"`)
	case p == "/api/sport/sign/sync":
		var ids []int64
		_ = json.Unmarshal(body, &ids)
		b.Syncs = append(b.Syncs, ids)
		ok(w, `"ok"`)
	case p == "/api/pair/challenges" || p == "/api/agent/pair/session":
		var req map[string]string
		_ = json.Unmarshal(body, &req)
		b.Created = append(b.Created, req)
		ok(w, `{"id":"`+ChallengeID+`","code":"ABCD2345","pollSecret":"poll-secret","expiresAt":"2026-10-07T06:02:00Z"}`)
	case strings.HasPrefix(p, "/api/pair/challenges/"):
		if r.Header.Get("X-Poll-Secret") != "poll-secret" || b.Poll == nil {
			fail(w, 404, "not_found")
			return
		}
		b.polls++
		status, data := b.Poll(b.polls)
		if status != 200 {
			fail(w, status, "rate_limited")
			return
		}
		ok(w, data)
	default:
		fail(w, 403, "access_denied")
	}
}

// Calls returns a snapshot of the recorded reports and syncs.
func (b *Backend) Calls() (reports []string, syncs [][]int64, itmo []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.Reports...), append([][]int64(nil), b.Syncs...), append([]string(nil), b.ItmoParams...)
}

// ITMO serves the ITMO.ID token endpoint at /token and My ITMO at the root.
type ITMO struct {
	Srv *httptest.Server

	mu sync.Mutex
	// TokenStatus and TokenBody override the token answer; zero means a fresh token set.
	TokenStatus   int
	TokenBody     string
	Refreshes     int
	RefreshTokens []string
	Chosen        []int64
	// MyStatus forces an HTTP status on every My ITMO call, MyStatusOnce on the next one only.
	MyStatus     int
	MyStatusOnce int
	SignStatus   int
	SignBody     string
	SignIns      []int64
	MyCalls      int
}

func NewITMO(t testing.TB) *ITMO {
	f := &ITMO{}
	f.Srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Srv.Close)
	return f
}

func (f *ITMO) TokenURL() string { return f.Srv.URL + "/token" }

func (f *ITMO) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/token" {
		_ = r.ParseForm()
		f.Refreshes++
		f.RefreshTokens = append(f.RefreshTokens, r.PostForm.Get("refresh_token"))
		if r.PostForm.Get("grant_type") != "refresh_token" || r.PostForm.Get("client_id") != "student-personal-cabinet" ||
			r.PostForm.Get("scopes") != "openid profile" {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":"invalid_request"}`)
			return
		}
		if f.TokenStatus != 0 {
			w.WriteHeader(f.TokenStatus)
			fmt.Fprint(w, f.TokenBody)
			return
		}
		fmt.Fprintf(w, `{"access_token":"SECRET-ACCESS-%d","expires_in":1800,"refresh_token":"SECRET-REFRESH-%d","refresh_expires_in":2592000,"id_token":"SECRET-ID-%d"}`,
			f.Refreshes, f.Refreshes, f.Refreshes)
		return
	}
	f.MyCalls++
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer SECRET-ACCESS-") || r.Header.Get("Accept-Language") != "ru" {
		w.WriteHeader(401)
		return
	}
	if f.MyStatusOnce != 0 {
		w.WriteHeader(f.MyStatusOnce)
		f.MyStatusOnce = 0
		return
	}
	if f.MyStatus != 0 {
		w.WriteHeader(f.MyStatus)
		return
	}
	switch r.URL.Path {
	case "/api/sport/sign/chosen":
		var lessons []string
		for _, id := range f.Chosen {
			lessons = append(lessons, fmt.Sprintf(`{"id":%d,"date_start":"2026-10-07T10:00:00+03:00","date_end":"2026-10-07T11:30:00+03:00"}`, id))
		}
		fmt.Fprintf(w, `{"error_code":0,"error_message":null,"result":[{"id":31,"lesson_groups":[{"id":41,"lessons":[%s]}]}]}`,
			strings.Join(lessons, ","))
	case "/api/sport/sign/schedule/lessons":
		var ids []int64
		_ = json.NewDecoder(r.Body).Decode(&ids)
		f.SignIns = append(f.SignIns, ids...)
		if f.SignStatus != 0 {
			w.WriteHeader(f.SignStatus)
		}
		if f.SignBody != "" {
			fmt.Fprint(w, f.SignBody)
			return
		}
		data, _ := json.Marshal(ids)
		fmt.Fprintf(w, `{"error_code":0,"error_message":null,"result":%s}`, data)
	default:
		w.WriteHeader(404)
	}
}

// Snapshot returns sign-ins and refresh count.
func (f *ITMO) Snapshot() (signIns []int64, refreshes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.SignIns...), f.Refreshes
}

func (f *ITMO) Set(fn func(f *ITMO)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// Ntfy records notifications.
type Ntfy struct {
	Srv *httptest.Server

	mu       sync.Mutex
	Messages []string
}

func NewNtfy(t testing.TB) *Ntfy {
	n := &Ntfy{}
	n.Srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		n.mu.Lock()
		n.Messages = append(n.Messages, r.Header.Get("Title")+"|"+string(body))
		n.mu.Unlock()
	}))
	t.Cleanup(n.Srv.Close)
	return n
}

func (n *Ntfy) All() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.Messages...)
}

// Lesson is a SportLessonDto as Backend sends it in a push payload.
func Lesson(id int64, start, end string) string {
	return fmt.Sprintf(`{"id":%d,"sectionId":41,"sectionName":"Плавание","sectionLevel":1,"level":1,"typeId":2,"buildingId":13,"roomName":"Бассейн","start":%q,"end":%q,"timeSlotId":3,"teacherIsu":200001,"teacherFio":"Тренер Тестовый"}`,
		id, start, end)
}

// Payload wraps lessons into a SPORT_*_SIGN_LESSONS_PAYLOAD payload.
func Payload(lessons ...string) json.RawMessage {
	return json.RawMessage(`{"sportLessons":[` + strings.Join(lessons, ",") + `]}`)
}

// FreeEntry and AutoEntry are entry/my items.
func FreeEntry(lessonID int64, status string, cancelled bool) string {
	return fmt.Sprintf(`{"id":11,"lessonId":%d,"status":%q,"isCancelled":%t,"type":"free"}`, lessonID, status, cancelled)
}

func AutoEntry(realLessonID int64, status string) string {
	return fmt.Sprintf(`{"id":21,"prototypeLessonId":9101,"realLessonId":%d,"status":%q,"isCancelled":false,"type":"auto"}`, realLessonID, status)
}
