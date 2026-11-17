package main

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func newTestServer(t *testing.T, dir string) *httptest.Server {
	t.Helper()
	db, err := openDB(filepath.Join(dir, "caldav.db"))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(&server{db: db})
	t.Cleanup(func() {
		ts.Close()
		db.Close()
	})
	return ts
}

type reply struct {
	status int
	etag   string
	body   string
}

func do(t *testing.T, ts *httptest.Server, user, method, path, body string, headers ...string) reply {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if user != "" {
		req.Header.Set("X-Remote-User", user)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusMultiStatus {
		var anything struct{}
		if err := xml.Unmarshal(b, &anything); err != nil {
			t.Fatalf("%s %s: malformed multistatus: %v\n%s", method, path, err, b)
		}
	}
	return reply{resp.StatusCode, resp.Header.Get("ETag"), string(b)}
}

func expectStatus(t *testing.T, what string, got reply, want int) {
	t.Helper()
	if got.status != want {
		t.Fatalf("%s: status %d, want %d; body: %s", what, got.status, want, got.body)
	}
}

func event(uid string) string {
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:" + uid + "\r\nSUMMARY:<" + uid + "> & co\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
}

var ctagRe = regexp.MustCompile(`<getctag xmlns="http://calendarserver.org/ns/">([^<]*)</getctag>`)

func ctag(t *testing.T, ts *httptest.Server, user, cal string) string {
	t.Helper()
	r := do(t, ts, user, "PROPFIND", "/"+user+"/"+cal+"/",
		`<d:propfind xmlns:d="DAV:" xmlns:cs="http://calendarserver.org/ns/"><d:prop><cs:getctag/></d:prop></d:propfind>`,
		"Depth", "0")
	expectStatus(t, "ctag propfind", r, http.StatusMultiStatus)
	m := ctagRe.FindStringSubmatch(r.body)
	if m == nil {
		t.Fatalf("no ctag in %s", r.body)
	}
	return m[1]
}

func TestEventLifecycle(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t, t.TempDir())

	home := do(t, ts, "alice", "PROPFIND", "/alice/", "", "Depth", "1")
	expectStatus(t, "propfind home", home, http.StatusMultiStatus)
	if !strings.Contains(home.body, "<d:href>/alice/calendar/</d:href>") {
		t.Fatalf("default calendar missing: %s", home.body)
	}

	ctag0 := ctag(t, ts, "alice", "calendar")
	created := do(t, ts, "alice", "PUT", "/alice/calendar/a.ics", event("a"), "If-None-Match", "*")
	expectStatus(t, "create", created, http.StatusCreated)
	if created.etag != etagOf([]byte(event("a"))) {
		t.Fatalf("etag %q", created.etag)
	}
	ctag1 := ctag(t, ts, "alice", "calendar")
	if ctag1 == ctag0 {
		t.Fatal("ctag unchanged after create")
	}

	expectStatus(t, "create again with If-None-Match",
		do(t, ts, "alice", "PUT", "/alice/calendar/a.ics", event("a"), "If-None-Match", "*"), http.StatusPreconditionFailed)
	expectStatus(t, "update with stale If-Match",
		do(t, ts, "alice", "PUT", "/alice/calendar/a.ics", event("a2"), "If-Match", `"stale"`), http.StatusPreconditionFailed)
	updated := do(t, ts, "alice", "PUT", "/alice/calendar/a.ics", event("a2"), "If-Match", created.etag)
	expectStatus(t, "update", updated, http.StatusNoContent)
	if ctag(t, ts, "alice", "calendar") == ctag1 {
		t.Fatal("ctag unchanged after update")
	}

	got := do(t, ts, "alice", "GET", "/alice/calendar/a.ics", "")
	expectStatus(t, "get", got, http.StatusOK)
	if got.body != event("a2") || got.etag != updated.etag {
		t.Fatalf("get returned %q etag %q", got.body, got.etag)
	}

	expectStatus(t, "create b", do(t, ts, "alice", "PUT", "/alice/calendar/b.ics", event("b"), ""), http.StatusCreated)

	listing := do(t, ts, "alice", "PROPFIND", "/alice/calendar/",
		`<d:propfind xmlns:d="DAV:"><d:prop><d:getetag/><d:getcontentlength/></d:prop></d:propfind>`, "Depth", "1")
	expectStatus(t, "list etags", listing, http.StatusMultiStatus)
	for _, want := range []string{"/alice/calendar/a.ics", "/alice/calendar/b.ics", esc(updated.etag),
		"<getcontentlength xmlns=\"DAV:\">" + strconv.Itoa(len(event("a2")))} {
		if !strings.Contains(listing.body, want) {
			t.Fatalf("listing lacks %q: %s", want, listing.body)
		}
	}
	if strings.Contains(listing.body, "BEGIN:VCALENDAR") {
		t.Fatal("listing without calendar-data returned data")
	}

	query := do(t, ts, "alice", "REPORT", "/alice/calendar/",
		`<c:calendar-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><d:getetag/><c:calendar-data/></d:prop></c:calendar-query>`,
		"Depth", "1")
	expectStatus(t, "calendar-query", query, http.StatusMultiStatus)
	if !strings.Contains(query.body, esc(event("a2"))) || !strings.Contains(query.body, esc(event("b"))) {
		t.Fatalf("query lacks data: %s", query.body)
	}

	multiget := do(t, ts, "alice", "REPORT", "/alice/calendar/",
		`<c:calendar-multiget xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><d:getetag/><c:calendar-data/></d:prop>`+
			`<d:href>/alice/calendar/b.ics</d:href><d:href>/alice/calendar/missing.ics</d:href><d:href>/bob/calendar/b.ics</d:href></c:calendar-multiget>`)
	expectStatus(t, "multiget", multiget, http.StatusMultiStatus)
	if !strings.Contains(multiget.body, esc(event("b"))) || strings.Contains(multiget.body, esc(event("a2"))) {
		t.Fatalf("multiget data wrong: %s", multiget.body)
	}
	if n := strings.Count(multiget.body, "404 Not Found"); n != 2 {
		t.Fatalf("multiget: %d not-found responses, want 2: %s", n, multiget.body)
	}

	ctag2 := ctag(t, ts, "alice", "calendar")
	expectStatus(t, "delete with stale If-Match",
		do(t, ts, "alice", "DELETE", "/alice/calendar/a.ics", "", "If-Match", created.etag), http.StatusPreconditionFailed)
	expectStatus(t, "delete", do(t, ts, "alice", "DELETE", "/alice/calendar/a.ics", "", "If-Match", updated.etag), http.StatusNoContent)
	expectStatus(t, "get deleted", do(t, ts, "alice", "GET", "/alice/calendar/a.ics", ""), http.StatusNotFound)
	expectStatus(t, "delete again", do(t, ts, "alice", "DELETE", "/alice/calendar/a.ics", ""), http.StatusNotFound)
	if ctag(t, ts, "alice", "calendar") == ctag2 {
		t.Fatal("ctag unchanged after delete")
	}
}

func TestCalendars(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t, t.TempDir())

	expectStatus(t, "put into missing calendar",
		do(t, ts, "alice", "PUT", "/alice/work/x.ics", event("x"), ""), http.StatusConflict)
	expectStatus(t, "mkcalendar", do(t, ts, "alice", "MKCALENDAR", "/alice/work/", ""), http.StatusCreated)
	expectStatus(t, "mkcalendar again", do(t, ts, "alice", "MKCALENDAR", "/alice/work/", ""), http.StatusMethodNotAllowed)
	expectStatus(t, "put", do(t, ts, "alice", "PUT", "/alice/work/x.ics", event("x"), ""), http.StatusCreated)

	expectStatus(t, "delete calendar", do(t, ts, "alice", "DELETE", "/alice/work/", ""), http.StatusNoContent)
	expectStatus(t, "propfind deleted calendar", do(t, ts, "alice", "PROPFIND", "/alice/work/", "", "Depth", "0"), http.StatusNotFound)
	expectStatus(t, "recreate", do(t, ts, "alice", "MKCALENDAR", "/alice/work/", ""), http.StatusCreated)
	expectStatus(t, "object gone with its calendar", do(t, ts, "alice", "GET", "/alice/work/x.ics", ""), http.StatusNotFound)

	expectStatus(t, "delete default calendar", do(t, ts, "alice", "DELETE", "/alice/calendar/", ""), http.StatusNoContent)
	expectStatus(t, "default calendar back on next contact",
		do(t, ts, "alice", "PUT", "/alice/calendar/y.ics", event("y"), ""), http.StatusCreated)
}

func TestUsersAreIsolated(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t, t.TempDir())

	expectStatus(t, "no user", do(t, ts, "", "PROPFIND", "/", ""), http.StatusUnauthorized)
	expectStatus(t, "alice put", do(t, ts, "alice", "PUT", "/alice/calendar/a.ics", event("a"), ""), http.StatusCreated)
	expectStatus(t, "bob reads alice", do(t, ts, "bob", "GET", "/alice/calendar/a.ics", ""), http.StatusForbidden)
	expectStatus(t, "bob writes alice", do(t, ts, "bob", "PUT", "/alice/calendar/a.ics", event("b"), ""), http.StatusForbidden)
	expectStatus(t, "bob same name", do(t, ts, "bob", "GET", "/bob/calendar/a.ics", ""), http.StatusNotFound)
}

func TestPersistsAcrossRestart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ts := newTestServer(t, dir)
	expectStatus(t, "put kept", do(t, ts, "alice", "PUT", "/alice/calendar/kept.ics", event("kept"), ""), http.StatusCreated)
	expectStatus(t, "put gone", do(t, ts, "alice", "PUT", "/alice/calendar/gone.ics", event("gone"), ""), http.StatusCreated)
	expectStatus(t, "delete gone", do(t, ts, "alice", "DELETE", "/alice/calendar/gone.ics", ""), http.StatusNoContent)
	expectStatus(t, "mkcalendar", do(t, ts, "alice", "MKCALENDAR", "/alice/work/", ""), http.StatusCreated)
	ts.Close()
	ts.Config.Handler.(*server).db.Close()

	reopened := newTestServer(t, dir)
	got := do(t, reopened, "alice", "GET", "/alice/calendar/kept.ics", "")
	expectStatus(t, "kept persisted", got, http.StatusOK)
	if got.body != event("kept") || got.etag != etagOf([]byte(event("kept"))) {
		t.Fatalf("got %q etag %q", got.body, got.etag)
	}
	expectStatus(t, "deleted stays deleted", do(t, reopened, "alice", "GET", "/alice/calendar/gone.ics", ""), http.StatusNotFound)
	expectStatus(t, "calendar persisted", do(t, reopened, "alice", "PROPFIND", "/alice/work/", "", "Depth", "0"), http.StatusMultiStatus)
}

func TestProperties(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t, t.TempDir())
	expectStatus(t, "put", do(t, ts, "alice", "PUT", "/alice/calendar/a.ics", event("a"), ""), http.StatusCreated)

	mixed := do(t, ts, "alice", "PROPFIND", "/alice/calendar/a.ics",
		`<d:propfind xmlns:d="DAV:" xmlns:x="urn:x&quot;y"><d:prop><d:getetag/><x:nope/><d:getcontenttype/></d:prop></d:propfind>`, "Depth", "0")
	expectStatus(t, "mixed propfind", mixed, http.StatusMultiStatus)
	for _, want := range []string{
		`<d:propstat><d:prop><getetag xmlns="DAV:">` + esc(etagOf([]byte(event("a")))) + `</getetag><getcontenttype xmlns="DAV:">text/calendar; charset=utf-8</getcontenttype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat>`,
		`<d:propstat><d:prop><nope xmlns="urn:x&#34;y"/></d:prop><d:status>HTTP/1.1 404 Not Found</d:status></d:propstat>`,
	} {
		if !strings.Contains(mixed.body, want) {
			t.Fatalf("mixed propfind lacks %s:\n%s", want, mixed.body)
		}
	}

	allMissing := do(t, ts, "alice", "PROPFIND", "/alice/calendar/a.ics",
		`<d:propfind xmlns:d="DAV:"><d:prop><d:nope/></d:prop></d:propfind>`, "Depth", "0")
	expectStatus(t, "all-missing propfind", allMissing, http.StatusMultiStatus)
	if strings.Contains(allMissing.body, "200 OK") {
		t.Fatalf("empty 200 propstat: %s", allMissing.body)
	}

	expectStatus(t, "proppatch refused", do(t, ts, "alice", "PROPPATCH", "/alice/calendar/",
		`<d:propertyupdate xmlns:d="DAV:"><d:set><d:prop><d:displayname>x</d:displayname></d:prop></d:set></d:propertyupdate>`), http.StatusMultiStatus)
	expectStatus(t, "If-Match * on missing object",
		do(t, ts, "alice", "PUT", "/alice/calendar/missing.ics", event("m"), "If-Match", "*"), http.StatusPreconditionFailed)
}

func TestMultigetOrderAndMissing(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t, t.TempDir())
	const n = 5
	var hrefs strings.Builder
	for i := range n {
		expectStatus(t, "put", do(t, ts, "alice", "PUT", fmt.Sprintf("/alice/calendar/%d.ics", i), event(fmt.Sprint(i)), ""), http.StatusCreated)
		fmt.Fprintf(&hrefs, "<d:href>/alice/calendar/%d.ics</d:href>", n-1-i)
	}
	hrefs.WriteString("<d:href>/alice/calendar/missing.ics</d:href>")
	r := do(t, ts, "alice", "REPORT", "/alice/calendar/",
		`<c:calendar-multiget xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><d:getetag/></d:prop>`+hrefs.String()+`</c:calendar-multiget>`)
	expectStatus(t, "multiget", r, http.StatusMultiStatus)
	if got := strings.Count(r.body, "200 OK"); got != n {
		t.Fatalf("%d found, want %d", got, n)
	}
	if !strings.Contains(r.body, "<d:href>/alice/calendar/missing.ics</d:href><d:status>HTTP/1.1 404 Not Found") {
		t.Fatal("missing href not reported")
	}
	if strings.Index(r.body, "/alice/calendar/4.ics") > strings.Index(r.body, "/alice/calendar/0.ics") {
		t.Fatal("responses not in request order")
	}
}

func TestConcurrentIfMatchUpdatesOnlyOneWins(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t, t.TempDir())
	created := do(t, ts, "alice", "PUT", "/alice/calendar/a.ics", event("a"), "")
	expectStatus(t, "create", created, http.StatusCreated)

	var wg sync.WaitGroup
	statuses := make([]int, 8)
	for i := range statuses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i] = do(t, ts, "alice", "PUT", "/alice/calendar/a.ics", event(fmt.Sprint("v", i)), "If-Match", created.etag).status
		}()
	}
	wg.Wait()
	won := 0
	for _, s := range statuses {
		switch s {
		case http.StatusNoContent:
			won++
		case http.StatusPreconditionFailed:
		default:
			t.Fatalf("unexpected status %d", s)
		}
	}
	if won != 1 {
		t.Fatalf("%d updates won, want exactly 1", won)
	}
}
