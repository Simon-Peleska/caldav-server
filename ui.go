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
	cal := r.FormValue("cal")
	if cal == "" {
		cal = "calendar"
	}
	if !validName.MatchString(cal) {
		uiError(w, http.StatusBadRequest, "E-UI-BAD-CAL", fmt.Errorf("invalid calendar name %q", cal))
		return
	}
	past := r.FormValue("past") == "1"

	switch r.Method + " " + r.URL.Path {
	case "GET /~/":
		log.Printf("ui %s: page %s", user, cal)
		s.uiPage(w, r, user, cal, past)
	case "GET /~/events":
		s.uiPoll(w, r, user, cal, past)
	case "POST /~/events":
		log.Printf("ui %s: add event to %s", user, cal)
		s.uiAddEvent(w, r, user, cal, past)
	case "DELETE /~/events":
		log.Printf("ui %s: delete event %s/%s", user, cal, r.FormValue("name"))
		s.uiDeleteEvent(w, r, user, cal, past)
	case "POST /~/calendars":
		log.Printf("ui %s: add calendar %s", user, r.FormValue("name"))
		s.uiAddCalendar(w, r, user)
	case "DELETE /~/calendars":
		log.Printf("ui %s: delete calendar %s", user, cal)
		s.uiDeleteCalendar(w, r, user, cal)
	default:
		http.NotFound(w, r)
	}
}

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

func eventsURL(cal string, past bool) string {
	q := url.Values{"cal": {cal}}
	if past {
		q.Set("past", "1")
	}
	return "/~/events?" + q.Encode()
}

func pageURL(cal string, past bool) string {
	q := url.Values{"cal": {cal}}
	if past {
		q.Set("past", "1")
	}
	return "/~/?" + q.Encode()
}

// ── pages ────────────────────────────────────────────────────────────────────

type calendarLink struct {
	Name, URL string
	Current   bool
}

type pageView struct {
	User, Cal, Today       string
	Calendars              []calendarLink
	Missing, Past          bool
	TogglePastURL, CalsURL string
	Events                 eventsView
}

func (s *server) uiPage(w http.ResponseWriter, r *http.Request, user, cal string, past bool) {
	cals, err := s.children(r.Context(), resource{kind: kHome, user: user}, false)
	if err != nil {
		uiError(w, http.StatusInternalServerError, "E-UI-DB-PAGE-CALS", err)
		return
	}
	v := pageView{User: user, Cal: cal, Past: past, Today: time.Now().Format("2006-01-02"),
		TogglePastURL: pageURL(cal, !past), CalsURL: "/~/calendars?" + url.Values{"cal": {cal}}.Encode()}
	for _, c := range cals {
		v.Calendars = append(v.Calendars, calendarLink{Name: c.segs[1], URL: pageURL(c.segs[1], false), Current: c.segs[1] == cal})
	}
	events, found, err := s.loadEvents(r.Context(), user, cal, past)
	if err != nil {
		uiError(w, http.StatusInternalServerError, "E-UI-DB-PAGE-EVENTS", err)
		return
	}
	v.Missing = !found
	v.Events = events
	render(w, "page", v)
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
func (s *server) uiPoll(w http.ResponseWriter, r *http.Request, user, cal string, past bool) {
	res := resource{kind: kCal, user: user, segs: []string{user, cal}}
	found, err := s.load(r.Context(), &res, false)
	if err != nil {
		uiError(w, http.StatusInternalServerError, "E-UI-DB-POLL", err)
		return
	}
	if !found {
		w.Header().Set("HX-Refresh", "true")
		return
	}
	if r.FormValue("ctag") == strconv.FormatInt(res.ctag, 10) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	log.Printf("ui %s: %s changed, refreshing", user, cal)
	// No clearError here: a phone syncing must not wipe an error the user is reading.
	s.renderEvents(w, r.Context(), user, cal, past, "E-UI-DB-POLL-EVENTS")
}

func (s *server) renderEvents(w http.ResponseWriter, ctx context.Context, user, cal string, past bool, code string, extra ...string) {
	events, found, err := s.loadEvents(ctx, user, cal, past)
	if err != nil {
		uiError(w, http.StatusInternalServerError, code, err)
		return
	}
	if !found {
		w.Header().Set("HX-Refresh", "true")
		return
	}
	render(w, "events", events, extra...)
}

// ── events ───────────────────────────────────────────────────────────────────

type eventRow struct {
	start                    time.Time
	When, Summary, DeleteURL string
	Past                     bool
}

type eventsView struct {
	URL, PollURL string
	Rows         []eventRow
	Hidden       int
	Past         bool
}

// loadEvents reads a calendar's ctag and objects and turns them into rows
// sorted by start time. Past events are counted but left out unless asked for.
func (s *server) loadEvents(ctx context.Context, user, cal string, past bool) (eventsView, bool, error) {
	res := resource{kind: kCal, user: user, segs: []string{user, cal}}
	// ctag first: if an edit lands between the two reads, the list is newer
	// than the ctag and the next poll simply renders it again.
	found, err := s.load(ctx, &res, false)
	if err != nil || !found {
		return eventsView{}, found, err
	}
	objs, err := s.children(ctx, res, true)
	if err != nil {
		return eventsView{}, true, err
	}
	v := eventsView{URL: eventsURL(cal, past), Past: past}
	v.PollURL = v.URL + "&ctag=" + strconv.FormatInt(res.ctag, 10)
	now := time.Now()
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
		when := ev.when()
		if ev.repeats {
			when += ", repeats"
		}
		v.Rows = append(v.Rows, eventRow{start: ev.start, When: when, Summary: summary, Past: isPast,
			DeleteURL: v.URL + "&" + url.Values{"name": {o.segs[2]}}.Encode()})
	}
	sort.SliceStable(v.Rows, func(i, j int) bool {
		a, b := v.Rows[i].start, v.Rows[j].start
		if a.IsZero() || b.IsZero() {
			return !a.IsZero() && b.IsZero()
		}
		return a.Before(b)
	})
	return v, true, nil
}

func (s *server) uiAddEvent(w http.ResponseWriter, r *http.Request, user, cal string, past bool) {
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

	if status, code, err := s.insertEvent(r.Context(), user, cal, uid+".ics", []byte(data)); err != nil {
		uiError(w, status, code, err)
		return
	}
	s.renderEvents(w, r.Context(), user, cal, past, "E-UI-DB-ADD-EVENTS", clearError)
}

// insertEvent holds the write lock only for the write, not for re-rendering.
func (s *server) insertEvent(ctx context.Context, user, cal, name string, data []byte) (int, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return http.StatusInternalServerError, "E-UI-DB-ADD-BEGIN", err
	}
	defer tx.Rollback()
	var ctag int64
	err = tx.QueryRowContext(ctx, `SELECT ctag FROM calendars WHERE user = ? AND name = ?`, user, cal).Scan(&ctag)
	if errors.Is(err, sql.ErrNoRows) {
		return http.StatusNotFound, "E-UI-CAL-MISSING", fmt.Errorf("calendar %s does not exist", cal)
	} else if err != nil {
		return http.StatusInternalServerError, "E-UI-DB-ADD-CAL", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO objects (user, cal, name, etag, modified, data) VALUES (?, ?, ?, ?, ?, ?)`,
		user, cal, name, etagOf(data), time.Now().Unix(), data); err != nil {
		return http.StatusInternalServerError, "E-UI-DB-ADD-WRITE", err
	}
	if err := bumpCtag(ctx, tx, user, cal); err != nil {
		return http.StatusInternalServerError, "E-UI-DB-ADD-CTAG", err
	}
	if err := tx.Commit(); err != nil {
		return http.StatusInternalServerError, "E-UI-DB-ADD-COMMIT", err
	}
	return 0, "", nil
}

func (s *server) uiDeleteEvent(w http.ResponseWriter, r *http.Request, user, cal string, past bool) {
	name := r.FormValue("name")
	if !validName.MatchString(name) || !strings.HasSuffix(name, ".ics") {
		uiError(w, http.StatusBadRequest, "E-UI-BAD-EVENT", fmt.Errorf("invalid event name %q", name))
		return
	}
	if code, err := s.deleteEvent(r.Context(), user, cal, name); err != nil {
		uiError(w, http.StatusInternalServerError, code, err)
		return
	}
	s.renderEvents(w, r.Context(), user, cal, past, "E-UI-DB-DEL-EVENTS", clearError)
}

func (s *server) deleteEvent(ctx context.Context, user, cal, name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "E-UI-DB-DEL-BEGIN", err
	}
	defer tx.Rollback()
	deleted, err := tx.ExecContext(ctx, `DELETE FROM objects WHERE user = ? AND cal = ? AND name = ?`, user, cal, name)
	if err != nil {
		return "E-UI-DB-DEL-WRITE", err
	}
	// Already gone (deleted elsewhere) is fine: the re-rendered list shows that.
	if n, _ := deleted.RowsAffected(); n > 0 {
		if err := bumpCtag(ctx, tx, user, cal); err != nil {
			return "E-UI-DB-DEL-CTAG", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "E-UI-DB-DEL-COMMIT", err
	}
	return "", nil
}

// ── calendars ────────────────────────────────────────────────────────────────

func (s *server) uiAddCalendar(w http.ResponseWriter, r *http.Request, user string) {
	name := strings.TrimSpace(r.FormValue("name"))
	if !validName.MatchString(name) {
		uiError(w, http.StatusUnprocessableEntity, "E-UI-CAL-NAME", errors.New("calendar names may only use letters, digits and . _ @ + -"))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	added, err := s.db.ExecContext(r.Context(), `INSERT INTO calendars (user, name, ctag) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`,
		user, name, time.Now().UnixNano())
	if err != nil {
		uiError(w, http.StatusInternalServerError, "E-UI-DB-MKCAL", err)
		return
	}
	if n, _ := added.RowsAffected(); n == 0 {
		uiError(w, http.StatusUnprocessableEntity, "E-UI-CAL-EXISTS", fmt.Errorf("calendar %s already exists", name))
		return
	}
	w.Header().Set("HX-Redirect", pageURL(name, false))
}

func (s *server) uiDeleteCalendar(w http.ResponseWriter, r *http.Request, user, cal string) {
	ctx := r.Context()
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		uiError(w, http.StatusInternalServerError, "E-UI-DB-RMCAL-BEGIN", err)
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM objects WHERE user = ? AND cal = ?`, user, cal); err != nil {
		uiError(w, http.StatusInternalServerError, "E-UI-DB-RMCAL-OBJECTS", err)
		return
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM calendars WHERE user = ? AND name = ?`, user, cal); err != nil {
		uiError(w, http.StatusInternalServerError, "E-UI-DB-RMCAL-WRITE", err)
		return
	}
	if err := tx.Commit(); err != nil {
		uiError(w, http.StatusInternalServerError, "E-UI-DB-RMCAL-COMMIT", err)
		return
	}
	if cal == "calendar" {
		s.homes.Delete(user)
	}
	w.Header().Set("HX-Redirect", "/~/")
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

// when formats the event's time as written in the event, labelled with its
// zone, since the server does not know the viewer's time zone.
func (ev eventInfo) when() string {
	if ev.start.IsZero() {
		return ""
	}
	if ev.allDay {
		s := ev.start.Format("Mon 2 Jan 2006")
		if last := ev.ends().AddDate(0, 0, -1); last.After(ev.start) {
			s += " – " + last.Format("Mon 2 Jan 2006")
		}
		return s
	}
	s := ev.start.Format("Mon 2 Jan 2006, 15:04")
	if !ev.end.IsZero() {
		end := ev.end.In(ev.start.Location())
		if end.Format("20060102") == ev.start.Format("20060102") {
			s += "–" + end.Format("15:04")
		} else {
			s += " – " + end.Format("Mon 2 Jan, 15:04")
		}
	}
	if ev.zone != "" {
		s += " " + ev.zone
	}
	return s
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
<title>{{.Cal}} · caldav</title>
<script src="` + htmxPath + `"></script>
<style>
:root { color-scheme: light dark; font-family: system-ui, sans-serif; line-height: 1.4; }
body { max-width: 56rem; margin: 0 auto; padding: 1rem; }
header { display: flex; justify-content: space-between; align-items: baseline; border-bottom: 1px solid #8884; }
header h1 { font-size: 1.2rem; margin: .5rem 0; }
.layout { display: grid; grid-template-columns: 12rem 1fr; gap: 2rem; }
@media (max-width: 40rem) { .layout { grid-template-columns: 1fr; } }
ul { list-style: none; padding: 0; }
nav li { padding: .2rem 0; }
nav a[aria-current] { font-weight: bold; }
form { display: flex; flex-wrap: wrap; gap: .5rem; margin: 1rem 0; }
form input[name=name], form input[name=summary] { flex: 1; min-width: 8rem; }
#events li { display: flex; gap: 1rem; align-items: baseline; padding: .4rem 0; border-bottom: 1px solid #8882; }
#events time { flex: 0 0 15rem; font-variant-numeric: tabular-nums; opacity: .75; }
#events span { flex: 1; overflow-wrap: anywhere; }
#events li.past { opacity: .55; }
.muted { opacity: .7; }
#error:not(:empty) { margin: 1rem 0; padding: .5rem 1rem; border: 1px solid #d33; border-radius: 4px; color: #d33; }
#error p { margin: 0; }
</style>
</head>
<body>
<header><h1>caldav</h1><span>{{.User}}</span></header>
<div id="error" role="alert"></div>
<div class="layout">
<nav>
<h2>Calendars</h2>
<ul>{{range .Calendars}}<li><a href="{{.URL}}"{{if .Current}} aria-current="page"{{end}}>{{.Name}}</a></li>{{end}}</ul>
<form hx-post="/~/calendars">
<input name="name" placeholder="New calendar" required aria-label="New calendar name">
<button>Add</button>
</form>
</nav>
<main>
{{if .Missing}}<p>Calendar <strong>{{.Cal}}</strong> does not exist.</p>{{else}}
<h2>{{.Cal}}</h2>
<form hx-post="{{.Events.URL}}" hx-target="#events" hx-swap="outerHTML"
      hx-on:htmx:after:request="event.detail.ctx.response?.status < 400 && this.reset()">
<input name="summary" placeholder="Title" required aria-label="Title">
<input name="date" type="date" required value="{{.Today}}" aria-label="Date">
<input name="start" type="time" aria-label="Start time">
<input name="end" type="time" aria-label="End time">
<button>Add event</button>
</form>
{{template "events" .Events}}
<p><a href="{{.TogglePastURL}}">{{if .Past}}Hide past events{{else}}Show past events{{end}}</a></p>
{{if ne .Cal "calendar"}}<p><button hx-delete="{{.CalsURL}}" hx-confirm="Delete calendar {{.Cal}} and all its events?">Delete calendar</button></p>{{end}}
{{end}}
</main>
</div>
</body>
</html>
{{end}}

{{define "events"}}<section id="events" hx-get="{{.PollURL}}" hx-trigger="every 10s" hx-swap="outerHTML">
{{if .Rows}}<ul>{{range .Rows}}<li{{if .Past}} class="past"{{end}}><time>{{.When}}</time><span>{{.Summary}}</span><button hx-delete="{{.DeleteURL}}" hx-target="#events" hx-swap="outerHTML" hx-confirm="Delete this event?" title="Delete event">✕</button></li>{{end}}</ul>
{{else}}<p class="muted">{{if .Past}}No events.{{else}}No upcoming events.{{end}}</p>{{end}}
{{if .Hidden}}<p class="muted">{{.Hidden}} past events hidden.</p>{{end}}
</section>{{end}}
`))
