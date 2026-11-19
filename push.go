package main

// Web Push without dependencies: VAPID-signed (RFC 8292) requests carrying
// aes128gcm-encrypted payloads (RFC 8291), sent to the push service of every
// device a user turned notifications on for. Each send records its outcome on
// the device, and the service worker confirms each notification it showed, so
// the UI can say which devices really work instead of failing silently.

import (
	"bytes"
	"cmp"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

const pushSchema = `
CREATE TABLE IF NOT EXISTS settings (
	name  TEXT PRIMARY KEY,
	value BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS push_devices (
	id       TEXT PRIMARY KEY,
	user     TEXT NOT NULL,
	endpoint TEXT NOT NULL,
	p256dh   BLOB NOT NULL,
	auth     BLOB NOT NULL,
	subject  TEXT NOT NULL,
	name     TEXT NOT NULL,
	created  INTEGER NOT NULL,
	sent     INTEGER NOT NULL DEFAULT 0,
	shown    INTEGER NOT NULL DEFAULT 0,
	error    TEXT NOT NULL DEFAULT '',
	error_at INTEGER NOT NULL DEFAULT 0,
	gone     INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS push_sent (
	user TEXT NOT NULL,
	cal  TEXT NOT NULL,
	name TEXT NOT NULL,
	at   INTEGER NOT NULL,
	PRIMARY KEY (user, cal, name, at)
);`

const (
	// Apple accepts at most 4 KiB per push; this leaves room for the encryption header.
	maxPushPayload = 3000
	// Alarms that fell due while the server was down are still sent if they
	// are at most this old; older ones would only be noise.
	pushCatchUp = 10 * time.Minute
	// Each device gets its own goroutine, so a whole send takes at most this,
	// well inside the server's WriteTimeout.
	pushTimeout = 10 * time.Second
)

// pushServices are the hosts (and their subdomains) browsers subscribe with:
// Chrome/Edge/Android, Firefox, Safari, legacy Edge. The server POSTs to
// whatever endpoint a user registers, so anything else is refused.
var pushServices = []string{"fcm.googleapis.com", "android.googleapis.com", "push.services.mozilla.com", "push.apple.com", "notify.windows.com"}

func newServer(db *sql.DB) (*server, error) {
	key, err := loadVAPIDKey(db)
	if err != nil {
		return nil, err
	}
	public, err := key.PublicKey.Bytes()
	if err != nil {
		return nil, err
	}
	return &server{
		db:          db,
		vapid:       key,
		vapidPublic: base64.RawURLEncoding.EncodeToString(public),
		pushSubject: os.Getenv("PUSH_SUBJECT"),
		push: &http.Client{
			Timeout:       pushTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		pushHosts: pushServices,
	}, nil
}

// loadVAPIDKey returns the server's push signing key, creating it on first
// start. Browsers bind subscriptions to it, so losing it disconnects every device.
func loadVAPIDKey(db *sql.DB) (*ecdsa.PrivateKey, error) {
	var raw []byte
	err := db.QueryRow(`SELECT value FROM settings WHERE name = 'vapid_private_key'`).Scan(&raw)
	if err == nil {
		return ecdsa.ParseRawPrivateKey(elliptic.P256(), raw)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	if raw, err = key.Bytes(); err != nil {
		return nil, err
	}
	if _, err := db.Exec(`INSERT INTO settings (name, value) VALUES ('vapid_private_key', ?)`, raw); err != nil {
		return nil, err
	}
	log.Printf("push: generated a new VAPID key")
	return key, nil
}

// ── sending ──────────────────────────────────────────────────────────────────

type pushMessage struct {
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	URL   string `json:"url,omitempty"`
	ID    string `json:"id,omitempty"` // the device's, so its service worker can confirm
}

type pushDevice struct {
	id, endpoint, subject, name string
	p256dh, auth                []byte
	gone                        bool
}

type pushResult struct {
	ID      string `json:"id"`
	Device  string `json:"device"`
	OK      bool   `json:"ok"`
	Code    string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`
}

// sendToUser sends msg to each of the user's devices (or just to onlyID) and
// records every outcome on the device. Devices whose subscription expired
// are skipped unless asked for by id.
func (s *server) sendToUser(ctx context.Context, user, onlyID string, msg pushMessage) ([]pushResult, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, endpoint, p256dh, auth, subject, name, gone FROM push_devices WHERE user = ?`, user)
	if err != nil {
		return nil, err
	}
	var devices []pushDevice
	for rows.Next() {
		var d pushDevice
		var gone int
		if err := rows.Scan(&d.id, &d.endpoint, &d.p256dh, &d.auth, &d.subject, &d.name, &gone); err != nil {
			rows.Close()
			return nil, err
		}
		d.gone = gone != 0
		devices = append(devices, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	devices = slices.DeleteFunc(devices, func(d pushDevice) bool {
		return (onlyID != "" && d.id != onlyID) || (onlyID == "" && d.gone)
	})
	// In parallel, so one unreachable device can't hold up the others or
	// push the HTTP answer past the server's write timeout.
	// Detached from the caller: a user who leaves the page mid-send must not
	// leave every device marked as failed. pushTimeout bounds each send.
	ctx = context.WithoutCancel(ctx)
	results := make([]pushResult, len(devices))
	var wg sync.WaitGroup
	for i, d := range devices {
		wg.Go(func() {
			msg := msg
			msg.ID = d.id
			payload, _ := json.Marshal(msg)
			started := time.Now().Unix()
			code, err := s.sendPush(ctx, d, payload)
			s.recordPush(ctx, d.id, started, code, err)
			results[i] = pushResult{ID: d.id, Device: d.name, OK: err == nil}
			if err != nil {
				results[i].Code, results[i].Message = code, err.Error()
				log.Printf("%s push to %s's %s (%s) failed: %v", code, user, d.name, d.id, err)
			} else {
				log.Printf("push to %s's %s (%s): accepted, %q", user, d.name, d.id, msg.Title)
			}
		})
	}
	wg.Wait()
	return results, nil
}

func (s *server) sendPush(ctx context.Context, d pushDevice, payload []byte) (string, error) {
	if len(payload) > maxPushPayload {
		return "E-PUSH-TOO-LARGE", fmt.Errorf("the notification is %d bytes, at most %d fit", len(payload), maxPushPayload)
	}
	body, err := encryptPush(payload, d.p256dh, d.auth)
	if err != nil {
		return "E-PUSH-ENCRYPT", err
	}
	auth, err := s.vapidAuth(d.endpoint, cmp.Or(s.pushSubject, d.subject))
	if err != nil {
		return "E-PUSH-VAPID", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.endpoint, bytes.NewReader(body))
	if err != nil {
		return "E-PUSH-REQUEST-BUILD", err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", "86400")
	req.Header.Set("Urgency", "high")
	resp, err := s.push.Do(req)
	if err != nil {
		return "E-PUSH-UNREACHABLE", fmt.Errorf("the push service could not be reached: %w", err)
	}
	defer func() {
		// Read to the end so the connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
	}()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return "", nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return "E-PUSH-GONE", fmt.Errorf("the subscription expired (%s); open the calendar on this device to renew it", resp.Status)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// Apple's reason (e.g. BadJwtToken) is the only clue to what's wrong.
		reason, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return "E-PUSH-REJECTED-AUTH", fmt.Errorf("the push service refused our signature (%s %s); set PUSH_SUBJECT to a mailto: address or https URL", resp.Status, strings.TrimSpace(string(reason)))
	default:
		// No response body: it's shown to the user, and must not leak what
		// some other server answered.
		return "E-PUSH-REJECTED", fmt.Errorf("the push service answered %s", resp.Status)
	}
}

func (s *server) recordPush(ctx context.Context, id string, started int64, code string, sendErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var err error
	if sendErr == nil {
		_, err = s.db.ExecContext(ctx, `UPDATE push_devices SET sent = ?, error = '', error_at = 0, gone = 0 WHERE id = ?`, started, id)
	} else {
		gone := 0
		if code == "E-PUSH-GONE" {
			gone = 1
		}
		_, err = s.db.ExecContext(ctx, `UPDATE push_devices SET error = ?, error_at = ?, gone = ? WHERE id = ?`,
			code+" "+sendErr.Error(), time.Now().Unix(), gone, id)
	}
	if err != nil {
		log.Printf("E-PUSH-DB-STATUS recording the push outcome for device %s: %v", id, err)
	}
}

// vapidAuth signs a short-lived ES256 JWT for the endpoint's push service.
func (s *server) vapidAuth(endpoint, subject string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(map[string]any{
		"aud": u.Scheme + "://" + u.Host,
		"exp": time.Now().Add(12 * time.Hour).Unix(),
		"sub": subject,
	})
	if err != nil {
		return "", err
	}
	b64 := base64.RawURLEncoding.EncodeToString
	unsigned := b64([]byte(`{"typ":"JWT","alg":"ES256"}`)) + "." + b64(claims)
	hash := sha256.Sum256([]byte(unsigned))
	r, sig, err := ecdsa.Sign(rand.Reader, s.vapid, hash[:])
	if err != nil {
		return "", err
	}
	raw := make([]byte, 64)
	r.FillBytes(raw[:32])
	sig.FillBytes(raw[32:])
	return "vapid t=" + unsigned + "." + b64(raw) + ", k=" + s.vapidPublic, nil
}

func encryptPush(plaintext, uaPublic, authSecret []byte) ([]byte, error) {
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	return encryptPushWith(plaintext, uaPublic, authSecret, key, salt)
}

// encryptPushWith is RFC 8291 with a single aes128gcm record.
func encryptPushWith(plaintext, uaPublic, authSecret []byte, key *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	ua, err := ecdh.P256().NewPublicKey(uaPublic)
	if err != nil {
		return nil, fmt.Errorf("the device's key is invalid: %w", err)
	}
	shared, err := key.ECDH(ua)
	if err != nil {
		return nil, err
	}
	asPublic := key.PublicKey().Bytes()
	ikm, err := hkdf.Key(sha256.New, shared, authSecret, "WebPush: info\x00"+string(uaPublic)+string(asPublic), 32)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	header := slices.Concat(salt, binary.BigEndian.AppendUint32(nil, 4096), []byte{byte(len(asPublic))}, asPublic)
	// 0x02 marks the last (and only) record.
	return gcm.Seal(header, nonce, slices.Concat(plaintext, []byte{2}), nil), nil
}

// ── event alarms ─────────────────────────────────────────────────────────────

func (s *server) pushLoop() {
	for range time.Tick(30 * time.Second) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		s.notifyDue(ctx, time.Now())
		cancel()
	}
}

// notifyDue sends every event alarm that fell due within pushCatchUp before
// now, once: push_sent remembers what was sent, so a moved event alarms again.
func (s *server) notifyDue(ctx context.Context, now time.Time) {
	began := time.Now()
	rows, err := s.db.QueryContext(ctx, `SELECT user FROM push_devices WHERE gone = 0`)
	if err != nil {
		log.Printf("E-PUSH-DUE-USERS-QUERY reading users with devices: %v", err)
		return
	}
	var users []string
	for rows.Next() {
		var user string
		if err := rows.Scan(&user); err != nil {
			log.Printf("E-PUSH-DUE-USERS-SCAN reading users with devices: %v", err)
			continue
		}
		if !slices.Contains(users, user) {
			users = append(users, user)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		log.Printf("E-PUSH-DUE-USERS-ROWS reading users with devices: %v", err)
		return
	}

	type due struct {
		cal, name string
		at        time.Time
		msg       pushMessage
	}
	for _, user := range users {
		var dues []due
		rows, err := s.db.QueryContext(ctx, `SELECT cal, name, data FROM objects WHERE user = ?`, user)
		if err != nil {
			log.Printf("E-PUSH-DUE-EVENTS-QUERY reading %s's events: %v", user, err)
			continue
		}
		for rows.Next() {
			var cal, name string
			var data []byte
			if err := rows.Scan(&cal, &name, &data); err != nil {
				log.Printf("E-PUSH-DUE-EVENTS-SCAN reading %s's events: %v", user, err)
				break
			}
			ev := parseEvent(data)
			for _, at := range ev.alarms() {
				if at.After(now) || !at.After(now.Add(-pushCatchUp)) {
					continue
				}
				dues = append(dues, due{cal: cal, name: name, at: at, msg: eventMessage(ev, now)})
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			log.Printf("E-PUSH-DUE-EVENTS-ROWS reading %s's events: %v", user, err)
		}

		for _, d := range dues {
			// Marked before sending: an alarm is sent at most once, and a
			// failed send is recorded on the device rather than retried.
			s.mu.Lock()
			res, err := s.db.ExecContext(ctx, `INSERT INTO push_sent (user, cal, name, at) VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`,
				user, d.cal, d.name, d.at.Unix())
			s.mu.Unlock()
			if err != nil {
				log.Printf("E-PUSH-DUE-MARK marking the alarm for %s/%s/%s: %v", user, d.cal, d.name, err)
				continue
			}
			if n, _ := res.RowsAffected(); n == 0 {
				continue
			}
			log.Printf("push: alarm for %s/%s/%s at %s", user, d.cal, d.name, d.at.Format(time.RFC3339))
			if _, err := s.sendToUser(ctx, user, "", d.msg); err != nil {
				log.Printf("E-PUSH-DUE-DEVICES reading %s's devices: %v", user, err)
			}
		}
	}

	s.mu.Lock()
	_, err = s.db.ExecContext(ctx, `DELETE FROM push_sent WHERE at < ?`, now.Add(-48*time.Hour).Unix())
	s.mu.Unlock()
	if err != nil {
		log.Printf("E-PUSH-DUE-CLEANUP forgetting old alarms: %v", err)
	}
	if took := time.Since(began); took > time.Second {
		log.Printf("push: checking alarms took %s", took)
	}
}

func eventMessage(ev eventInfo, now time.Time) pushMessage {
	label, _, _ := dayLabel(ev.start, now)
	clock, detail := ev.times()
	body := label + " · " + clock
	if detail != "" {
		body += " · " + detail
	}
	title := cmp.Or(ev.summary, "(no title)")
	// A long summary must not push the alarm over maxPushPayload.
	if runes := []rune(title); len(runes) > 200 {
		title = string(runes[:200]) + "…"
	}
	return pushMessage{Title: title, Body: body, URL: "/~/"}
}

// alarms lists when to notify about an event: its VALARM triggers, or else
// its start (9:00 on the day for all-day events). Recurrences aren't expanded.
func (ev eventInfo) alarms() []time.Time {
	if ev.start.IsZero() {
		return nil
	}
	var out []time.Time
	for _, tr := range ev.triggers {
		params := strings.ToUpper(tr.params)
		if strings.Contains(params, "VALUE=DATE-TIME") {
			if t, _, _ := parseTime(tr.value, ""); !t.IsZero() {
				out = append(out, t)
			}
			continue
		}
		d, ok := parseDuration(tr.value)
		if !ok {
			continue
		}
		if strings.Contains(params, "RELATED=END") {
			out = append(out, ev.ends().Add(d))
		} else {
			out = append(out, ev.start.Add(d))
		}
	}
	if len(out) > 0 {
		return out
	}
	if ev.allDay {
		y, m, d := ev.start.Date()
		return []time.Time{time.Date(y, m, d, 9, 0, 0, 0, ev.start.Location())}
	}
	return []time.Time{ev.start}
}

// parseDuration reads an RFC 5545 duration such as -PT15M or P1DT2H30M.
func parseDuration(s string) (time.Duration, bool) {
	sign := time.Duration(1)
	if rest, ok := strings.CutPrefix(s, "-"); ok {
		sign, s = -1, rest
	} else {
		s = strings.TrimPrefix(s, "+")
	}
	s, ok := strings.CutPrefix(s, "P")
	if !ok || s == "" {
		return 0, false
	}
	units := map[byte]time.Duration{'W': 7 * 24 * time.Hour, 'D': 24 * time.Hour, 'H': time.Hour, 'M': time.Minute, 'S': time.Second}
	var d time.Duration
	n, parts := -1, 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == 'T':
		case c >= '0' && c <= '9':
			n = max(n, 0)*10 + int(c-'0')
		default:
			unit, ok := units[c]
			if !ok || n < 0 {
				return 0, false
			}
			d += time.Duration(n) * unit
			n = -1
			parts++
		}
	}
	return sign * d, n < 0 && parts > 0
}

// ── handlers ─────────────────────────────────────────────────────────────────

func (s *server) pushSubscribe(w http.ResponseWriter, r *http.Request, user string) {
	var req struct {
		Subscription struct {
			Endpoint string `json:"endpoint"`
			Keys     struct {
				P256dh string `json:"p256dh"`
				Auth   string `json:"auth"`
			} `json:"keys"`
		} `json:"subscription"`
		Replaces string `json:"replaces"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		uiError(w, http.StatusBadRequest, "E-PUSH-SUB-JSON", fmt.Errorf("the subscription is not valid JSON: %w", err))
		return
	}
	endpoint := req.Subscription.Endpoint
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		uiError(w, http.StatusUnprocessableEntity, "E-PUSH-SUB-URL", fmt.Errorf("the push endpoint %q is not an https URL", endpoint))
		return
	}
	// The server will POST to this URL on the user's behalf, so it must be a
	// real push service and not, say, something on the server's own network.
	if !knownPushHost(s.pushHosts, u.Hostname()) {
		uiError(w, http.StatusUnprocessableEntity, "E-PUSH-SUB-HOST", fmt.Errorf("%s is not a known push service (%s)", u.Hostname(), strings.Join(s.pushHosts, ", ")))
		return
	}
	decode := func(s string) []byte {
		b, _ := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
		return b
	}
	p256dh, auth := decode(req.Subscription.Keys.P256dh), decode(req.Subscription.Keys.Auth)
	if _, err := ecdh.P256().NewPublicKey(p256dh); err != nil || len(auth) != 16 {
		uiError(w, http.StatusUnprocessableEntity, "E-PUSH-SUB-KEYS", errors.New("the subscription's keys are missing or invalid"))
		return
	}
	// Apple rejects VAPID tokens whose subject isn't a real mailto: or https
	// URL; the page's own origin is one, and the proxy keeps it intact.
	subject := r.Header.Get("Origin")
	if !strings.HasPrefix(subject, "https://") {
		subject = "https://" + r.Host
	}
	sum := sha256.Sum256([]byte(endpoint))
	id := hex.EncodeToString(sum[:8])
	name := deviceName(r.UserAgent())

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		uiError(w, http.StatusInternalServerError, "E-PUSH-DB-SUB-BEGIN", err)
		return
	}
	defer tx.Rollback()
	if req.Replaces != "" && req.Replaces != endpoint {
		if _, err := tx.ExecContext(r.Context(), `DELETE FROM push_devices WHERE user = ? AND endpoint = ?`, user, req.Replaces); err != nil {
			uiError(w, http.StatusInternalServerError, "E-PUSH-DB-SUB-REPLACE", err)
			return
		}
	}
	// The page re-sends its subscription on every visit; only a new one
	// (other keys or user) starts over with a clean status.
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM push_devices WHERE id = ? AND (user != ? OR p256dh != ? OR auth != ?)`, id, user, p256dh, auth); err != nil {
		uiError(w, http.StatusInternalServerError, "E-PUSH-DB-SUB-RESET", err)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `INSERT INTO push_devices (id, user, endpoint, p256dh, auth, subject, name, created) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET subject = excluded.subject, name = excluded.name`,
		id, user, endpoint, p256dh, auth, subject, name, time.Now().Unix()); err != nil {
		uiError(w, http.StatusInternalServerError, "E-PUSH-DB-SUB-SAVE", err)
		return
	}
	var gone bool
	if err := tx.QueryRowContext(r.Context(), `SELECT gone != 0 FROM push_devices WHERE id = ?`, id).Scan(&gone); err != nil {
		uiError(w, http.StatusInternalServerError, "E-PUSH-DB-SUB-GONE", err)
		return
	}
	if err := tx.Commit(); err != nil {
		uiError(w, http.StatusInternalServerError, "E-PUSH-DB-SUB-COMMIT", err)
		return
	}
	// A browser can hand back a subscription its push service already dropped;
	// "gone" tells the page to throw it away and subscribe afresh.
	log.Printf("push: %s saved device %s (%s), gone=%v", user, id, name, gone)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "gone": gone})
}

func knownPushHost(hosts []string, host string) bool {
	return slices.ContainsFunc(hosts, func(h string) bool { return host == h || strings.HasSuffix(host, "."+h) })
}

func deviceName(ua string) string {
	device := "Unknown device"
	for _, d := range [][2]string{{"iPhone", "iPhone"}, {"iPad", "iPad"}, {"Android", "Android"}, {"CrOS", "ChromeOS"}, {"Macintosh", "Mac"}, {"Windows", "Windows"}, {"Linux", "Linux"}} {
		if strings.Contains(ua, d[0]) {
			device = d[1]
			break
		}
	}
	for _, b := range [][2]string{{"Firefox", "Firefox"}, {"FxiOS", "Firefox"}, {"Edg", "Edge"}, {"Chrome", "Chrome"}, {"CriOS", "Chrome"}, {"Safari", "Safari"}} {
		if strings.Contains(ua, b[0]) {
			return device + " · " + b[1]
		}
	}
	return device
}

type deviceView struct {
	ID, Name, Status string
	Failed           bool
}

func (s *server) loadDevices(ctx context.Context, user string) ([]deviceView, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, sent, shown, error, error_at FROM push_devices WHERE user = ? ORDER BY created`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	when := func(unix int64) string { return time.Unix(unix, 0).Format("2 Jan 15:04") }
	var devices []deviceView
	for rows.Next() {
		var d deviceView
		var sent, shown, errorAt int64
		var lastError string
		if err := rows.Scan(&d.ID, &d.Name, &sent, &shown, &lastError, &errorAt); err != nil {
			return nil, err
		}
		switch {
		case lastError != "":
			d.Status, d.Failed = lastError+" ("+when(errorAt)+")", true
		case sent == 0:
			d.Status = "Nothing sent yet"
		case shown >= sent:
			d.Status = "Last notification " + when(sent) + ", shown on the device"
		default:
			d.Status = "Last notification " + when(sent) + " was accepted for delivery, but the device hasn't confirmed it yet"
		}
		devices = append(devices, d)
	}
	return devices, rows.Err()
}

func (s *server) renderDevices(w http.ResponseWriter, ctx context.Context, user string, extra ...string) {
	devices, err := s.loadDevices(ctx, user)
	if err != nil {
		uiError(w, http.StatusInternalServerError, "E-PUSH-DB-DEVICES", err)
		return
	}
	render(w, "devices", devices, extra...)
}

func (s *server) pushTest(w http.ResponseWriter, r *http.Request, user string) {
	id := r.FormValue("id")
	results, err := s.sendToUser(r.Context(), user, id, pushMessage{Title: "Test notification", Body: "Notifications work on this device.", URL: "/~/"})
	if err != nil {
		uiError(w, http.StatusInternalServerError, "E-PUSH-DB-TEST", err)
		return
	}
	if len(results) == 0 && id != "" {
		uiError(w, http.StatusNotFound, "E-PUSH-TEST-UNKNOWN", errors.New("this device was removed; reload the page"))
		return
	}
	if len(results) == 0 {
		uiError(w, http.StatusConflict, "E-PUSH-NO-DEVICES", errors.New("no device has notifications turned on"))
		return
	}
	var failures strings.Builder
	for _, res := range results {
		if !res.OK {
			fmt.Fprintf(&failures, "<p><strong>%s</strong> %s: %s</p>", res.Code, html.EscapeString(res.Device), html.EscapeString(res.Message))
		}
	}
	box := clearError
	if failures.Len() > 0 {
		box = `<div id="error" role="alert" hx-swap-oob="true">` + failures.String() + `</div>`
	}
	s.renderDevices(w, r.Context(), user, box)
}

func (s *server) pushRemove(w http.ResponseWriter, r *http.Request, user string) {
	s.mu.Lock()
	_, err := s.db.ExecContext(r.Context(), `DELETE FROM push_devices WHERE user = ? AND id = ?`, user, r.FormValue("id"))
	s.mu.Unlock()
	if err != nil {
		uiError(w, http.StatusInternalServerError, "E-PUSH-DB-REMOVE", err)
		return
	}
	s.renderDevices(w, r.Context(), user, clearError)
}

// pushAck is called by the service worker after it showed a notification.
func (s *server) pushAck(w http.ResponseWriter, r *http.Request, user string) {
	s.mu.Lock()
	_, err := s.db.ExecContext(r.Context(), `UPDATE push_devices SET shown = ? WHERE user = ? AND id = ?`, time.Now().Unix(), user, r.FormValue("id"))
	s.mu.Unlock()
	if err != nil {
		uiError(w, http.StatusInternalServerError, "E-PUSH-DB-ACK", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

var clientCode = regexp.MustCompile(`^E-PUSH-[A-Z-]{1,40}$`)

// pushLog records error codes the page showed, which happen in the browser
// (permission, subscribing, unsupported browser) and would otherwise never
// reach the server log.
func pushLog(w http.ResponseWriter, r *http.Request, user string) {
	code := r.FormValue("code")
	if !clientCode.MatchString(code) {
		code = "E-PUSH-CLIENT-UNKNOWN"
	}
	message := r.FormValue("message")
	if len(message) > 500 {
		message = message[:500]
	}
	log.Printf("%s on %s's %s: %q", code, user, deviceName(r.UserAgent()), message)
	w.WriteHeader(http.StatusNoContent)
}

// apiNotify lets scripts send a notification to all of the caller's devices:
//
//	curl -u user:pass -d title=Hello -d body=World https://host/~/api/notify
//
// JSON ({"title", "body", "url"}) works too. The answer says per device
// whether the push service accepted it.
func (s *server) apiNotify(w http.ResponseWriter, r *http.Request, user string) {
	var msg pushMessage
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			apiError(w, http.StatusBadRequest, "E-API-JSON", fmt.Errorf("the body is not valid JSON: %w", err))
			return
		}
	} else {
		msg = pushMessage{Title: r.FormValue("title"), Body: r.FormValue("body"), URL: r.FormValue("url")}
	}
	msg.ID = ""
	msg.Title = strings.TrimSpace(msg.Title)
	if msg.Title == "" {
		apiError(w, http.StatusUnprocessableEntity, "E-API-TITLE", errors.New("title is required"))
		return
	}
	if msg.URL != "" {
		u, err := url.Parse(msg.URL)
		if err != nil || !(u.Scheme == "https" || u.Scheme == "http" || (u.Scheme == "" && strings.HasPrefix(msg.URL, "/"))) {
			apiError(w, http.StatusUnprocessableEntity, "E-API-URL", fmt.Errorf("url %q must be an http(s) URL or a path", msg.URL))
			return
		}
	}
	if payload, _ := json.Marshal(msg); len(payload) > maxPushPayload-100 {
		apiError(w, http.StatusRequestEntityTooLarge, "E-API-TOO-LARGE", fmt.Errorf("title, body and url take %d bytes, at most %d fit", len(payload), maxPushPayload-100))
		return
	}
	log.Printf("api %s: notify %q", user, msg.Title)
	results, err := s.sendToUser(r.Context(), user, "", msg)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "E-API-DB", err)
		return
	}
	if len(results) == 0 {
		apiError(w, http.StatusConflict, "E-API-NO-DEVICES", errors.New("no device has notifications turned on; open the calendar on a device and turn them on"))
		return
	}
	reply := struct {
		Sent    int          `json:"sent"`
		Failed  int          `json:"failed"`
		Devices []pushResult `json:"devices"`
	}{Devices: results}
	for _, res := range results {
		if res.OK {
			reply.Sent++
		} else {
			reply.Failed++
		}
	}
	status := http.StatusOK
	if reply.Sent == 0 {
		status = http.StatusBadGateway
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(reply)
}

func apiError(w http.ResponseWriter, status int, code string, err error) {
	log.Printf("%s api error: %v", code, err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "message": err.Error()})
}

// ── installable app ──────────────────────────────────────────────────────────

// iOS only delivers push to pages added to the home screen, which needs the
// manifest; the service worker is what shows the notifications.
const manifestJSON = `{
  "name": "Calendar", "short_name": "Calendar", "id": "/~/", "start_url": "/~/", "scope": "/~/",
  "display": "standalone", "background_color": "#f6f6f4", "theme_color": "#2f6fdf",
  "icons": [{"src": "/~/icon.png", "sizes": "512x512", "type": "image/png", "purpose": "any"}]
}`

// Safari ends the subscription of a worker that receives a push without
// showing a notification, so every push shows one, even an unreadable one.
const serviceWorkerJS = `self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", e => e.waitUntil(self.clients.claim()));
self.addEventListener("push", e => {
  let msg = {};
  try { msg = e.data ? e.data.json() : {}; } catch (err) { msg = { body: e.data.text() }; }
  const shown = self.registration.showNotification(msg.title || "Calendar", {
    body: msg.body || "", icon: "/~/icon.png", data: { url: msg.url || "/~/" },
  });
  e.waitUntil(shown.then(() => msg.id && fetch("/~/push/ack", { method: "POST", body: new URLSearchParams({ id: msg.id }) }).catch(() => {})));
});
self.addEventListener("notificationclick", e => {
  e.notification.close();
  const url = new URL((e.notification.data && e.notification.data.url) || "/~/", self.location.origin).href;
  e.waitUntil(self.clients.matchAll({ type: "window", includeUncontrolled: true }).then(list => {
    const open = list.find(c => c.url === url);
    return open ? open.focus() : self.clients.openWindow(url);
  }));
});
`

var iconPNG = func() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 512, 512))
	fill := func(x0, y0, x1, y1 int, c color.Color) {
		draw.Draw(img, image.Rect(x0, y0, x1, y1), &image.Uniform{c}, image.Point{}, draw.Src)
	}
	blue, white := color.RGBA{0x2f, 0x6f, 0xdf, 0xff}, color.RGBA{0xff, 0xff, 0xff, 0xff}
	fill(0, 0, 512, 512, blue)
	fill(112, 128, 400, 400, white)
	fill(112, 128, 400, 192, color.RGBA{0x1d, 0x4f, 0xaf, 0xff})
	for row := range 3 {
		for col := range 4 {
			x, y := 140+col*66, 216+row*60
			fill(x, y, x+38, y+36, blue)
		}
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return b.Bytes()
}()

func serveStatic(w http.ResponseWriter, contentType, cacheControl string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", cacheControl)
	_, _ = w.Write(body)
}
