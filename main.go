// A very small CalDAV server: one .ics file per event/todo on disk, no
// database, standard library only.
//
// Layout on disk (and in the URL space):
//
//	DATA_DIR/<user>/<calendar>/<object>.ics   ->  /<user>/<calendar>/<object>.ics
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
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	nsD  = "DAV:"
	nsC  = "urn:ietf:params:xml:ns:caldav"
	nsCS = "http://calendarserver.org/ns/"

	maxBody = 1 << 20
)

var validName = regexp.MustCompile(`^[A-Za-z0-9_@+-][A-Za-z0-9._@+-]*$`)

type server struct {
	dir string
	mu  sync.Mutex // serialises writes so If-Match checks are not racy
}

func main() {
	addr := env("ADDR", "127.0.0.1:5232")
	dir := env("DATA_DIR", "./data")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           &server{dir: dir},
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
	fs   string
}

func (r resource) href() string {
	p := "/" + strings.Join(r.segs, "/")
	if r.kind != kObj && p != "/" {
		p += "/"
	}
	return (&url.URL{Path: p}).EscapedPath()
}

func (s *server) resolve(user string, segs []string) (resource, bool) {
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
	return resource{
		kind: len(segs), user: user, segs: segs,
		fs: filepath.Join(append([]string{s.dir}, segs...)...),
	}, true
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user := r.Header.Get("X-Remote-User")
	if !validName.MatchString(user) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	segs := strings.FieldsFunc(r.URL.Path, func(c rune) bool { return c == '/' })
	if len(segs) > 0 && segs[0] != user {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	res, ok := s.resolve(user, segs)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := os.MkdirAll(filepath.Join(s.dir, user, "calendar"), 0o750); err != nil {
		log.Print(err)
		http.Error(w, "storage error", http.StatusInternalServerError)
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
		s.mkcal(w, res)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
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

// prop returns the XML for one property of a resource, or false if the
// resource does not have it.
func (res resource) prop(n xml.Name) (string, bool) {
	var data []byte
	var info os.FileInfo
	if res.kind == kObj {
		var err error
		if data, err = os.ReadFile(res.fs); err != nil {
			return "", false
		}
		info, _ = os.Stat(res.fs)
	}
	switch n {
	case xml.Name{Space: nsD, Local: "resourcetype"}:
		switch res.kind {
		case kRoot:
			return "<d:collection/>", true
		case kHome:
			return "<d:collection/><d:principal/>", true
		case kCal:
			return "<d:collection/><c:calendar/>", true
		}
		return "", true
	case xml.Name{Space: nsD, Local: "displayname"}:
		switch res.kind {
		case kHome:
			return esc(res.user), true
		case kCal, kObj:
			return esc(res.segs[len(res.segs)-1]), true
		}
	case xml.Name{Space: nsD, Local: "current-user-principal"},
		xml.Name{Space: nsD, Local: "principal-URL"},
		xml.Name{Space: nsD, Local: "owner"}:
		return principal(res.user), true
	case xml.Name{Space: nsC, Local: "calendar-home-set"}:
		return principal(res.user), true
	case xml.Name{Space: nsD, Local: "supported-report-set"}:
		if res.kind == kCal {
			return `<d:supported-report><d:report><c:calendar-multiget/></d:report></d:supported-report>` +
				`<d:supported-report><d:report><c:calendar-query/></d:report></d:supported-report>`, true
		}
	case xml.Name{Space: nsC, Local: "supported-calendar-component-set"}:
		if res.kind == kCal {
			return `<c:comp name="VEVENT"/><c:comp name="VTODO"/><c:comp name="VJOURNAL"/>`, true
		}
	case xml.Name{Space: nsCS, Local: "getctag"}:
		if res.kind == kCal {
			return esc(res.ctag()), true
		}
	case xml.Name{Space: nsD, Local: "getetag"}:
		if res.kind == kObj {
			return esc(etagOf(data)), true
		}
	case xml.Name{Space: nsD, Local: "getcontenttype"}:
		if res.kind == kObj {
			return "text/calendar; charset=utf-8", true
		}
	case xml.Name{Space: nsD, Local: "getcontentlength"}:
		if res.kind == kObj {
			return fmt.Sprint(len(data)), true
		}
	case xml.Name{Space: nsD, Local: "getlastmodified"}:
		if res.kind == kObj {
			return info.ModTime().UTC().Format(http.TimeFormat), true
		}
	case xml.Name{Space: nsC, Local: "calendar-data"}:
		if res.kind == kObj {
			return esc(string(data)), true
		}
	}
	return "", false
}

// ctag changes whenever anything in the calendar is added, changed or removed.
func (res resource) ctag() string {
	h := sha256.New()
	entries, _ := os.ReadDir(res.fs)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && strings.HasSuffix(e.Name(), ".ics") {
			fmt.Fprintln(h, e.Name(), info.Size(), info.ModTime().UnixNano())
		}
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

func (res resource) children() []resource {
	var out []resource
	switch res.kind {
	case kRoot:
		out = append(out, resource{kind: kHome, user: res.user, segs: []string{res.user}, fs: filepath.Join(res.fs, res.user)})
	case kHome, kCal:
		entries, _ := os.ReadDir(res.fs)
		for _, e := range entries {
			if (res.kind == kHome && e.IsDir()) || (res.kind == kCal && !e.IsDir() && strings.HasSuffix(e.Name(), ".ics")) {
				out = append(out, resource{
					kind: res.kind + 1, user: res.user,
					segs: append(append([]string{}, res.segs...), e.Name()),
					fs:   filepath.Join(res.fs, e.Name()),
				})
			}
		}
	}
	return out
}

func (res resource) exists() bool {
	switch res.kind {
	case kRoot, kHome:
		return true
	}
	info, err := os.Stat(res.fs)
	return err == nil && info.IsDir() == (res.kind == kCal)
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

func writeResponse(b *strings.Builder, res resource, names []xml.Name) {
	var ok, missing strings.Builder
	for _, n := range names {
		if v, found := res.prop(n); found {
			fmt.Fprintf(&ok, "<%s xmlns=%q>%s</%s>", n.Local, n.Space, v, n.Local)
		} else {
			fmt.Fprintf(&missing, "<%s xmlns=%q/>", n.Local, n.Space)
		}
	}
	fmt.Fprintf(b, "<d:response><d:href>%s</d:href>", res.href())
	if ok.Len() > 0 {
		fmt.Fprintf(b, "<d:propstat><d:prop>%s</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat>", ok.String())
	}
	if missing.Len() > 0 {
		fmt.Fprintf(b, "<d:propstat><d:prop>%s</d:prop><d:status>HTTP/1.1 404 Not Found</d:status></d:propstat>", missing.String())
	}
	b.WriteString("</d:response>")
}

func writeMultistatus(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?>`+
		`<d:multistatus xmlns:d="DAV:" xmlns:c="%s" xmlns:cs="%s">%s</d:multistatus>`, nsC, nsCS, body)
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
	if !res.exists() {
		http.NotFound(w, r)
		return
	}
	names := req.Prop.names()
	if len(names) == 0 {
		names = defaultProps
	}
	var b strings.Builder
	writeResponse(&b, res, names)
	if r.Header.Get("Depth") != "0" {
		for _, c := range res.children() {
			writeResponse(&b, c, names)
		}
	}
	writeMultistatus(w, b.String())
}

// Properties are fixed (names are the directory names), so refuse changes
// instead of pretending they were stored.
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
			fmt.Fprintf(&props, "<%s xmlns=%q/>", n.Local, n.Space)
		}
	}
	writeMultistatus(w, fmt.Sprintf(`<d:response><d:href>%s</d:href><d:propstat><d:prop>%s</d:prop>`+
		`<d:status>HTTP/1.1 403 Forbidden</d:status></d:propstat></d:response>`, res.href(), props.String()))
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
	var b strings.Builder
	switch req.XMLName {
	case xml.Name{Space: nsC, Local: "calendar-multiget"}:
		for _, h := range req.Hrefs {
			u, err := url.Parse(h)
			if err != nil {
				continue
			}
			segs := strings.FieldsFunc(u.Path, func(c rune) bool { return c == '/' })
			o, ok := s.resolve(res.user, segs)
			if !ok || o.kind != kObj || segs[0] != res.user || !o.exists() {
				fmt.Fprintf(&b, "<d:response><d:href>%s</d:href><d:status>HTTP/1.1 404 Not Found</d:status></d:response>", esc(h))
				continue
			}
			writeResponse(&b, o, names)
		}
	case xml.Name{Space: nsC, Local: "calendar-query"}:
		if res.kind != kCal || !res.exists() {
			http.NotFound(w, r)
			return
		}
		for _, o := range res.children() {
			writeResponse(&b, o, names)
		}
	default:
		http.Error(w, "unsupported report", http.StatusForbidden)
		return
	}
	writeMultistatus(w, b.String())
}

func (s *server) get(w http.ResponseWriter, r *http.Request, res resource) {
	if res.kind != kObj {
		http.Error(w, "not a calendar object", http.StatusMethodNotAllowed)
		return
	}
	data, err := os.ReadFile(res.fs)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("ETag", etagOf(data))
	_, _ = w.Write(data)
}

// preconditionFailed checks If-Match / If-None-Match against the current file.
func preconditionFailed(r *http.Request, res resource) bool {
	data, err := os.ReadFile(res.fs)
	exists := err == nil
	if m := r.Header.Get("If-Match"); m != "" && m != "*" && (!exists || m != etagOf(data)) {
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
	s.mu.Lock()
	defer s.mu.Unlock()
	calDir := filepath.Dir(res.fs)
	if info, err := os.Stat(calDir); err != nil || !info.IsDir() {
		http.Error(w, "calendar does not exist", http.StatusConflict)
		return
	}
	if preconditionFailed(r, res) {
		w.WriteHeader(http.StatusPreconditionFailed)
		return
	}
	_, statErr := os.Stat(res.fs)
	created := statErr != nil

	tmp, err := os.CreateTemp(calDir, ".tmp-*")
	if err == nil {
		_, err = tmp.Write(data)
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Rename(tmp.Name(), res.fs)
		}
		if err != nil {
			os.Remove(tmp.Name())
		}
	}
	if err != nil {
		log.Print(err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("ETag", etagOf(data))
	if created {
		w.WriteHeader(http.StatusCreated)
	} else {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *server) del(w http.ResponseWriter, r *http.Request, res resource) {
	if res.kind != kObj && res.kind != kCal {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !res.exists() {
		http.NotFound(w, r)
		return
	}
	if res.kind == kObj && preconditionFailed(r, res) {
		w.WriteHeader(http.StatusPreconditionFailed)
		return
	}
	if err := os.RemoveAll(res.fs); err != nil {
		log.Print(err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) mkcal(w http.ResponseWriter, res resource) {
	if res.kind != kCal {
		http.Error(w, "calendars live at /<user>/<name>/", http.StatusForbidden)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Mkdir(res.fs, 0o750); err != nil {
		if os.IsExist(err) {
			http.Error(w, "exists", http.StatusMethodNotAllowed)
		} else {
			log.Print(err)
			http.Error(w, "storage error", http.StatusInternalServerError)
		}
		return
	}
	w.WriteHeader(http.StatusCreated)
}
