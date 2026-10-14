// Package sport books queued sport lessons on a Backend event, the agent's version of the app's SportSignPushBooker.
package sport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/alllexey-dev/itmo-widgets-agent/internal/backend"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/itmo"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/notify"
	"github.com/alllexey-dev/itmo-widgets-agent/internal/state"
)

const maxLessons = 100

// Moscow has had no DST since 2014; a fixed zone works in a scratch image without tzdata.
var moscow = time.FixedZone("MSK", 3*60*60)

type Executor struct {
	Backend *backend.Client
	ITMO    *itmo.Client
	Notify  *notify.Ntfy
	Log     *slog.Logger
	Now     func() time.Time
	// Record, when set, gets every booking decision for the status history.
	Record func(state.SportAttempt)
}

type lesson struct {
	ID          int64  `json:"id"`
	SectionName string `json:"sectionName"`
	Start       string `json:"start"`
	End         string `json:"end"`
}

// Handle books the lessons of one SPORT_*_SIGN_LESSONS_PAYLOAD; queue is "free" or "auto". It returns
// itmo.ErrSessionClosed when the ITMO session ended; every other failure is logged and left to Backend's next attempt.
func (e *Executor) Handle(ctx context.Context, queue string, payload json.RawMessage) error {
	var p struct {
		SportLessons []json.RawMessage `json:"sportLessons"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		e.Log.Warn("malformed sport payload skipped")
		return nil
	}
	var (
		entries   []backend.Entry
		chosen    []itmo.ChosenLesson
		loaded    bool
		changed   bool
		seen      = map[int64]bool{}
		now       = e.Now()
		lessonsIn = p.SportLessons[:min(len(p.SportLessons), maxLessons)]
	)
	for _, raw := range lessonsIn {
		var l lesson
		if err := json.Unmarshal(raw, &l); err != nil {
			e.Log.Warn("malformed sport lesson skipped")
			continue
		}
		start, errStart := parseTime(l.Start)
		end, errEnd := parseTime(l.End)
		section := strings.TrimSpace(l.SectionName)
		if l.ID <= 0 || section == "" || errStart != nil || errEnd != nil {
			e.Log.Warn("malformed sport lesson skipped")
			continue
		}
		if seen[l.ID] || !end.After(now) {
			continue
		}
		seen[l.ID] = true
		log := e.Log.With("queue", queue, "lesson", l.ID)

		if entries == nil {
			var err error
			if entries, err = e.Backend.Entries(ctx, queue); err != nil {
				log.Warn("queue recheck failed, lesson left to the next attempt", "err", err)
				return nil
			}
		}
		if !queued(entries, queue, l.ID) {
			log.Info("no waiting queue entry for the lesson, skipped")
			continue
		}
		if !loaded {
			var err error
			if chosen, err = e.ITMO.Chosen(ctx); err != nil {
				if errors.Is(err, itmo.ErrSessionClosed) {
					return err
				}
				log.Warn("chosen lessons check failed, lesson left to the next attempt", "err", err)
				return nil
			}
			loaded = true
		}
		if slices.ContainsFunc(chosen, func(c itmo.ChosenLesson) bool { return c.ID == l.ID }) {
			log.Info("already booked, reporting satisfied")
			e.record(queue, l.ID, start, "ALREADY_BOOKED")
			e.report(ctx, log, queue, l.ID, "mark-satisfied")
			changed = true
			continue
		}
		outcome, err := e.ITMO.SignIn(ctx, l.ID)
		log.Info("sport sign-up", "outcome", outcome.String())
		e.record(queue, l.ID, start, outcome.String())
		label := fmt.Sprintf("%s, %s", section, start.In(moscow).Format("02.01 15:04"))
		switch outcome {
		case itmo.SignedIn:
			e.notify(ctx, log, "Записал на физкультуру", label)
			e.report(ctx, log, queue, l.ID, "mark-satisfied")
			chosen = append(chosen, itmo.ChosenLesson{ID: l.ID, End: end})
			changed = true
		case itmo.Rejected:
			e.notify(ctx, log, "Не удалось записаться", label+": ИТМО отказал, запись в очереди отменена")
			e.report(ctx, log, queue, l.ID, "cancel")
		case itmo.NoCapacity:
		case itmo.RetryLater:
			if errors.Is(err, itmo.ErrSessionClosed) {
				return err
			}
			log.Warn("sport sign-up deferred", "err", err)
		}
	}
	if changed {
		var future []int64
		for _, c := range chosen {
			if c.End.After(now) {
				future = append(future, c.ID)
			}
		}
		if err := e.Backend.SyncSport(ctx, future); err != nil {
			e.Log.Warn("sport bookings sync failed", "err", err)
		}
	}
	return nil
}

func queued(entries []backend.Entry, queue string, lessonID int64) bool {
	for _, en := range entries {
		if en.Type != queue || en.IsCancelled || (en.Status != "WAITING" && en.Status != "NOTIFIED") {
			continue
		}
		id := en.LessonID
		if queue == "auto" {
			id = en.RealLessonID
		}
		if id != nil && *id == lessonID {
			return true
		}
	}
	return false
}

func (e *Executor) record(queue string, id int64, start time.Time, outcome string) {
	if e.Record != nil {
		e.Record(state.SportAttempt{At: e.Now(), Queue: queue, LessonID: id, Start: start, Outcome: outcome})
	}
}

func (e *Executor) report(ctx context.Context, log *slog.Logger, queue string, id int64, action string) {
	if err := e.Backend.Report(ctx, queue, id, action); err != nil {
		log.Warn("sport report failed", "action", action, "err", err)
	}
}

func (e *Executor) notify(ctx context.Context, log *slog.Logger, title, body string) {
	if err := e.Notify.Send(ctx, title, body); err != nil {
		log.Warn("notification failed", "err", err)
	}
}

// parseTime accepts both Backend writers: Jackson ISO-8601 and Gson's OffsetDateTime without ":00" seconds.
func parseTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02T15:04Z07:00", s)
}
