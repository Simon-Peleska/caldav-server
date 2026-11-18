package main

// The web UI: server-rendered pages under /~/ ('~' cannot start a user name,
// so these paths never collide with CalDAV ones), made interactive with htmx.
// The event list polls with the calendar's ctag and gets 204 (no swap) while
// nothing changed, so edits made from phones show up without websockets.

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"html"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed htmx-4.0.1.min.js
var htmxJS []byte

const htmxPath = "/~/htmx-4.0.1.min.js"

// Basic-auth credentials are sent along with cross-site requests too, so UI
// writes must come from our own pages.
var crossOrigin = http.NewCrossOriginProtection()

func (s *server) ui(w http.ResponseWriter, r *http.Request, user string) {
	// Framing would let another site trick clicks into our same-origin forms.
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
	if r.URL.Path == htmxPath {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		_, _ = w.Write(htmxJS)
		return
	}
	if err := crossOrigin.Check(r); err != nil {
		uiError(w, http.StatusForbidden, "E-UI-CROSS-ORIGIN", err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	past := r.FormValue("past") == "1"

	switch r.Method + " " + r.URL.Path {
	case "GET /~/":
		log.Printf("ui %s: page", user)
		s.uiPage(w, r, user, past)
	case "GET /~/events":
		s.uiPoll(w, r, user, past)
	case "POST /~/events":
		log.Printf("ui %s: add event", user)
		s.uiAddEvent(w, r, user, past)
	case "DELETE /~/events":
		log.Printf("ui %s: delete event %s", user, r.FormValue("name"))
		s.uiDeleteEvent(w, r, user, past)
	default:
		http.NotFound(w, r)
	}
}

// The UI shows only the default calendar; other calendars made by CalDAV
// clients are left alone.
const uiCal = "calendar"

// uiError logs the error under its code and shows both in the page's error
// box, whatever element made the request.
func uiError(w http.ResponseWriter, status int, code string, err error) {
	log.Printf("%s ui error: %v", code, err)
	w.Header().Set("HX-Retarget", "#error")
	w.Header().Set("HX-Reswap", "innerHTML")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<p><strong>%s</strong> %s</p>", code, html.EscapeString(err.Error()))
}

const clearError = `<div id="error" role="alert" hx-swap-oob="true"></div>`

// eventsURL keeps the past toggle on every request the event list makes.
func eventsURL(past bool, q url.Values) string {
	if past {
		q.Set("past", "1")
	}
	if len(q) == 0 {
		return "/~/events"
	}
	return "/~/events?" + q.Encode()
}

// ── page ─────────────────────────────────────────────────────────────────────

type pageView struct {
	User, Today string
	Past        bool
	Events      eventsView
}

func (s *server) uiPage(w http.ResponseWriter, r *http.Request, user string, past bool) {
	events, err := s.loadEvents(r.Context(), user, past)
	if err != nil {
		uiError(w, http.StatusInternalServerError, "E-UI-DB-PAGE-EVENTS", err)
		return
	}
	render(w, "page", pageView{User: user, Today: time.Now().Format("2006-01-02"), Past: past, Events: events})
}

func render(w http.ResponseWriter, name string, data any, extra ...string) {
	var b bytes.Buffer
	if err := uiTemplates.ExecuteTemplate(&b, name, data); err != nil {
		uiError(w, http.StatusInternalServerError, "E-UI-TEMPLATE", err)
		return
	}
	for _, e := range extra {
		b.WriteString(e)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b.Bytes())
}

// uiPoll re-renders the event list only when the calendar's ctag moved.
func (s *server) uiPoll(w http.ResponseWriter, r *http.Request, user string, past bool) {
	res := resource{kind: kCal, user: user, segs: []string{user, uiCal}}
	found, err := s.load(r.Context(), &res, false)
	if err != nil {
		uiError(w, http.StatusInternalServerError, "E-UI-DB-POLL", err)
		return
	}
	// A missing calendar has ctag 0 and renders as an empty list, like loadEvents.
	if r.FormValue("ctag") == strconv.FormatInt(res.ctag, 10) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	log.Printf("ui %s: calendar changed (found=%v), refreshing", user, found)
	// No clearError here: a phone syncing must not wipe an error the user is reading.
	s.renderEvents(w, r.Context(), user, past, "E-UI-DB-POLL-EVENTS")
}

func (s *server) renderEvents(w http.ResponseWriter, ctx context.Context, user string, past bool, code string, extra ...string) {
	events, err := s.loadEvents(ctx, user, past)
	if err != nil {
		uiError(w, http.StatusInternalServerError, code, err)
		return
	}
	render(w, "events", events, extra...)
}

// ── events ───────────────────────────────────────────────────────────────────

// Rows are listed by group, then start: dated events, series that already
// started (we don't expand recurrences, so their first date says nothing
// about the next one), then events without a date.
const (
	groupDated = iota
	groupRepeating
	groupUndated
)

type eventRow struct {
	group                            int
	start                            time.Time
	day                              string // start's date in the event's own zone, which is what the list shows
	Time, Detail, Summary, DeleteURL string
	Past                             bool
}

type eventDay struct {
	Label, Date string
	Today       bool
	Rows        []eventRow
}

type eventsView struct {
	URL, PollURL string
	Days         []eventDay
	Hidden       int
	Past         bool
}

// loadEvents reads the UI calendar's ctag and objects and groups them into
// days. Past events are counted but left out unless asked for. A missing
// calendar (deleted over CalDAV a moment ago) is just an empty list.
func (s *server) loadEvents(ctx context.Context, user string, past bool) (eventsView, error) {
	v := eventsView{URL: eventsURL(past, url.Values{}), Past: past}
	res := resource{kind: kCal, user: user, segs: []string{user, uiCal}}
	// ctag first: if an edit lands between the two reads, the list is newer
	// than the ctag and the next poll simply renders it again.
	found, err := s.load(ctx, &res, false)
	if err != nil {
		return v, err
	}
	v.PollURL = eventsURL(past, url.Values{"ctag": {strconv.FormatInt(res.ctag, 10)}})
	if !found {
		return v, nil
	}
	objs, err := s.children(ctx, res, true)
	if err != nil {
		return v, err
	}
	now := time.Now()
	var rows []eventRow
	for _, o := range objs {
		ev := parseEvent(o.obj.data)
		// Recurrences aren't expanded, so a series is never treated as over.
		isPast := !ev.start.IsZero() && !ev.repeats && ev.ends().Before(now)
		if isPast && !past {
			v.Hidden++
			continue
		}
		summary := ev.summary
		if summary == "" {
			summary = "(no title)"
		}
		clock, detail := ev.times()
		group := groupDated
		switch {
		case ev.start.IsZero():
			group = groupUndated
		case ev.repeats && ev.start.Before(now):
			group = groupRepeating
			detail = "since " + ev.start.Format("2 Jan 2006")
			if ev.zone != "" {
				detail += " · " + ev.zone
			}
		case ev.repeats:
			detail = strings.TrimPrefix(detail+" · repeats", " · ")
		}
		rows = append(rows, eventRow{group: group, start: ev.start, day: ev.start.Format("2006-01-02"),
			Time: clock, Detail: detail, Summary: summary, Past: isPast,
			DeleteURL: eventsURL(past, url.Values{"name": {o.segs[2]}})})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.group != b.group {
			return a.group < b.group
		}
		if a.group == groupDated && a.day != b.day {
			return a.day < b.day
		}
		return a.start.Before(b.start)
	})

	prev := eventRow{group: -1}
	for _, row := range rows {
		if row.group != prev.group || (row.group == groupDated && row.day != prev.day) {
			day := eventDay{Label: "Repeating"}
			if row.group != groupRepeating {
				day.Label, day.Date, day.Today = dayLabel(row.start, now)
			}
			v.Days = append(v.Days, day)
		}
		v.Days[len(v.Days)-1].Rows = append(v.Days[len(v.Days)-1].Rows, row)
		prev = row
	}
	return v, nil
}

// dayLabel names the date an event starts on (in the event's own zone, as
// shown), relative to today in the server's zone where that reads better.
func dayLabel(start, now time.Time) (label, date string, today bool) {
	if start.IsZero() {
		return "No date", "", false
	}
	date = start.Format("Mon 2 Jan")
	if start.Year() != now.Year() {
		date += start.Format(" 2006")
	}
	day := func(t time.Time) string { return t.Format("2006-01-02") }
	switch day(start) {
	case day(now):
		return "Today", date, true
	case day(now.AddDate(0, 0, 1)):
		return "Tomorrow", date, false
	case day(now.AddDate(0, 0, -1)):
		return "Yesterday", date, false
	}
	return start.Format("Monday"), date, false
}

func (s *server) uiAddEvent(w http.ResponseWriter, r *http.Request, user string, past bool) {
	summary := strings.TrimSpace(r.FormValue("summary"))
	if summary == "" {
		uiError(w, http.StatusUnprocessableEntity, "E-UI-EVENT-SUMMARY", errors.New("the event needs a title"))
		return
	}
	day, err := time.Parse("2006-01-02", r.FormValue("date"))
	if err != nil {
		uiError(w, http.StatusUnprocessableEntity, "E-UI-EVENT-DATE", errors.New("the date is missing or invalid"))
		return
	}
	startText, endText := r.FormValue("start"), r.FormValue("end")
	var dtstart, dtend string
	if startText == "" {
		if endText != "" {
			uiError(w, http.StatusUnprocessableEntity, "E-UI-EVENT-END", errors.New("an end time needs a start time"))
			return
		}
		dtstart = "DTSTART;VALUE=DATE:" + day.Format("20060102")
		dtend = "DTEND;VALUE=DATE:" + day.AddDate(0, 0, 1).Format("20060102")
	} else {
		start, err := time.Parse("2006-01-02 15:04", r.FormValue("date")+" "+startText)
		if err != nil {
			uiError(w, http.StatusUnprocessableEntity, "E-UI-EVENT-TIME", errors.New("the start time is invalid"))
			return
		}
		end := start.Add(time.Hour)
		if endText != "" {
			end, err = time.Parse("2006-01-02 15:04", r.FormValue("date")+" "+endText)
			if err != nil || !end.After(start) {
				uiError(w, http.StatusUnprocessableEntity, "E-UI-EVENT-END", errors.New("the end time must be after the start time"))
				return
			}
		}
		// Floating local time (no zone): the event happens at this wall-clock
		// time wherever the user is, which is what the form asked for.
		dtstart = "DTSTART:" + start.Format("20060102T150405")
		dtend = "DTEND:" + end.Format("20060102T150405")
	}
	uid := rand.Text()
	data := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//caldav-server//web ui//EN\r\nBEGIN:VEVENT\r\n" +
		"UID:" + uid + "\r\n" +
		"DTSTAMP:" + time.Now().UTC().Format("20060102T150405Z") + "\r\n" +
		dtstart + "\r\n" + dtend + "\r\n" +
		foldLine("SUMMARY:"+escapeText(summary)) +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"

	if status, code, err := s.insertEvent(r.Context(), user, uid+".ics", []byte(data)); err != nil {
		uiError(w, status, code, err)
		return
	}
	s.renderEvents(w, r.Context(), user, past, "E-UI-DB-ADD-EVENTS", clearError)
}

// insertEvent holds the write lock only for the write, not for re-rendering.
func (s *server) insertEvent(ctx context.Context, user, name string, data []byte) (int, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return http.StatusInternalServerError, "E-UI-DB-ADD-BEGIN", err
	}
	defer tx.Rollback()
	// Only missing if deleted over CalDAV since this request started.
	var ctag int64
	err = tx.QueryRowContext(ctx, `SELECT ctag FROM calendars WHERE user = ? AND name = ?`, user, uiCal).Scan(&ctag)
	if errors.Is(err, sql.ErrNoRows) {
		return http.StatusNotFound, "E-UI-CAL-MISSING", errors.New("the calendar was just deleted, reload the page")
	} else if err != nil {
		return http.StatusInternalServerError, "E-UI-DB-ADD-CAL", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO objects (user, cal, name, etag, modified, data) VALUES (?, ?, ?, ?, ?, ?)`,
		user, uiCal, name, etagOf(data), time.Now().Unix(), data); err != nil {
		return http.StatusInternalServerError, "E-UI-DB-ADD-WRITE", err
	}
	if err := bumpCtag(ctx, tx, user, uiCal); err != nil {
		return http.StatusInternalServerError, "E-UI-DB-ADD-CTAG", err
	}
	if err := tx.Commit(); err != nil {
		return http.StatusInternalServerError, "E-UI-DB-ADD-COMMIT", err
	}
	return 0, "", nil
}

func (s *server) uiDeleteEvent(w http.ResponseWriter, r *http.Request, user string, past bool) {
	name := r.FormValue("name")
	if !validName.MatchString(name) || !strings.HasSuffix(name, ".ics") {
		uiError(w, http.StatusBadRequest, "E-UI-BAD-EVENT", fmt.Errorf("invalid event name %q", name))
		return
	}
	if code, err := s.deleteEvent(r.Context(), user, name); err != nil {
		uiError(w, http.StatusInternalServerError, code, err)
		return
	}
	s.renderEvents(w, r.Context(), user, past, "E-UI-DB-DEL-EVENTS", clearError)
}

func (s *server) deleteEvent(ctx context.Context, user, name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "E-UI-DB-DEL-BEGIN", err
	}
	defer tx.Rollback()
	deleted, err := tx.ExecContext(ctx, `DELETE FROM objects WHERE user = ? AND cal = ? AND name = ?`, user, uiCal, name)
	if err != nil {
		return "E-UI-DB-DEL-WRITE", err
	}
	// Already gone (deleted elsewhere) is fine: the re-rendered list shows that.
	if n, _ := deleted.RowsAffected(); n > 0 {
		if err := bumpCtag(ctx, tx, user, uiCal); err != nil {
			return "E-UI-DB-DEL-CTAG", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "E-UI-DB-DEL-COMMIT", err
	}
	return "", nil
}

// ── iCalendar ────────────────────────────────────────────────────────────────

type eventInfo struct {
	summary    string
	start, end time.Time
	allDay     bool
	zone       string // "" for floating times, "UTC", or the TZID
	repeats    bool
}

// parseEvent reads the summary and times of the first VEVENT or VTODO. It is
// only meant for display; anything it does not understand is left empty.
func parseEvent(data []byte) eventInfo {
	unfolded := strings.NewReplacer("\r\n ", "", "\r\n\t", "", "\n ", "", "\n\t", "").Replace(string(data))
	var ev eventInfo
	var due eventInfo
	var components []string
	for _, line := range strings.Split(unfolded, "\n") {
		name, params, value, ok := splitProperty(strings.TrimRight(line, "\r"))
		if !ok {
			continue
		}
		if name == "BEGIN" {
			components = append(components, strings.ToUpper(value))
			continue
		}
		if name == "END" {
			if len(components) > 0 {
				current := components[len(components)-1]
				components = components[:len(components)-1]
				if current == "VEVENT" || current == "VTODO" {
					break
				}
			}
			continue
		}
		if len(components) == 0 || (components[len(components)-1] != "VEVENT" && components[len(components)-1] != "VTODO") {
			continue
		}
		switch name {
		case "SUMMARY":
			ev.summary = unescapeText(value)
		case "DTSTART":
			ev.start, ev.allDay, ev.zone = parseTime(value, params)
		case "DTEND":
			ev.end, _, _ = parseTime(value, params)
		case "DUE":
			due.start, due.allDay, due.zone = parseTime(value, params)
		case "RRULE", "RDATE":
			ev.repeats = true
		}
	}
	if ev.start.IsZero() && !due.start.IsZero() {
		ev.start, ev.allDay, ev.zone = due.start, due.allDay, due.zone
	} else if ev.end.IsZero() && !due.start.IsZero() {
		ev.end = due.start
	}
	return ev
}

// splitProperty splits "NAME;PARAM=x:value", ignoring colons inside quoted
// parameter values.
func splitProperty(line string) (name, params, value string, ok bool) {
	quoted := false
	for i, c := range line {
		switch {
		case c == '"':
			quoted = !quoted
		case c == ':' && !quoted:
			name, params, _ = strings.Cut(line[:i], ";")
			return strings.ToUpper(name), params, line[i+1:], true
		}
	}
	return "", "", "", false
}

func parseTime(value, params string) (t time.Time, allDay bool, zone string) {
	if len(value) == 8 {
		t, _ = time.ParseInLocation("20060102", value, time.Local)
		return t, true, ""
	}
	if strings.HasSuffix(value, "Z") {
		t, _ = time.Parse("20060102T150405Z", value)
		return t, false, "UTC"
	}
	loc := time.Local
	for _, p := range strings.Split(params, ";") {
		if k, v, _ := strings.Cut(p, "="); strings.EqualFold(k, "TZID") {
			zone = strings.Trim(v, `"`)
			if l := loadLocation(zone); l != nil {
				loc = l
			}
		}
	}
	t, _ = time.ParseInLocation("20060102T150405", value, loc)
	return t, false, zone
}

// time.LoadLocation reads the zoneinfo file on every call, and a calendar has
// thousands of events in the same few zones. Unknown zones are cached as nil.
var locations sync.Map

func loadLocation(name string) *time.Location {
	if l, ok := locations.Load(name); ok {
		return l.(*time.Location)
	}
	l, _ := time.LoadLocation(name)
	locations.Store(name, l)
	return l
}

func (ev eventInfo) ends() time.Time {
	if !ev.end.IsZero() {
		return ev.end
	}
	if ev.allDay {
		return ev.start.AddDate(0, 0, 1)
	}
	return ev.start
}

// times formats the event's time of day, and a detail line for what doesn't
// fit there: the end of a multi-day event and the zone. Times are shown as
// written in the event, since the server does not know the viewer's zone.
func (ev eventInfo) times() (clock, detail string) {
	if ev.start.IsZero() {
		return "", ""
	}
	until := func(t time.Time, layout string) string {
		if t.Year() != ev.start.Year() {
			layout = strings.Replace(layout, "Jan", "Jan 2006", 1)
		}
		return "until " + t.Format(layout)
	}
	if ev.allDay {
		if last := ev.ends().AddDate(0, 0, -1); last.After(ev.start) {
			return "All day", until(last, "Mon 2 Jan")
		}
		return "All day", ""
	}
	clock = ev.start.Format("15:04")
	if !ev.end.IsZero() {
		end := ev.end.In(ev.start.Location())
		if end.Format("20060102") == ev.start.Format("20060102") {
			clock += "–" + end.Format("15:04")
		} else {
			detail = until(end, "Mon 2 Jan, 15:04")
		}
	}
	if ev.zone != "" {
		detail = strings.TrimPrefix(detail+" · "+ev.zone, " · ")
	}
	return clock, detail
}

var (
	textEscaper   = strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\r\n", `\n`, "\n", `\n`, "\r", `\n`)
	textUnescaper = strings.NewReplacer(`\\`, `\`, `\;`, ";", `\,`, ",", `\n`, " ", `\N`, " ")
)

func escapeText(s string) string   { return textEscaper.Replace(s) }
func unescapeText(s string) string { return textUnescaper.Replace(s) }

// foldLine ends a content line with CRLF, wrapped at 75 octets as RFC 5545
// requires without splitting a UTF-8 character.
func foldLine(line string) string {
	var b strings.Builder
	width := 0
	for _, c := range line {
		n := len(string(c))
		if width+n > 75 {
			b.WriteString("\r\n ")
			width = 1
		}
		b.WriteRune(c)
		width += n
	}
	b.WriteString("\r\n")
	return b.String()
}

// ── templates ────────────────────────────────────────────────────────────────

var uiTemplates = template.Must(template.New("").Parse(`
{{define "page"}}<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Calendar</title>
<script src="` + htmxPath + `"></script>
<style>
:root {
  --bg: #f6f6f4; --card: #fff; --text: #1d1d1f; --muted: #6e6e73; --line: #e3e3e0;
  --accent: #2f6fdf; --accent-text: #fff; --accent-soft: #e8effc; --danger: #c62f2f; --danger-soft: #fbeaea;
  color-scheme: light; font: 15px/1.45 system-ui, -apple-system, "Segoe UI", sans-serif;
}
@media (prefers-color-scheme: dark) {
  :root {
    --bg: #161618; --card: #1f1f22; --text: #ececee; --muted: #9a9aa0; --line: #2f2f33;
    --accent: #6c9cff; --accent-text: #0b1530; --accent-soft: #1e2a44; --danger: #ff7b7b; --danger-soft: #3a1f1f;
    color-scheme: dark;
  }
}
* { box-sizing: border-box; }
body { margin: 0; background: var(--bg); color: var(--text); }
.wrap { max-width: 40rem; margin: 0 auto; padding: 1.5rem 1rem 4rem; }
header { display: flex; justify-content: space-between; align-items: baseline; gap: 1rem; margin-bottom: 1.25rem; }
h1 { font-size: 1.6rem; margin: 0; letter-spacing: -.01em; }
.user { color: var(--muted); font-size: .9rem; }
.card { background: var(--card); border: 1px solid var(--line); border-radius: 12px; padding: 1rem; }
form.add { display: grid; grid-template-columns: 1fr 1fr 1fr; gap: .75rem; margin-bottom: 2rem; }
form.add .title { grid-column: 1 / -1; }
label { display: grid; gap: .25rem; font-size: .8rem; font-weight: 600; color: var(--muted); }
input { font: inherit; color: inherit; background: var(--bg); border: 1px solid var(--line); border-radius: 8px; padding: .5rem .6rem; width: 100%; min-width: 0; }
input:focus { outline: 2px solid var(--accent); outline-offset: -1px; border-color: transparent; }
.actions { grid-column: 1 / -1; display: flex; justify-content: space-between; align-items: center; gap: 1rem; flex-wrap: wrap; }
.hint { color: var(--muted); font-size: .8rem; }
button.primary { font: inherit; font-weight: 600; background: var(--accent); color: var(--accent-text); border: 0; border-radius: 8px; padding: .55rem 1.1rem; cursor: pointer; }
button.primary:hover { filter: brightness(1.08); }
@media (max-width: 30rem) { form.add { grid-template-columns: 1fr 1fr; } form.add .date { grid-column: 1 / -1; } }
#error:not(:empty) { margin-bottom: 1rem; padding: .7rem 1rem; border-radius: 10px; background: var(--danger-soft); color: var(--danger); }
#error p { margin: 0; }
.day { margin-bottom: 1.5rem; }
.day h2 { display: flex; gap: .5rem; align-items: baseline; font-size: .95rem; margin: 0 0 .5rem; }
.day h2 span { color: var(--muted); font-weight: 400; }
.day.today h2 { color: var(--accent); }
.day ul { list-style: none; margin: 0; padding: 0; }
.event { display: grid; grid-template-columns: 7.5rem 1fr auto; gap: .75rem; align-items: start; background: var(--card); border: 1px solid var(--line); border-radius: 10px; padding: .6rem .5rem .6rem .9rem; margin-bottom: .4rem; }
.event.past { opacity: .55; }
.event .time { color: var(--muted); font-variant-numeric: tabular-nums; font-size: .9rem; padding-top: .05rem; }
.event .summary { font-weight: 500; overflow-wrap: anywhere; }
.event .detail { color: var(--muted); font-size: .8rem; }
.event button { font: inherit; line-height: 1; background: none; border: 0; border-radius: 6px; color: var(--muted); padding: .3rem .45rem; cursor: pointer; }
.event button:hover, .event button:focus-visible { background: var(--danger-soft); color: var(--danger); }
@media (max-width: 30rem) { .event { grid-template-columns: 1fr auto; } .event .time { grid-column: 1; } .event .body { grid-row: 2; } .event button { grid-row: 1 / 3; grid-column: 2; } }
.empty { color: var(--muted); text-align: center; padding: 2rem 1rem; }
.footer { display: flex; justify-content: space-between; gap: 1rem; color: var(--muted); font-size: .85rem; }
a { color: var(--accent); }
</style>
</head>
<body>
<div class="wrap">
<header><h1>Calendar</h1><span class="user">{{.User}}</span></header>
<div id="error" role="alert"></div>
<form class="add card" autocomplete="off" hx-post="{{.Events.URL}}" hx-target="#events" hx-swap="outerHTML"
      hx-on:htmx:after:request="event.detail.ctx.response?.status < 400 && this.reset()">
<label class="title">Title<input name="summary" required placeholder="What's happening?"></label>
<label class="date">Date<input name="date" type="date" required value="{{.Today}}"></label>
<label>Start<input name="start" type="time"></label>
<label>End<input name="end" type="time"></label>
<div class="actions"><span class="hint">Leave start empty for an all-day event.</span><button class="primary">Add event</button></div>
</form>
{{template "events" .Events}}
</div>
</body>
</html>
{{end}}

{{define "events"}}<section id="events" hx-get="{{.PollURL}}" hx-trigger="every 10s" hx-swap="outerHTML">
{{range .Days}}<div class="day{{if .Today}} today{{end}}">
<h2>{{.Label}}{{if .Date}} <span>{{.Date}}</span>{{end}}</h2>
<ul>{{range .Rows}}<li class="event{{if .Past}} past{{end}}"><span class="time">{{.Time}}</span><div class="body"><div class="summary">{{.Summary}}</div>{{if .Detail}}<div class="detail">{{.Detail}}</div>{{end}}</div><button hx-delete="{{.DeleteURL}}" hx-target="#events" hx-swap="outerHTML" hx-confirm="Delete “{{.Summary}}”?" aria-label="Delete {{.Summary}}">✕</button></li>{{end}}</ul>
</div>{{else}}<p class="empty">{{if .Past}}No events yet.{{else}}Nothing coming up.{{end}}</p>{{end}}
<div class="footer"><span>{{if .Hidden}}{{.Hidden}} past events hidden{{end}}</span><a href="/~/{{if not .Past}}?past=1{{end}}">{{if .Past}}Hide past events{{else}}Show past events{{end}}</a></div>
</section>{{end}}
`))
