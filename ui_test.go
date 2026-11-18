package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

var noRedirects = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// uiDo sends a request the way htmx does from our own page.
func uiDo(t *testing.T, ts *httptest.Server, user, method, path string, form url.Values, headers ...string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Remote-User", user)
	req.Header.Set("HX-Request", "true")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := noRedirects.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func expectUIStatus(t *testing.T, what string, resp *http.Response, body string, want int) {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("%s: status %d, want %d; body: %s", what, resp.StatusCode, want, body)
	}
}

var pollRe = regexp.MustCompile(`hx-get="([^"]*ctag=[^"]*)"`)

func TestUIPageShowsCalDAVEvents(t *testing.T) {
	ts := newTestServer(t, t.TempDir())
	expectStatus(t, "put", do(t, ts, "alice", "PUT", "/alice/calendar/a.ics", event("a")), http.StatusCreated)

	resp, body := uiDo(t, ts, "alice", "GET", "/", nil, "Accept", "text/html")
	expectUIStatus(t, "browser root", resp, body, http.StatusSeeOther)
	if resp.Header.Get("Location") != "/~/" {
		t.Fatalf("root redirects to %q", resp.Header.Get("Location"))
	}

	for _, path := range []string{"/alice/", "/~"} {
		resp, body = uiDo(t, ts, "alice", "GET", path, nil, "Accept", "text/html")
		expectUIStatus(t, "browser "+path, resp, body, http.StatusSeeOther)
		if resp.Header.Get("Location") != "/~/" {
			t.Fatalf("%s redirects to %q", path, resp.Header.Get("Location"))
		}
	}

	resp, body = uiDo(t, ts, "alice", "GET", "/~/", nil)
	expectUIStatus(t, "page", resp, body, http.StatusOK)
	if resp.Header.Get("Content-Security-Policy") != "frame-ancestors 'none'" {
		t.Fatal("page may be framed")
	}
	if !strings.Contains(body, "&lt;a&gt; &amp; co") {
		t.Fatalf("page lacks the escaped summary:\n%s", body)
	}
	if !strings.Contains(body, `<script src="/~/htmx-4.0.1.min.js">`) {
		t.Fatalf("page lacks htmx:\n%s", body)
	}

	resp, body = uiDo(t, ts, "alice", "GET", "/~/htmx-4.0.1.min.js", nil)
	expectUIStatus(t, "htmx", resp, body, http.StatusOK)
	if !strings.Contains(body, `version="4.0.1"`) {
		t.Fatal("served htmx is not 4.0.1")
	}

	resp, body = uiDo(t, ts, "bob", "GET", "/~/", nil)
	expectUIStatus(t, "other user's page", resp, body, http.StatusOK)
	if strings.Contains(body, "&lt;a&gt;") {
		t.Fatal("bob sees alice's event")
	}
}

func TestUIAddAndDeleteEvent(t *testing.T) {
	ts := newTestServer(t, t.TempDir())
	before := ctag(t, ts, "alice", "calendar")

	resp, body := uiDo(t, ts, "alice", "POST", "/~/events", url.Values{
		"summary": {"Dentist; bring card, please"}, "date": {"2099-03-04"}, "start": {"09:30"}, "end": {"10:15"}})
	expectUIStatus(t, "add", resp, body, http.StatusOK)
	if !strings.Contains(body, "Dentist; bring card, please") || !strings.Contains(body, "<h2>Wednesday <span>Wed 4 Mar 2099</span></h2>") ||
		!strings.Contains(body, `<span class="time">09:30–10:15</span>`) {
		t.Fatalf("added event not listed:\n%s", body)
	}
	if !strings.Contains(body, `id="error"`) {
		t.Fatal("add does not clear the error box")
	}
	if ctag(t, ts, "alice", "calendar") == before {
		t.Fatal("ctag not bumped")
	}

	name := regexp.MustCompile(`name=([A-Za-z0-9]+\.ics)`).FindStringSubmatch(body)
	if name == nil {
		t.Fatalf("no delete link in:\n%s", body)
	}
	got := do(t, ts, "alice", "GET", "/alice/calendar/"+name[1], "")
	expectStatus(t, "caldav get", got, http.StatusOK)
	for _, want := range []string{"SUMMARY:Dentist\\; bring card\\, please\r\n", "DTSTART:20990304T093000\r\n", "DTEND:20990304T101500\r\n"} {
		if !strings.Contains(got.body, want) {
			t.Fatalf("stored event lacks %q:\n%s", want, got.body)
		}
	}

	resp, body = uiDo(t, ts, "alice", "POST", "/~/events", url.Values{"summary": {"Holiday"}, "date": {"2099-05-01"}})
	expectUIStatus(t, "add all-day", resp, body, http.StatusOK)
	if !strings.Contains(body, "<h2>Friday <span>Fri 1 May 2099</span></h2>") || !strings.Contains(body, `<span class="time">All day</span>`) {
		t.Fatalf("all-day event not listed:\n%s", body)
	}

	resp, body = uiDo(t, ts, "alice", "DELETE", "/~/events?name="+name[1], nil)
	expectUIStatus(t, "delete", resp, body, http.StatusOK)
	if strings.Contains(body, "Dentist") || !strings.Contains(body, "Holiday") {
		t.Fatalf("wrong list after delete:\n%s", body)
	}
	expectStatus(t, "caldav get after delete", do(t, ts, "alice", "GET", "/alice/calendar/"+name[1], ""), http.StatusNotFound)
}

func TestUIValidationErrors(t *testing.T) {
	ts := newTestServer(t, t.TempDir())
	cases := []struct {
		form url.Values
		code string
	}{
		{url.Values{"summary": {" "}, "date": {"2099-01-01"}}, "E-UI-EVENT-SUMMARY"},
		{url.Values{"summary": {"x"}, "date": {"tomorrow"}}, "E-UI-EVENT-DATE"},
		{url.Values{"summary": {"x"}, "date": {"2099-01-01"}, "start": {"25:00"}}, "E-UI-EVENT-TIME"},
		{url.Values{"summary": {"x"}, "date": {"2099-01-01"}, "start": {"10:00"}, "end": {"09:00"}}, "E-UI-EVENT-END"},
		{url.Values{"summary": {"x"}, "date": {"2099-01-01"}, "end": {"09:00"}}, "E-UI-EVENT-END"},
	}
	for _, c := range cases {
		resp, body := uiDo(t, ts, "alice", "POST", "/~/events", c.form)
		expectUIStatus(t, c.code, resp, body, http.StatusUnprocessableEntity)
		if !strings.Contains(body, c.code) || resp.Header.Get("HX-Retarget") != "#error" {
			t.Fatalf("%s: body %q, retarget %q", c.code, body, resp.Header.Get("HX-Retarget"))
		}
	}

	for _, path := range []string{"/~/events?name=../x.ics", "/~/events?name=no-suffix"} {
		resp, body := uiDo(t, ts, "alice", "DELETE", path, nil)
		expectUIStatus(t, path, resp, body, http.StatusBadRequest)
		if !strings.Contains(body, "E-UI-BAD-EVENT") {
			t.Fatalf("%s: %s", path, body)
		}
	}

	resp, body := uiDo(t, ts, "alice", "POST", "/~/events", url.Values{"summary": {"x"}, "date": {"2099-01-01"}},
		"Sec-Fetch-Site", "cross-site")
	expectUIStatus(t, "cross-site", resp, body, http.StatusForbidden)
	if !strings.Contains(body, "E-UI-CROSS-ORIGIN") {
		t.Fatalf("cross-site: %s", body)
	}
}

func TestUIPastEvents(t *testing.T) {
	ts := newTestServer(t, t.TempDir())
	for uid, props := range map[string]string{
		"old":      "DTSTART:20000101T100000",
		"new":      "DTSTART:20990101T100000",
		"birthday": "DTSTART;VALUE=DATE:19900101\r\nRRULE:FREQ=YEARLY",
	} {
		ics := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:" + uid + "\r\nSUMMARY:" + uid + " event\r\n" + props + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
		expectStatus(t, "put "+uid, do(t, ts, "alice", "PUT", "/alice/calendar/"+uid+".ics", ics), http.StatusCreated)
	}

	_, body := uiDo(t, ts, "alice", "GET", "/~/", nil)
	if strings.Contains(body, "old event") || !strings.Contains(body, "new event") || !strings.Contains(body, "1 past events hidden") ||
		!strings.Contains(body, `<div class="summary">birthday event</div><div class="detail">since 1 Jan 1990</div>`) ||
		strings.Index(body, "new event") > strings.Index(body, "birthday event") {
		t.Fatalf("default page:\n%s", body)
	}
	_, body = uiDo(t, ts, "alice", "GET", "/~/?past=1", nil)
	if !strings.Contains(body, `<h2>Saturday <span>Sat 1 Jan 2000</span></h2>`) ||
		!strings.Contains(body, `<li class="event past"><span class="time">10:00</span><div class="body"><div class="summary">old event</div>`) ||
		!strings.Contains(body, "new event") {
		t.Fatalf("past page:\n%s", body)
	}

	resp, body := uiDo(t, ts, "alice", "DELETE", "/~/events?past=1&name=new.ics", nil)
	expectUIStatus(t, "delete with past", resp, body, http.StatusOK)
	if !strings.Contains(body, "old event") || strings.Contains(body, "new event") {
		t.Fatalf("delete with past=1 lost the past view:\n%s", body)
	}
}

func TestUIUsersCannotTouchOthersEvents(t *testing.T) {
	ts := newTestServer(t, t.TempDir())
	expectStatus(t, "put", do(t, ts, "alice", "PUT", "/alice/calendar/a.ics", event("a")), http.StatusCreated)
	resp, body := uiDo(t, ts, "bob", "DELETE", "/~/events?name=a.ics", nil)
	expectUIStatus(t, "bob deletes", resp, body, http.StatusOK)
	expectStatus(t, "alice's event survives", do(t, ts, "alice", "GET", "/alice/calendar/a.ics", ""), http.StatusOK)
}

func TestUIPollOnlyRendersChanges(t *testing.T) {
	ts := newTestServer(t, t.TempDir())
	_, page := uiDo(t, ts, "alice", "GET", "/~/", nil)
	poll := pollRe.FindStringSubmatch(page)
	if poll == nil {
		t.Fatalf("no poll url in:\n%s", page)
	}
	pollURL := strings.ReplaceAll(poll[1], "&amp;", "&")

	resp, body := uiDo(t, ts, "alice", "GET", pollURL, nil)
	expectUIStatus(t, "unchanged poll", resp, body, http.StatusNoContent)

	expectStatus(t, "put", do(t, ts, "alice", "PUT", "/alice/calendar/a.ics", event("a")), http.StatusCreated)
	resp, body = uiDo(t, ts, "alice", "GET", pollURL, nil)
	expectUIStatus(t, "changed poll", resp, body, http.StatusOK)
	if !strings.Contains(body, "&lt;a&gt; &amp; co") || pollRe.FindStringSubmatch(body)[1] == poll[1] || strings.Contains(body, `id="error"`) {
		t.Fatalf("changed poll:\n%s", body)
	}
}

func TestUIGroupsEventsByDay(t *testing.T) {
	ts := newTestServer(t, t.TempDir())
	for uid, props := range map[string]string{
		"berlin-night":   "DTSTART;TZID=Europe/Berlin:20990305T001500",
		"utc-evening":    "DTSTART:20990304T233000Z",
		"berlin-morning": "DTSTART;TZID=Europe/Berlin:20990305T090000",
		"future-series":  "DTSTART;VALUE=DATE:20990601\r\nRRULE:FREQ=YEARLY",
		"old-series":     "DTSTART;VALUE=DATE:19900101\r\nRRULE:FREQ=YEARLY",
		"undated":        "",
		"tomorrow-east":  "DTSTART;TZID=Pacific/Kiritimati:" + time.Now().AddDate(0, 0, 1).Format("20060102") + "T120000",
		"tomorrow-west":  "DTSTART;TZID=Pacific/Pago_Pago:" + time.Now().AddDate(0, 0, 1).Format("20060102") + "T120000",
		"tomorrow-utc":   "DTSTART:" + time.Now().AddDate(0, 0, 1).Format("20060102") + "T120000Z",
	} {
		ics := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:" + uid + "\r\nSUMMARY:" + uid + "\r\n" + props + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
		expectStatus(t, "put "+uid, do(t, ts, "alice", "PUT", "/alice/calendar/"+uid+".ics", ics), http.StatusCreated)
	}
	_, body := uiDo(t, ts, "alice", "GET", "/~/", nil)
	order := []string{"Wednesday <span>Wed 4 Mar 2099", "utc-evening", "Thursday <span>Thu 5 Mar 2099", "berlin-night", "berlin-morning",
		"Monday <span>Mon 1 Jun 2099", "future-series", "repeats", "<h2>Repeating", "old-series", "<h2>No date", "undated"}
	at := 0
	for _, want := range order {
		i := strings.Index(body[at:], want)
		if i < 0 {
			t.Fatalf("%q missing or out of order:\n%s", want, body)
		}
		at += i
	}
	if strings.Count(body, "Thursday") != 1 || strings.Count(body, "<h2>Tomorrow") != 1 {
		t.Fatalf("day heading repeated:\n%s", body)
	}
}

func TestDayLabel(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.Local)
	for _, c := range []struct {
		start       time.Time
		label, date string
	}{
		{time.Date(2026, 11, 17, 9, 0, 0, 0, time.Local), "Today", "Tue 17 Nov"},
		{time.Date(2026, 11, 18, 0, 0, 0, 0, time.Local), "Tomorrow", "Wed 18 Nov"},
		{time.Date(2026, 11, 16, 23, 0, 0, 0, time.Local), "Yesterday", "Mon 16 Nov"},
		{time.Date(2026, 11, 20, 9, 0, 0, 0, time.Local), "Friday", "Fri 20 Nov"},
		{time.Date(2027, 1, 1, 9, 0, 0, 0, time.Local), "Friday", "Fri 1 Jan 2027"},
		{time.Time{}, "No date", ""},
		// Today is judged by the server's zone, so the same shown date gets the same label in any zone.
		{time.Date(2026, 11, 17, 23, 0, 0, 0, time.FixedZone("far", -11*3600)), "Today", "Tue 17 Nov"},
		{time.Date(2026, 11, 18, 1, 0, 0, 0, time.FixedZone("far", 14*3600)), "Tomorrow", "Wed 18 Nov"},
	} {
		if label, date, _ := dayLabel(c.start, now); label != c.label || date != c.date {
			t.Errorf("%v: %q %q, want %q %q", c.start, label, date, c.label, c.date)
		}
	}
}

func TestParseEvent(t *testing.T) {
	data := "BEGIN:VCALENDAR\r\n" +
		"BEGIN:VTIMEZONE\r\nTZID:Europe/Berlin\r\nBEGIN:STANDARD\r\nDTSTART:19701025T030000\r\nEND:STANDARD\r\nEND:VTIMEZONE\r\n" +
		"BEGIN:VEVENT\r\n" +
		"SUMMARY:Long\\, folded\r\n  line\\nwith a break\r\n" +
		"DTSTART;TZID=\"Europe/Berlin\":20990304T093000\r\n" +
		"DTEND;TZID=Europe/Berlin:20990304T101500\r\n" +
		"BEGIN:VALARM\r\nSUMMARY:alarm\r\nEND:VALARM\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"
	ev := parseEvent([]byte(data))
	if ev.summary != "Long, folded line with a break" {
		t.Fatalf("summary %q", ev.summary)
	}
	for _, c := range []struct{ ics, clock, detail string }{
		{data, "09:30–10:15", "Europe/Berlin"},
		{"BEGIN:VEVENT\nDTSTART;VALUE=DATE:20990304\nDTEND;VALUE=DATE:20990306\nEND:VEVENT\n", "All day", "until Thu 5 Mar"},
		{"BEGIN:VEVENT\nDTSTART:20990304T093000Z\nEND:VEVENT\n", "09:30", "UTC"},
		{"BEGIN:VEVENT\nDTSTART:20990304T223300\nDTEND:20990305T013300\nEND:VEVENT\n", "22:33", "until Thu 5 Mar, 01:33"},
		{"BEGIN:VEVENT\nDTSTART;VALUE=DATE:20981230\nDTEND;VALUE=DATE:20990103\nEND:VEVENT\n", "All day", "until Fri 2 Jan 2099"},
	} {
		if clock, detail := parseEvent([]byte(c.ics)).times(); clock != c.clock || detail != c.detail {
			t.Errorf("times %q %q, want %q %q", clock, detail, c.clock, c.detail)
		}
	}
	todo := parseEvent([]byte("BEGIN:VTODO\nSUMMARY:task\nDUE;VALUE=DATE:20990304\nEND:VTODO\n"))
	if todo.summary != "task" || todo.start.Year() != 2099 || !todo.allDay {
		t.Fatalf("todo %+v", todo)
	}
	openTask := parseEvent([]byte("BEGIN:VTODO\nDTSTART:20000101T000000\nDUE:20990101T000000\nEND:VTODO\n"))
	if openTask.ends().Year() != 2099 {
		t.Fatalf("task with start and due ends %v", openTask.ends())
	}
	if !parseEvent([]byte("BEGIN:VEVENT\nDTSTART:20000101T000000\nEND:VEVENT\n")).ends().Before(time.Now()) {
		t.Fatal("old event not past")
	}
}

func TestFoldLine(t *testing.T) {
	line := "SUMMARY:" + strings.Repeat("ä", 60)
	folded := foldLine(line)
	for _, l := range strings.Split(strings.TrimSuffix(folded, "\r\n"), "\r\n") {
		if len(l) > 75 {
			t.Fatalf("line of %d octets: %q", len(l), l)
		}
	}
	if parseEvent([]byte("BEGIN:VEVENT\r\n"+folded+"END:VEVENT\r\n")).summary != strings.Repeat("ä", 60) {
		t.Fatal("fold does not round-trip")
	}
}
