package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// BenchmarkHotPaths measures what sync clients do all day against a
// calendar with 2000 events: poll etags, fetch everything, fetch a batch,
// fetch one.
func BenchmarkHotPaths(b *testing.B) {
	db, err := openDB(filepath.Join(b.TempDir(), "caldav.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	s := &server{db: db}
	call := func(method, path, body string, headers ...string) int {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("X-Remote-User", "alice")
		for i := 0; i+1 < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		return w.Code
	}
	const events = 2000
	for i := range events {
		ics := strings.Replace(event(fmt.Sprint(i)), "UID:", fmt.Sprintf("DTSTART;TZID=Europe/Berlin:2%03d0101T100000\r\nUID:", i%1000), 1)
		if code := call("PUT", fmt.Sprintf("/alice/calendar/%d.ics", i), ics+strings.Repeat("X", 600)); code != http.StatusCreated {
			b.Fatalf("put: %d", code)
		}
	}
	var hrefs strings.Builder
	for i := range 100 {
		fmt.Fprintf(&hrefs, "<d:href>/alice/calendar/%d.ics</d:href>", i*7)
	}

	b.Run("propfind-etags", func(b *testing.B) {
		for b.Loop() {
			if code := call("PROPFIND", "/alice/calendar/", `<d:propfind xmlns:d="DAV:"><d:prop><d:getetag/></d:prop></d:propfind>`, "Depth", "1"); code != http.StatusMultiStatus {
				b.Fatalf("status %d", code)
			}
		}
	})
	b.Run("query-all-data", func(b *testing.B) {
		for b.Loop() {
			if code := call("REPORT", "/alice/calendar/", `<c:calendar-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><d:getetag/><c:calendar-data/></d:prop></c:calendar-query>`); code != http.StatusMultiStatus {
				b.Fatalf("status %d", code)
			}
		}
	})
	b.Run("multiget-100", func(b *testing.B) {
		for b.Loop() {
			if code := call("REPORT", "/alice/calendar/", `<c:calendar-multiget xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><d:getetag/><c:calendar-data/></d:prop>`+hrefs.String()+`</c:calendar-multiget>`); code != http.StatusMultiStatus {
				b.Fatalf("status %d", code)
			}
		}
	})
	b.Run("ui-page", func(b *testing.B) {
		for b.Loop() {
			if code := call("GET", "/~/", ""); code != http.StatusOK {
				b.Fatalf("status %d", code)
			}
		}
	})
	b.Run("ui-poll-unchanged", func(b *testing.B) {
		var ctag int64
		if err := db.QueryRow(`SELECT ctag FROM calendars WHERE user = 'alice' AND name = 'calendar'`).Scan(&ctag); err != nil {
			b.Fatal(err)
		}
		for b.Loop() {
			if code := call("GET", fmt.Sprintf("/~/events?ctag=%d", ctag), ""); code != http.StatusNoContent {
				b.Fatalf("status %d", code)
			}
		}
	})
	b.Run("get", func(b *testing.B) {
		i := 0
		for b.Loop() {
			i++
			if code := call("GET", fmt.Sprintf("/alice/calendar/%d.ics", i%events), ""); code != http.StatusOK {
				b.Fatalf("get: %d", code)
			}
		}
	})
}
