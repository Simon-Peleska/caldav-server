// A very small CalDAV server: calendars and their events/todos live in a local
// Turso database (DATA_DIR/caldav.db).
//
// URL space:
//
//	/<user>/<calendar>/<object>.ics
//
// Authentication is not done here: put it behind a reverse proxy that does
// basic auth and passes the name in X-Remote-User, and bind to loopback.
// Every user only ever sees their own tree. A calendar called "calendar" is
// created on first contact.
//
// Not implemented (clients cope without): sync-collection, time-range
// filtering in calendar-query (all objects are returned), scheduling/ACLs,
// and changing properties (PROPPATCH is refused).
package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "turso.tech/database/tursogo"
)

const (
	nsD  = "DAV:"
	nsC  = "urn:ietf:params:xml:ns:caldav"
	nsCS = "http://calendarserver.org/ns/"

	maxBody = 1 << 20
)

var validName = regexp.MustCompile(`^[A-Za-z0-9_@+-][A-Za-z0-9._@+-]*$`)

const schema = `
CREATE TABLE IF NOT EXISTS calendars (
	user TEXT NOT NULL,
	name TEXT NOT NULL,
	ctag INTEGER NOT NULL,
	PRIMARY KEY (user, name)
);
CREATE TABLE IF NOT EXISTS objects (
	user     TEXT NOT NULL,
	cal      TEXT NOT NULL,
	name     TEXT NOT NULL,
	etag     TEXT NOT NULL,
	modified INTEGER NOT NULL,
	data     BLOB NOT NULL,
	PRIMARY KEY (user, cal, name)
);`

type server struct {
	db    *sql.DB
	mu    sync.Mutex // serialises writes so If-Match checks are not racy
	homes sync.Map   // users whose default calendar is known to exist

	vapid       *ecdsa.PrivateKey // signs Web Push requests
	vapidPublic string            // base64url, what browsers subscribe with
	pushSubject string            // PUSH_SUBJECT; overrides the per-device origin
	push        *http.Client
	pushHosts   []string // push services a device may subscribe with; see pushServices
}

func main() {
	addr := env("ADDR", "127.0.0.1:5232")
	dir := env("DATA_DIR", "./data")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		log.Fatal(err)
	}
	db, err := openDB(filepath.Join(dir, "caldav.db"))
	if err != nil {
		log.Fatalf("E-DB-OPEN opening database: %v", err)
	}
	s, err := newServer(db)
	if err != nil {
		log.Fatalf("E-PUSH-KEY loading the push key: %v", err)
	}
	go s.pushLoop()
	srv := &http.Server{
		Addr:              addr,
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	log.Printf("caldav listening on %s, data in %s", addr, dir)
	log.Fatal(srv.ListenAndServe())
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("turso", path)
	if err != nil {
		return nil, err
	}
	// Every pooled connection is a separately opened database handle, so keep
	// them around instead of letting database/sql close and reopen them.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	if _, err := db.Exec(schema + pushSchema); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// storageError logs a database failure under a stable code and reports the
// same code to the client.
func storageError(w http.ResponseWriter, code string, err error) {
	log.Printf("%s storage error: %v", code, err)
	http.Error(w, code+" storage error", http.StatusInternalServerError)
}

// resource kinds, by path depth
const (
	kRoot = iota // /
	kHome        // /<user>/        (principal and calendar home in one)
	kCal         // /<user>/<cal>/
	kObj         // /<user>/<cal>/<obj>.ics
)

type resource struct {
	kind int
	user string
	segs []string
	ctag int64  // kCal, filled in by load/children
	obj  object // kObj, filled in by load/children
}

type object struct {
	etag     string
	modified int64 // unix seconds
	size     int64
	data     []byte // only loaded when calendar-data is asked for
}

func (r resource) href() string {
	p := "/" + strings.Join(r.segs, "/")
	if r.kind != kObj && p != "/" {
		p += "/"
	}
	return (&url.URL{Path: p}).EscapedPath()
}

func resolve(user string, segs []string) (resource, bool) {
	if len(segs) > 3 {
		return resource{}, false
	}
	for _, g := range segs {
		if !validName.MatchString(g) {
			return resource{}, false
		}
	}
	if len(segs) == 3 && !strings.HasSuffix(segs[2], ".ics") {
		return resource{}, false
	}
	return resource{kind: len(segs), user: user, segs: segs}, true
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user := r.Header.Get("X-Remote-User")
	if !validName.MatchString(user) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	isUI := strings.HasPrefix(r.URL.Path, "/~/")
	if err := s.ensureDefaultCalendar(r.Context(), user); err != nil {
		if strings.HasPrefix(r.URL.Path, "/~/api/") {
			apiError(w, http.StatusInternalServerError, "E-DB-HOME", err)
		} else if isUI {
			uiError(w, http.StatusInternalServerError, "E-DB-HOME", err)
		} else {
			storageError(w, "E-DB-HOME", err)
		}
		return
	}
	if isUI {
		s.ui(w, r, user)
		return
	}
	if (r.URL.Path == "/" || r.URL.Path == "/~" || r.URL.Path == "/"+user+"/") && r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html") {
		http.Redirect(w, r, "/~/", http.StatusSeeOther)
		return
	}
	segs := strings.FieldsFunc(r.URL.Path, func(c rune) bool { return c == '/' })
	if len(segs) > 0 && segs[0] != user {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	res, ok := resolve(user, segs)
	if !ok {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodOptions:
		w.Header().Set("DAV", "1, calendar-access")
		w.Header().Set("Allow", "OPTIONS, GET, HEAD, PUT, DELETE, PROPFIND, PROPPATCH, REPORT, MKCALENDAR, MKCOL")
		w.WriteHeader(http.StatusOK)
	case "PROPFIND":
		s.propfind(w, r, res)
	case "PROPPATCH":
		s.proppatch(w, r, res)
	case "REPORT":
		s.report(w, r, res)
	case http.MethodGet, http.MethodHead:
		s.get(w, r, res)
	case http.MethodPut:
		s.put(w, r, res)
	case http.MethodDelete:
		s.del(w, r, res)
	case "MKCALENDAR", "MKCOL":
		s.mkcal(w, r, res)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ── storage ──────────────────────────────────────────────────────────────────

// ensureDefaultCalendar runs on every request, so it remembers users whose
// default calendar exists; del forgets them when that calendar is deleted.
func (s *server) ensureDefaultCalendar(ctx context.Context, user string) error {
	if _, ok := s.homes.Load(user); ok {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO calendars (user, name, ctag) VALUES (?, 'calendar', ?) ON CONFLICT DO NOTHING`,
		user, time.Now().UnixNano())
	if err == nil {
		s.homes.Store(user, struct{}{})
	}
	return err
}

// load fills in a calendar's ctag or an object's metadata (and data, if
// asked for) and reports whether the resource exists.
func (s *server) load(ctx context.Context, res *resource, withData bool) (bool, error) {
	var err error
	switch res.kind {
	case kRoot, kHome:
		return true, nil
	case kCal:
		err = s.db.QueryRowContext(ctx, `SELECT ctag FROM calendars WHERE user = ? AND name = ?`,
			res.user, res.segs[1]).Scan(&res.ctag)
	case kObj:
		err = s.db.QueryRowContext(ctx, `SELECT etag, modified, length(data), `+dataColumn(withData)+` FROM objects WHERE user = ? AND cal = ? AND name = ?`,
			res.user, res.segs[1], res.segs[2]).Scan(&res.obj.etag, &res.obj.modified, &res.obj.size, &res.obj.data)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func dataColumn(withData bool) string {
	if withData {
		return "data"
	}
	return "NULL"
}

func (s *server) children(ctx context.Context, res resource, withData bool) ([]resource, error) {
	switch res.kind {
	case kRoot:
		return []resource{{kind: kHome, user: res.user, segs: []string{res.user}}}, nil
	case kHome:
		rows, err := s.db.QueryContext(ctx, `SELECT name, ctag FROM calendars WHERE user = ? ORDER BY name`, res.user)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []resource
		for rows.Next() {
			c := resource{kind: kCal, user: res.user}
			var name string
			if err := rows.Scan(&name, &c.ctag); err != nil {
				return nil, err
			}
			c.segs = []string{res.user, name}
			out = append(out, c)
		}
		return out, rows.Err()
	case kCal:
		rows, err := s.db.QueryContext(ctx, `SELECT name, etag, modified, length(data), `+dataColumn(withData)+` FROM objects WHERE user = ? AND cal = ? ORDER BY name`,
			res.user, res.segs[1])
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []resource
		for rows.Next() {
			o := resource{kind: kObj, user: res.user}
			var name string
			if err := rows.Scan(&name, &o.obj.etag, &o.obj.modified, &o.obj.size, &o.obj.data); err != nil {
				return nil, err
			}
			o.segs = []string{res.user, res.segs[1], name}
			out = append(out, o)
		}
		return out, rows.Err()
	}
	return nil, nil
}

// bumpCtag marks a calendar as changed. The ctag is a nanosecond timestamp so
// a deleted and recreated calendar does not reuse an old value.
func bumpCtag(ctx context.Context, tx *sql.Tx, user, cal string) error {
	_, err := tx.ExecContext(ctx, `UPDATE calendars SET ctag = max(ctag + 1, ?) WHERE user = ? AND name = ?`,
		time.Now().UnixNano(), user, cal)
	return err
}

// ── properties ───────────────────────────────────────────────────────────────

func etagOf(data []byte) string {
	h := sha256.Sum256(data)
	return `"` + hex.EncodeToString(h[:8]) + `"`
}

func esc(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func principal(user string) string { return "<d:href>/" + esc(user) + "/</d:href>" }

// prop writes the value of one property of a loaded resource into b and
// reports whether the resource has it; it writes nothing when it does not.
func (res resource) prop(b *bytes.Buffer, n xml.Name) bool {
	switch n {
	case xml.Name{Space: nsD, Local: "resourcetype"}:
		switch res.kind {
		case kRoot:
			b.WriteString("<d:collection/>")
		case kHome:
			b.WriteString("<d:collection/><d:principal/>")
		case kCal:
			b.WriteString("<d:collection/><c:calendar/>")
		}
		return true
	case xml.Name{Space: nsD, Local: "displayname"}:
		switch res.kind {
		case kHome:
			b.WriteString(esc(res.user))
			return true
		case kCal, kObj:
			b.WriteString(esc(res.segs[len(res.segs)-1]))
			return true
		}
	case xml.Name{Space: nsD, Local: "current-user-principal"},
		xml.Name{Space: nsD, Local: "principal-URL"},
		xml.Name{Space: nsD, Local: "owner"},
		xml.Name{Space: nsC, Local: "calendar-home-set"}:
		b.WriteString(principal(res.user))
		return true
	case xml.Name{Space: nsD, Local: "supported-report-set"}:
		if res.kind == kCal {
			b.WriteString(`<d:supported-report><d:report><c:calendar-multiget/></d:report></d:supported-report>` +
				`<d:supported-report><d:report><c:calendar-query/></d:report></d:supported-report>`)
			return true
		}
	case xml.Name{Space: nsC, Local: "supported-calendar-component-set"}:
		if res.kind == kCal {
			b.WriteString(`<c:comp name="VEVENT"/><c:comp name="VTODO"/><c:comp name="VJOURNAL"/>`)
			return true
		}
	case xml.Name{Space: nsCS, Local: "getctag"}:
		if res.kind == kCal {
			b.WriteString(strconv.FormatInt(res.ctag, 10))
			return true
		}
	case xml.Name{Space: nsD, Local: "getetag"}:
		if res.kind == kObj {
			b.WriteString(esc(res.obj.etag))
			return true
		}
	case xml.Name{Space: nsD, Local: "getcontenttype"}:
		if res.kind == kObj {
			b.WriteString("text/calendar; charset=utf-8")
			return true
		}
	case xml.Name{Space: nsD, Local: "getcontentlength"}:
		if res.kind == kObj {
			b.WriteString(strconv.FormatInt(res.obj.size, 10))
			return true
		}
	case xml.Name{Space: nsD, Local: "getlastmodified"}:
		if res.kind == kObj {
			b.WriteString(time.Unix(res.obj.modified, 0).UTC().Format(http.TimeFormat))
			return true
		}
	case xml.Name{Space: nsC, Local: "calendar-data"}:
		if res.kind == kObj {
			_ = xml.EscapeText(b, res.obj.data)
			return true
		}
	}
	return false
}

var calendarData = xml.Name{Space: nsC, Local: "calendar-data"}

func wantsData(names []xml.Name) bool {
	for _, n := range names {
		if n == calendarData {
			return true
		}
	}
	return false
}

var defaultProps = []xml.Name{
	{Space: nsD, Local: "resourcetype"}, {Space: nsD, Local: "displayname"},
	{Space: nsD, Local: "getetag"}, {Space: nsD, Local: "getcontenttype"},
	{Space: nsD, Local: "getlastmodified"}, {Space: nsCS, Local: "getctag"},
	{Space: nsC, Local: "supported-calendar-component-set"},
	{Space: nsD, Local: "current-user-principal"},
}

type propList struct {
	Items []struct {
		XMLName xml.Name
	} `xml:",any"`
}

func (p propList) names() []xml.Name {
	out := make([]xml.Name, len(p.Items))
	for i, it := range p.Items {
		out[i] = it.XMLName
	}
	return out
}

// writeResponse writes one <d:response>. Each property is written in place
// and cut off again if the resource lacks it, so values (calendar data
// especially) are copied only once.
func writeResponse(b *bytes.Buffer, res resource, names []xml.Name) {
	b.WriteString("<d:response><d:href>" + res.href() + "</d:href>")
	propstat := b.Len()
	b.WriteString("<d:propstat><d:prop>")
	var missing []xml.Name
	for _, n := range names {
		mark := b.Len()
		b.WriteString("<" + n.Local + ` xmlns="` + esc(n.Space) + `">`)
		if res.prop(b, n) {
			b.WriteString("</" + n.Local + ">")
		} else {
			b.Truncate(mark)
			missing = append(missing, n)
		}
	}
	if len(missing) == len(names) {
		b.Truncate(propstat)
	} else {
		b.WriteString("</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat>")
	}
	if len(missing) > 0 {
		b.WriteString("<d:propstat><d:prop>")
		for _, n := range missing {
			b.WriteString("<" + n.Local + ` xmlns="` + esc(n.Space) + `"/>`)
		}
		b.WriteString("</d:prop><d:status>HTTP/1.1 404 Not Found</d:status></d:propstat>")
	}
	b.WriteString("</d:response>")
}

func writeMultistatus(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	_, _ = io.WriteString(w, `<?xml version="1.0" encoding="utf-8"?>`+
		`<d:multistatus xmlns:d="DAV:" xmlns:c="`+nsC+`" xmlns:cs="`+nsCS+`">`)
	_, _ = w.Write(body)
	_, _ = io.WriteString(w, `</d:multistatus>`)
}

func readXML(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	return xml.Unmarshal(body, v)
}

// ── methods ──────────────────────────────────────────────────────────────────

func (s *server) propfind(w http.ResponseWriter, r *http.Request, res resource) {
	var req struct {
		Prop    propList  `xml:"prop"`
		AllProp *struct{} `xml:"allprop"`
	}
	if err := readXML(r, &req); err != nil {
		http.Error(w, "bad xml", http.StatusBadRequest)
		return
	}
	names := req.Prop.names()
	if len(names) == 0 {
		names = defaultProps
	}
	withData := wantsData(names)
	found, err := s.load(r.Context(), &res, withData)
	if err != nil {
		storageError(w, "E-DB-PROPFIND-LOAD", err)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	var b bytes.Buffer
	writeResponse(&b, res, names)
	if r.Header.Get("Depth") != "0" {
		children, err := s.children(r.Context(), res, withData)
		if err != nil {
			storageError(w, "E-DB-PROPFIND-LIST", err)
			return
		}
		for _, c := range children {
			writeResponse(&b, c, names)
		}
	}
	writeMultistatus(w, b.Bytes())
}

// Properties are fixed (names come from the URL), so refuse changes instead
// of pretending they were stored.
func (s *server) proppatch(w http.ResponseWriter, r *http.Request, res resource) {
	var req struct {
		Set []struct {
			Prop propList `xml:"prop"`
		} `xml:"set"`
		Remove []struct {
			Prop propList `xml:"prop"`
		} `xml:"remove"`
	}
	if err := readXML(r, &req); err != nil {
		http.Error(w, "bad xml", http.StatusBadRequest)
		return
	}
	var props strings.Builder
	for _, g := range append(req.Set, req.Remove...) {
		for _, n := range g.Prop.names() {
			props.WriteString("<" + n.Local + ` xmlns="` + esc(n.Space) + `"/>`)
		}
	}
	writeMultistatus(w, []byte(`<d:response><d:href>`+res.href()+`</d:href><d:propstat><d:prop>`+props.String()+
		`</d:prop><d:status>HTTP/1.1 403 Forbidden</d:status></d:propstat></d:response>`))
}

// report answers calendar-multiget and calendar-query. Filters in a query are
// ignored: every object in the calendar is returned and the client filters.
func (s *server) report(w http.ResponseWriter, r *http.Request, res resource) {
	var req struct {
		XMLName xml.Name
		Prop    propList `xml:"prop"`
		Hrefs   []string `xml:"href"`
	}
	if err := readXML(r, &req); err != nil {
		http.Error(w, "bad xml", http.StatusBadRequest)
		return
	}
	names := req.Prop.names()
	if len(names) == 0 {
		names = []xml.Name{{Space: nsD, Local: "getetag"}}
	}
	withData := wantsData(names)
	var b bytes.Buffer
	switch req.XMLName {
	case xml.Name{Space: nsC, Local: "calendar-multiget"}:
		// One point lookup per href: Turso does not use the primary key for
		// "name IN (...)" and would scan the whole calendar instead.
		for _, h := range req.Hrefs {
			u, err := url.Parse(h)
			if err != nil {
				continue
			}
			segs := strings.FieldsFunc(u.Path, func(c rune) bool { return c == '/' })
			o, ok := resolve(res.user, segs)
			found := false
			if ok && o.kind == kObj && segs[0] == res.user {
				if found, err = s.load(r.Context(), &o, withData); err != nil {
					storageError(w, "E-DB-MULTIGET", err)
					return
				}
			}
			if !found {
				b.WriteString("<d:response><d:href>" + esc(h) + "</d:href><d:status>HTTP/1.1 404 Not Found</d:status></d:response>")
				continue
			}
			writeResponse(&b, o, names)
		}
	case xml.Name{Space: nsC, Local: "calendar-query"}:
		if res.kind != kCal {
			http.NotFound(w, r)
			return
		}
		found, err := s.load(r.Context(), &res, false)
		if err != nil {
			storageError(w, "E-DB-QUERY-LOAD", err)
			return
		}
		if !found {
			http.NotFound(w, r)
			return
		}
		children, err := s.children(r.Context(), res, withData)
		if err != nil {
			storageError(w, "E-DB-QUERY-LIST", err)
			return
		}
		for _, o := range children {
			writeResponse(&b, o, names)
		}
	default:
		http.Error(w, "unsupported report", http.StatusForbidden)
		return
	}
	writeMultistatus(w, b.Bytes())
}

func (s *server) get(w http.ResponseWriter, r *http.Request, res resource) {
	if res.kind != kObj {
		http.Error(w, "not a calendar object", http.StatusMethodNotAllowed)
		return
	}
	found, err := s.load(r.Context(), &res, true)
	if err != nil {
		storageError(w, "E-DB-GET", err)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("ETag", res.obj.etag)
	_, _ = w.Write(res.obj.data)
}

// preconditionFailed checks If-Match / If-None-Match against the current
// object, given whether it exists and its etag.
func preconditionFailed(r *http.Request, exists bool, etag string) bool {
	if m := r.Header.Get("If-Match"); m != "" && m != "*" && (!exists || m != etag) {
		return true
	} else if m == "*" && !exists {
		return true
	}
	if r.Header.Get("If-None-Match") == "*" && exists {
		return true
	}
	return false
}

func (s *server) put(w http.ResponseWriter, r *http.Request, res resource) {
	if res.kind != kObj {
		http.Error(w, "not a calendar object", http.StatusMethodNotAllowed)
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(data) > maxBody {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	if !strings.HasPrefix(strings.TrimSpace(string(data)), "BEGIN:VCALENDAR") {
		http.Error(w, "not an iCalendar object", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	user, cal, name := res.user, res.segs[1], res.segs[2]
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		storageError(w, "E-DB-PUT-BEGIN", err)
		return
	}
	defer tx.Rollback()

	var ctag int64
	err = tx.QueryRowContext(ctx, `SELECT ctag FROM calendars WHERE user = ? AND name = ?`, user, cal).Scan(&ctag)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "calendar does not exist", http.StatusConflict)
		return
	} else if err != nil {
		storageError(w, "E-DB-PUT-CAL", err)
		return
	}
	var current string
	err = tx.QueryRowContext(ctx, `SELECT etag FROM objects WHERE user = ? AND cal = ? AND name = ?`, user, cal, name).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		storageError(w, "E-DB-PUT-OBJ", err)
		return
	}
	exists := err == nil
	if preconditionFailed(r, exists, current) {
		w.WriteHeader(http.StatusPreconditionFailed)
		return
	}

	etag := etagOf(data)
	if _, err := tx.ExecContext(ctx, `INSERT INTO objects (user, cal, name, etag, modified, data) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (user, cal, name) DO UPDATE SET etag = excluded.etag, modified = excluded.modified, data = excluded.data`,
		user, cal, name, etag, time.Now().Unix(), data); err != nil {
		storageError(w, "E-DB-PUT-WRITE", err)
		return
	}
	if err := bumpCtag(ctx, tx, user, cal); err != nil {
		storageError(w, "E-DB-PUT-CTAG", err)
		return
	}
	if err := tx.Commit(); err != nil {
		storageError(w, "E-DB-PUT-COMMIT", err)
		return
	}
	w.Header().Set("ETag", etag)
	if exists {
		w.WriteHeader(http.StatusNoContent)
	} else {
		w.WriteHeader(http.StatusCreated)
	}
}

func (s *server) del(w http.ResponseWriter, r *http.Request, res resource) {
	if res.kind != kObj && res.kind != kCal {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	ctx := r.Context()
	s.mu.Lock()
	defer s.mu.Unlock()
	found, err := s.load(ctx, &res, false)
	if err != nil {
		storageError(w, "E-DB-DEL-LOAD", err)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	if res.kind == kObj && preconditionFailed(r, true, res.obj.etag) {
		w.WriteHeader(http.StatusPreconditionFailed)
		return
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		storageError(w, "E-DB-DEL-BEGIN", err)
		return
	}
	defer tx.Rollback()
	user, cal := res.user, res.segs[1]
	if res.kind == kObj {
		_, err = tx.ExecContext(ctx, `DELETE FROM objects WHERE user = ? AND cal = ? AND name = ?`, user, cal, res.segs[2])
		if err == nil {
			err = bumpCtag(ctx, tx, user, cal)
		}
	} else {
		_, err = tx.ExecContext(ctx, `DELETE FROM objects WHERE user = ? AND cal = ?`, user, cal)
		if err == nil {
			_, err = tx.ExecContext(ctx, `DELETE FROM calendars WHERE user = ? AND name = ?`, user, cal)
		}
	}
	if err != nil {
		storageError(w, "E-DB-DEL-WRITE", err)
		return
	}
	if err := tx.Commit(); err != nil {
		storageError(w, "E-DB-DEL-COMMIT", err)
		return
	}
	if res.kind == kCal && cal == "calendar" {
		s.homes.Delete(user)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) mkcal(w http.ResponseWriter, r *http.Request, res resource) {
	if res.kind != kCal {
		http.Error(w, "calendars live at /<user>/<name>/", http.StatusForbidden)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	found, err := s.load(r.Context(), &res, false)
	if err != nil {
		storageError(w, "E-DB-MKCAL-LOAD", err)
		return
	}
	if found {
		http.Error(w, "exists", http.StatusMethodNotAllowed)
		return
	}
	if _, err := s.db.ExecContext(r.Context(), `INSERT INTO calendars (user, name, ctag) VALUES (?, ?, ?)`,
		res.user, res.segs[1], time.Now().UnixNano()); err != nil {
		storageError(w, "E-DB-MKCAL-WRITE", err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}
