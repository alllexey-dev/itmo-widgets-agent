package agent

import (
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/alllexey-dev/itmo-widgets-agent/internal/state"
)

type SessionState int

const (
	SessionNone SessionState = iota
	SessionOK
	SessionClosed
	SessionDamaged
)

// Report is what status shows, read from the files without any network call. It holds no secret.
type Report struct {
	Dir       string
	HasToken  bool
	LastPoll  time.Time
	LastError string
	Cursor    int64
	Session   SessionState
	Isu       int64
	// Refreshed is the last ITMO.ID refresh; Expires is when the refresh token ends without one.
	Refreshed, Expires time.Time
	// Events and Sport are newest first.
	Events []state.EventRecord
	Sport  []state.SportAttempt
}

func ReadReport(files state.Files) (Report, error) {
	st, err := files.State()
	if err != nil {
		return Report{}, err
	}
	_, tokenErr := files.Token()
	r := Report{Dir: files.Dir, HasToken: tokenErr == nil, LastPoll: st.LastPollAt, LastError: st.LastError,
		Cursor: st.Cursor, Events: slices.Clone(st.Events), Sport: slices.Clone(st.Sport)}
	slices.Reverse(r.Events)
	slices.Reverse(r.Sport)
	switch s, m, ok, err := files.Session(); {
	case err != nil:
		r.Session = SessionDamaged
	case !ok:
		r.Session = SessionNone
	default:
		r.Session, r.Isu, r.Refreshed, r.Expires = SessionOK, s.Isu, s.RefreshedAt, s.RefreshExpiresAt
		if st.SessionClosedMtime != 0 && m == st.SessionClosedMtime {
			r.Session = SessionClosed
		}
	}
	return r, nil
}

// Online: the last poll succeeded within 2 minutes.
func (r Report) Online(now time.Time) bool {
	return r.LastError == "" && !r.LastPoll.IsZero() && now.Sub(r.LastPoll) <= healthyPoll
}

// EventLabel names a Backend event type for people.
func EventLabel(eventType string) string {
	switch eventType {
	case "SPORT_FREE_SIGN_LESSONS_PAYLOAD":
		return "физкультура: свободное место"
	case "SPORT_AUTO_SIGN_LESSONS_PAYLOAD":
		return "физкультура: автозапись"
	case "FRIENDSHIP_EVENT_PAYLOAD":
		return "друзья"
	}
	return eventType
}

// OutcomeLabel names a state.SportAttempt outcome for people.
func OutcomeLabel(outcome string) string {
	switch outcome {
	case "SIGNED_IN":
		return "записан"
	case "ALREADY_BOOKED":
		return "уже записан"
	case "NO_CAPACITY":
		return "мест нет, ждёт в очереди"
	case "REJECTED":
		return "ИТМО отказал, запись отменена"
	case "RETRY_LATER":
		return "не получилось, повторит сервер"
	}
	return outcome
}

// QueueLabel names a sport queue.
func QueueLabel(queue string) string {
	if queue == "auto" {
		return "автозапись"
	}
	return "своб. место"
}

// Status prints the report as plain text.
func Status(files state.Files, now time.Time, out io.Writer) error {
	r, err := ReadReport(files)
	if err != nil {
		return err
	}
	conn := "нет связи"
	if r.Online(now) {
		conn = "на связи"
	}
	session := map[SessionState]string{
		SessionNone:    "нет (выполните pair)",
		SessionClosed:  "закрыта ИТМО (выполните pair --session)",
		SessionDamaged: "файл повреждён (выполните pair --session)",
	}[r.Session]
	if r.Session == SessionOK {
		session = fmt.Sprintf("есть (ISU %d, обновлена %s", r.Isu, ago(now, r.Refreshed))
		if !r.Expires.IsZero() {
			session += ", действует до " + r.Expires.Local().Format("02.01.2006 15:04")
		}
		session += ")"
	}
	fmt.Fprintf(out, "Токен агента: %s\n", map[bool]string{true: "есть", false: "нет"}[r.HasToken])
	fmt.Fprintf(out, "Сервер: %s, последний опрос %s\n", conn, ago(now, r.LastPoll))
	fmt.Fprintf(out, "Сессия ИТМО: %s\n", session)
	fmt.Fprintf(out, "Курсор событий: %d\n", r.Cursor)
	if r.LastError != "" {
		fmt.Fprintf(out, "Последняя ошибка: %s\n", r.LastError)
	}
	if r.Dir != "" {
		fmt.Fprintf(out, "Папка: %s\n", r.Dir)
	}
	if len(r.Events) > 0 {
		fmt.Fprintln(out, "\nПоследние события:")
		for _, e := range r.Events {
			fmt.Fprintf(out, "  %s  #%d  %s\n", e.At.Local().Format("02.01 15:04"), e.Seq, EventLabel(e.Type))
		}
	}
	if len(r.Sport) > 0 {
		fmt.Fprintln(out, "\nЗапись на физкультуру:")
		for _, s := range r.Sport {
			fmt.Fprintf(out, "  %s  занятие %d (%s, %s): %s\n", s.At.Local().Format("02.01 15:04"), s.LessonID,
				QueueLabel(s.Queue), s.Start.Local().Format("02.01 15:04"), OutcomeLabel(s.Outcome))
		}
	}
	return nil
}

func ago(now, t time.Time) string {
	if t.IsZero() {
		return "никогда"
	}
	return fmt.Sprintf("%s назад", now.Sub(t).Round(time.Second))
}
