package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var b64 = base64.RawURLEncoding

func TestEncryptPushRFC8291(t *testing.T) {
	dec := func(s string) []byte {
		b, err := b64.DecodeString(strings.ReplaceAll(s, " ", ""))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	// RFC 8291, Appendix A.
	asKey, err := ecdh.P256().NewPrivateKey(dec("yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	if err != nil {
		t.Fatal(err)
	}
	uaKey, err := ecdh.P256().NewPrivateKey(dec("q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94"))
	if err != nil {
		t.Fatal(err)
	}
	auth := dec("BTBZMqHH6r4Tts7J_aSIgg")
	plaintext := []byte("When I grow up, I want to be a watermelon")
	got, err := encryptPushWith(plaintext, uaKey.PublicKey().Bytes(), auth, asKey, dec("DGv6ra1nlYgDCS1FRnbzlw"))
	if err != nil {
		t.Fatal(err)
	}
	want := append(dec("DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8"),
		dec("8pfeW0KbunFT06SuDKoJH9Ql87S1QUrdirN6GcG7sFz1y1sqLgVi1VhjVkHsUoEsbI_0LpXMuGvnzQ")...)
	if !bytes.Equal(got, want) {
		t.Fatalf("encrypted\n%s\nwant\n%s", b64.EncodeToString(got), b64.EncodeToString(want))
	}
	if back := decryptPush(t, got, uaKey, auth); !bytes.Equal(back, plaintext) {
		t.Fatalf("decrypted %q", back)
	}
}

// decryptPush is what the browser does with a push message.
func decryptPush(t *testing.T, body []byte, key *ecdh.PrivateKey, auth []byte) []byte {
	t.Helper()
	if len(body) < 21 || len(body) < 21+int(body[20]) {
		t.Fatalf("push body too short: %d bytes", len(body))
	}
	salt, idLen := body[:16], int(body[20])
	asPublic, ciphertext := body[21:21+idLen], body[21+idLen:]
	as, err := ecdh.P256().NewPublicKey(asPublic)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := key.ECDH(as)
	if err != nil {
		t.Fatal(err)
	}
	ikm, _ := hkdf.Key(sha256.New, shared, auth, "WebPush: info\x00"+string(key.PublicKey().Bytes())+string(asPublic), 32)
	cek, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		t.Fatalf("decrypting push: %v", err)
	}
	plain = bytes.TrimRight(plain, "\x00")
	if len(plain) == 0 || plain[len(plain)-1] != 2 {
		t.Fatal("push record lacks the last-record delimiter")
	}
	return plain[:len(plain)-1]
}

// ── a fake push service ──────────────────────────────────────────────────────

type fakeDevice struct {
	endpoint string
	key      *ecdh.PrivateKey
	auth     []byte
}

type pushed struct {
	path string
	msg  pushMessage
}

// fakePush stands in for FCM / Mozilla / Apple: it checks the VAPID token,
// decrypts with the device's key like a browser would, and answers with
// whatever status a test set for the device.
type fakePush struct {
	t       *testing.T
	ts      *httptest.Server
	vapid   string
	mu      sync.Mutex
	devices map[string]fakeDevice
	answer  map[string]int
	reason  map[string]string
	got     []pushed
}

func newFakePush(t *testing.T, vapidPublic string) *fakePush {
	f := &fakePush{t: t, vapid: vapidPublic, devices: map[string]fakeDevice{}, answer: map[string]int{}, reason: map[string]string{}}
	f.ts = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.ts.Close)
	return f
}

func (f *fakePush) device(path string) fakeDevice {
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		f.t.Fatal(err)
	}
	d := fakeDevice{endpoint: f.ts.URL + path, key: key, auth: make([]byte, 16)}
	_, _ = rand.Read(d.auth)
	f.mu.Lock()
	f.devices[path] = d
	f.mu.Unlock()
	return d
}

func (f *fakePush) set(path string, status int, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answer[path], f.reason[path] = status, reason
}

func (f *fakePush) received() []pushed {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pushed(nil), f.got...)
}

func (f *fakePush) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Content-Encoding") != "aes128gcm" || r.Header.Get("TTL") == "" {
		f.t.Errorf("push without aes128gcm/TTL headers: %v", r.Header)
	}
	if err := f.checkVAPID(r.Header.Get("Authorization")); err != nil {
		f.t.Errorf("bad VAPID authorization: %v", err)
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	d, ok := f.devices[r.URL.Path]
	if !ok {
		f.t.Errorf("push to unknown device %s", r.URL.Path)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var msg pushMessage
	if err := json.Unmarshal(decryptPush(f.t, body, d.key, d.auth), &msg); err != nil {
		f.t.Errorf("push payload is not JSON: %v", err)
	}
	f.got = append(f.got, pushed{r.URL.Path, msg})
	if status := f.answer[r.URL.Path]; status != 0 {
		if status/100 == 3 {
			w.Header().Set("Location", f.reason[r.URL.Path])
		}
		http.Error(w, f.reason[r.URL.Path], status)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (f *fakePush) checkVAPID(header string) error {
	rest, ok := strings.CutPrefix(header, "vapid t=")
	token, k, ok2 := strings.Cut(rest, ", k=")
	if !ok || !ok2 || k != f.vapid {
		return fmt.Errorf("malformed header %q", header)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return fmt.Errorf("token has %d parts", len(parts))
	}
	raw, _ := b64.DecodeString(k)
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), raw)
	if err != nil {
		return err
	}
	sig, _ := b64.DecodeString(parts[2])
	hash := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if len(sig) != 64 || !ecdsa.Verify(pub, hash[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return fmt.Errorf("signature does not verify")
	}
	claimsJSON, _ := b64.DecodeString(parts[1])
	var claims struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		return err
	}
	if claims.Aud != f.ts.URL {
		return fmt.Errorf("aud %q, want %q", claims.Aud, f.ts.URL)
	}
	if exp := time.Unix(claims.Exp, 0); exp.Before(time.Now()) || exp.After(time.Now().Add(24*time.Hour)) {
		return fmt.Errorf("exp %s is not within 24 hours", exp)
	}
	if !strings.HasPrefix(claims.Sub, "https://") && !strings.HasPrefix(claims.Sub, "mailto:") {
		return fmt.Errorf("sub %q is neither https nor mailto", claims.Sub)
	}
	return nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

func newPushTestServer(t *testing.T) (*server, *httptest.Server, *fakePush) {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "caldav.db"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := newServer(db)
	if err != nil {
		t.Fatal(err)
	}
	fake := newFakePush(t, s.vapidPublic)
	// Only the transport (which trusts the fake's certificate) is swapped, so
	// tests run against the production client's redirect and timeout settings.
	s.push.Transport = fake.ts.Client().Transport
	s.pushHosts = []string{"127.0.0.1"}
	ts := httptest.NewServer(s)
	t.Cleanup(func() {
		ts.Close()
		db.Close()
	})
	return s, ts, fake
}

const (
	iPhoneUA  = "Mozilla/5.0 (iPhone; CPU iPhone OS 18_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Mobile/15E148 Safari/604.1"
	firefoxUA = "Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0"
)

// subscribe sends what the page's script sends after pushManager.subscribe.
func subscribe(t *testing.T, ts *httptest.Server, user, ua string, d fakeDevice, replaces string) (*http.Response, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"subscription": map[string]any{
			"endpoint": d.endpoint,
			"keys":     map[string]string{"p256dh": b64.EncodeToString(d.key.PublicKey().Bytes()), "auth": b64.EncodeToString(d.auth)},
		},
		"replaces": replaces,
	})
	return rawSubscribe(t, ts, user, ua, string(body))
}

func rawSubscribe(t *testing.T, ts *httptest.Server, user, ua, body string, headers ...string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest("POST", ts.URL+"/~/push/subscribe", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Remote-User", user)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Origin", "https://cal.example")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

var deviceIDRe = regexp.MustCompile(`/~/push/test\?id=([0-9a-f]+)`)

func deviceIDs(t *testing.T, ts *httptest.Server, user string) []string {
	t.Helper()
	resp, body := uiDo(t, ts, user, "GET", "/~/push/devices", nil)
	expectUIStatus(t, "devices", resp, body, http.StatusOK)
	var ids []string
	for _, m := range deviceIDRe.FindAllStringSubmatch(body, -1) {
		ids = append(ids, m[1])
	}
	return ids
}

func devicesHTML(t *testing.T, ts *httptest.Server, user string) string {
	t.Helper()
	resp, body := uiDo(t, ts, user, "GET", "/~/push/devices", nil)
	expectUIStatus(t, "devices", resp, body, http.StatusOK)
	return body
}

// api calls the notify API the way a script would: no browser headers.
func api(t *testing.T, ts *httptest.Server, user, contentType, body string, headers ...string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest("POST", ts.URL+"/~/api/notify", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Remote-User", user)
	req.Header.Set("Content-Type", contentType)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var reply map[string]any
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatalf("api answered non-JSON (%d): %s", resp.StatusCode, raw)
	}
	return resp.StatusCode, reply
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestPushInstallableApp(t *testing.T) {
	_, ts, _ := newPushTestServer(t)
	resp, body := uiDo(t, ts, "alice", "GET", "/~/", nil)
	expectUIStatus(t, "page", resp, body, http.StatusOK)
	for _, want := range []string{`<link rel="manifest" href="/~/manifest.webmanifest" crossorigin="use-credentials">`, `<link rel="apple-touch-icon"`, "No device gets notifications yet.", "E-PUSH-IOS-HOMESCREEN"} {
		if !strings.Contains(body, want) {
			t.Fatalf("page lacks %q", want)
		}
	}

	resp, body = uiDo(t, ts, "alice", "GET", "/~/manifest.webmanifest", nil)
	expectUIStatus(t, "manifest", resp, body, http.StatusOK)
	var manifest struct{ Display, Scope, StartURL string }
	if err := json.Unmarshal([]byte(body), &manifest); err != nil || manifest.Display != "standalone" || manifest.Scope != "/~/" {
		t.Fatalf("manifest %+v, %v", manifest, err)
	}
	resp, body = uiDo(t, ts, "alice", "GET", "/~/sw.js", nil)
	expectUIStatus(t, "service worker", resp, body, http.StatusOK)
	if !strings.Contains(resp.Header.Get("Content-Type"), "javascript") || !strings.Contains(body, "showNotification") {
		t.Fatalf("service worker served as %q:\n%s", resp.Header.Get("Content-Type"), body)
	}
	resp, body = uiDo(t, ts, "alice", "GET", "/~/icon.png", nil)
	expectUIStatus(t, "icon", resp, body, http.StatusOK)
	if img, err := png.Decode(strings.NewReader(body)); err != nil || img.Bounds().Dx() != 512 {
		t.Fatalf("icon is not a 512px PNG: %v", err)
	}
}

func TestPushTestAndConfirm(t *testing.T) {
	_, ts, fake := newPushTestServer(t)
	phone := fake.device("/apple/phone")

	resp, body := subscribe(t, ts, "alice", iPhoneUA, phone, "")
	expectUIStatus(t, "subscribe", resp, body, http.StatusOK)
	// The page re-sends its subscription on every visit.
	resp, body = subscribe(t, ts, "alice", iPhoneUA, phone, "")
	expectUIStatus(t, "subscribe again", resp, body, http.StatusOK)
	ids := deviceIDs(t, ts, "alice")
	if len(ids) != 1 {
		t.Fatalf("devices %v, want one", ids)
	}
	if html := devicesHTML(t, ts, "alice"); !strings.Contains(html, "iPhone · Safari") || !strings.Contains(html, "Nothing sent yet") {
		t.Fatalf("device list:\n%s", html)
	}

	resp, body = uiDo(t, ts, "alice", "POST", "/~/push/test?id="+ids[0], url.Values{})
	expectUIStatus(t, "test", resp, body, http.StatusOK)
	if !strings.Contains(body, clearError) || !strings.Contains(body, "the device hasn&#39;t confirmed it yet") {
		t.Fatalf("test response:\n%s", body)
	}
	got := fake.received()
	if len(got) != 1 || got[0].msg.Title != "Test notification" || got[0].msg.ID != ids[0] {
		t.Fatalf("push service received %+v", got)
	}

	// The service worker confirms after showing it; other users can't.
	resp, body = uiDo(t, ts, "bob", "POST", "/~/push/ack", url.Values{"id": {ids[0]}})
	expectUIStatus(t, "bob acks", resp, body, http.StatusNoContent)
	if html := devicesHTML(t, ts, "alice"); strings.Contains(html, "shown on the device") {
		t.Fatal("bob confirmed alice's notification")
	}
	resp, body = uiDo(t, ts, "alice", "POST", "/~/push/ack", url.Values{"id": {ids[0]}})
	expectUIStatus(t, "ack", resp, body, http.StatusNoContent)
	if html := devicesHTML(t, ts, "alice"); !strings.Contains(html, "shown on the device") {
		t.Fatalf("device list after ack:\n%s", html)
	}

	resp, body = uiDo(t, ts, "bob", "POST", "/~/push/test", url.Values{})
	expectUIStatus(t, "bob tests", resp, body, http.StatusConflict)
	if !strings.Contains(body, "E-PUSH-NO-DEVICES") || len(fake.received()) != 1 {
		t.Fatalf("bob's test: %s", body)
	}

	resp, body = uiDo(t, ts, "bob", "DELETE", "/~/push/devices?id="+ids[0], nil)
	expectUIStatus(t, "bob removes", resp, body, http.StatusOK)
	if len(deviceIDs(t, ts, "alice")) != 1 {
		t.Fatal("bob removed alice's device")
	}
	resp, body = uiDo(t, ts, "alice", "DELETE", "/~/push/devices?id="+ids[0], nil)
	expectUIStatus(t, "remove", resp, body, http.StatusOK)
	if !strings.Contains(body, "No device gets notifications yet.") {
		t.Fatalf("after removing:\n%s", body)
	}
	resp, body = uiDo(t, ts, "alice", "POST", "/~/push/test?id="+ids[0], url.Values{})
	expectUIStatus(t, "test removed", resp, body, http.StatusNotFound)
	if !strings.Contains(body, "E-PUSH-TEST-UNKNOWN") {
		t.Fatalf("testing a removed device: %s", body)
	}
}

func TestPushFailuresAreShown(t *testing.T) {
	_, ts, fake := newPushTestServer(t)
	expired, refused, working := fake.device("/expired"), fake.device("/refused"), fake.device("/working")
	fake.set("/expired", http.StatusGone, "")
	fake.set("/refused", http.StatusForbidden, `{"reason":"BadJwtToken"}`)
	unreachable := fake.device("/unreachable")
	unreachable.endpoint = "https://127.0.0.1:1/unreachable"
	// A redirect could bounce the POST to the server's own network; it must
	// not be followed, nor its body shown.
	redirect := fake.device("/redirect")
	fake.set("/redirect", http.StatusTemporaryRedirect, "/internal-secret")
	for _, d := range []fakeDevice{expired, refused, working, unreachable, redirect} {
		resp, body := subscribe(t, ts, "alice", firefoxUA, d, "")
		expectUIStatus(t, "subscribe", resp, body, http.StatusOK)
	}

	resp, body := uiDo(t, ts, "alice", "POST", "/~/push/test", url.Values{})
	expectUIStatus(t, "test all", resp, body, http.StatusOK)
	for _, want := range []string{`<div id="error" role="alert" hx-swap-oob="true">`, "E-PUSH-GONE", "E-PUSH-REJECTED-AUTH", "BadJwtToken", "PUSH_SUBJECT", "E-PUSH-UNREACHABLE", "E-PUSH-REJECTED</strong>", `class="device failed"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("test response lacks %q:\n%s", want, body)
		}
	}
	if n := strings.Count(body, `class="device failed"`); n != 4 {
		t.Fatalf("%d failed devices shown, want 4:\n%s", n, body)
	}
	if strings.Contains(body, "internal-secret") {
		t.Fatalf("a redirect's body was shown:\n%s", body)
	}
	for _, p := range fake.received() {
		if p.path == "/internal-secret" {
			t.Fatal("followed a redirect from the push service")
		}
	}

	// A browser may hand back the expired subscription; the page is told to
	// replace it.
	resp, body = subscribe(t, ts, "alice", firefoxUA, expired, "")
	expectUIStatus(t, "re-subscribe expired", resp, body, http.StatusOK)
	if !strings.Contains(body, `"gone":true`) {
		t.Fatalf("re-subscribing an expired device answered %s", body)
	}

	// An expired subscription is skipped until the device renews it...
	before := len(fake.received())
	resp, body = uiDo(t, ts, "alice", "POST", "/~/push/test", url.Values{})
	expectUIStatus(t, "test all again", resp, body, http.StatusOK)
	for _, p := range fake.received()[before:] {
		if p.path == "/expired" {
			t.Fatal("pushed to an expired subscription")
		}
	}
	// ...which replaces the old one.
	renewed := fake.device("/renewed")
	resp, body = subscribe(t, ts, "alice", firefoxUA, renewed, expired.endpoint)
	expectUIStatus(t, "renew", resp, body, http.StatusOK)
	if !strings.Contains(body, `"gone":false`) {
		t.Fatalf("renewing answered %s", body)
	}
	if n := len(deviceIDs(t, ts, "alice")); n != 5 {
		t.Fatalf("%d devices after renewing, want 5", n)
	}
	if strings.Contains(devicesHTML(t, ts, "alice"), "E-PUSH-GONE") {
		t.Fatal("the expired device is still listed")
	}

	// A device that works again clears its error.
	fake.set("/refused", 0, "")
	resp, body = uiDo(t, ts, "alice", "POST", "/~/push/test", url.Values{})
	expectUIStatus(t, "test after fix", resp, body, http.StatusOK)
	if strings.Contains(body, "BadJwtToken") {
		t.Fatalf("error kept after a successful push:\n%s", body)
	}
}

func TestPushSubscribeValidation(t *testing.T) {
	_, ts, fake := newPushTestServer(t)
	d := fake.device("/d")
	plain := d
	plain.endpoint = strings.Replace(d.endpoint, "https://", "http://", 1)
	resp, body := subscribe(t, ts, "alice", firefoxUA, plain, "")
	expectUIStatus(t, "http endpoint", resp, body, http.StatusUnprocessableEntity)
	if !strings.Contains(body, "E-PUSH-SUB-URL") || resp.Header.Get("HX-Retarget") != "#error" {
		t.Fatalf("http endpoint: %s", body)
	}
	// The server POSTs to endpoints, so only push services are allowed.
	for _, endpoint := range []string{"https://localhost:8443/admin", "https://169.254.169.254/latest", "https://evil127.0.0.1.example/x"} {
		other := d
		other.endpoint = endpoint
		resp, body = subscribe(t, ts, "alice", firefoxUA, other, "")
		expectUIStatus(t, endpoint, resp, body, http.StatusUnprocessableEntity)
		if !strings.Contains(body, "E-PUSH-SUB-HOST") {
			t.Fatalf("%s: %s", endpoint, body)
		}
	}
	resp, body = rawSubscribe(t, ts, "alice", firefoxUA, `{"subscription":{"endpoint":"`+d.endpoint+`","keys":{"p256dh":"AAAA","auth":"AAAA"}}}`)
	expectUIStatus(t, "bad keys", resp, body, http.StatusUnprocessableEntity)
	if !strings.Contains(body, "E-PUSH-SUB-KEYS") {
		t.Fatalf("bad keys: %s", body)
	}
	resp, body = rawSubscribe(t, ts, "alice", firefoxUA, `{`)
	expectUIStatus(t, "bad json", resp, body, http.StatusBadRequest)
	if !strings.Contains(body, "E-PUSH-SUB-JSON") {
		t.Fatalf("bad json: %s", body)
	}
	resp, body = subscribe(t, ts, "alice", firefoxUA, d, "")
	expectUIStatus(t, "valid", resp, body, http.StatusOK)
	resp, body = uiDo(t, ts, "alice", "DELETE", "/~/push/devices?id="+deviceIDs(t, ts, "alice")[0], nil)
	expectUIStatus(t, "remove", resp, body, http.StatusOK)
	resp, body = rawSubscribe(t, ts, "alice", firefoxUA, `{"subscription":{"endpoint":"`+d.endpoint+`"}}`, "Sec-Fetch-Site", "cross-site")
	expectUIStatus(t, "cross-site", resp, body, http.StatusForbidden)
	if len(deviceIDs(t, ts, "alice")) != 0 {
		t.Fatal("an invalid subscription was saved")
	}
}

func TestKnownPushHosts(t *testing.T) {
	for host, want := range map[string]bool{
		"fcm.googleapis.com": true, "web.push.apple.com": true, "updates.push.services.mozilla.com": true, "wns2-par02p.notify.windows.com": true,
		"fcm.googleapis.com.evil.com": false, "evilpush.apple.com": false, "localhost": false, "169.254.169.254": false, "": false,
	} {
		if got := knownPushHost(pushServices, host); got != want {
			t.Errorf("%q: %v, want %v", host, got, want)
		}
	}
}

func TestPushClientErrorsAreLogged(t *testing.T) {
	_, ts, _ := newPushTestServer(t)
	logs := &lockedBuffer{}
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	resp, body := uiDo(t, ts, "alice", "POST", "/~/push/log", url.Values{"code": {"E-PUSH-SUB"}, "message": {"AbortError: denied\nfake line"}})
	expectUIStatus(t, "log", resp, body, http.StatusNoContent)
	resp, body = uiDo(t, ts, "alice", "POST", "/~/push/log", url.Values{"code": {"E-DB-FAKE"}, "message": {"x"}})
	expectUIStatus(t, "log bad code", resp, body, http.StatusNoContent)
	got := logs.String()
	if !strings.Contains(got, `E-PUSH-SUB on alice's`) || !strings.Contains(got, `"AbortError: denied\nfake line"`) || !strings.Contains(got, "E-PUSH-CLIENT-UNKNOWN") || strings.Contains(got, "E-DB-FAKE") {
		t.Fatalf("log:\n%s", got)
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestNotifyAPI(t *testing.T) {
	_, ts, fake := newPushTestServer(t)
	status, reply := api(t, ts, "alice", "application/x-www-form-urlencoded", "title=Hi")
	if status != http.StatusConflict || reply["error"] != "E-API-NO-DEVICES" {
		t.Fatalf("no devices: %d %v", status, reply)
	}

	phone, laptop, bobs := fake.device("/phone"), fake.device("/laptop"), fake.device("/bob")
	for user, d := range map[string]fakeDevice{"alice": phone, "bob": bobs} {
		resp, body := subscribe(t, ts, user, iPhoneUA, d, "")
		expectUIStatus(t, "subscribe", resp, body, http.StatusOK)
	}
	resp, body := subscribe(t, ts, "alice", firefoxUA, laptop, "")
	expectUIStatus(t, "subscribe", resp, body, http.StatusOK)

	status, reply = api(t, ts, "alice", "application/x-www-form-urlencoded", url.Values{"title": {"Backup done"}, "body": {"42 GB"}}.Encode())
	if status != http.StatusOK || reply["sent"] != 2.0 || reply["failed"] != 0.0 {
		t.Fatalf("form: %d %v", status, reply)
	}
	got := fake.received()
	if len(got) != 2 {
		t.Fatalf("received %+v", got)
	}
	for _, p := range got {
		if p.path == "/bob" || p.msg.Title != "Backup done" || p.msg.Body != "42 GB" || p.msg.ID == "" {
			t.Fatalf("received %+v", p)
		}
	}

	status, reply = api(t, ts, "alice", "application/json", `{"title":"Door","body":"Someone rang","url":"https://example.com/cam","id":"spoofed"}`)
	if status != http.StatusOK || reply["sent"] != 2.0 {
		t.Fatalf("json: %d %v", status, reply)
	}
	if last := fake.received()[len(fake.received())-1].msg; last.URL != "https://example.com/cam" || last.ID == "spoofed" {
		t.Fatalf("json message arrived as %+v", last)
	}

	fake.set("/laptop", http.StatusGone, "")
	status, reply = api(t, ts, "alice", "application/json", `{"title":"Half"}`)
	if status != http.StatusOK || reply["sent"] != 1.0 || reply["failed"] != 1.0 || !strings.Contains(fmt.Sprint(reply["devices"]), "E-PUSH-GONE") {
		t.Fatalf("one failing: %d %v", status, reply)
	}
	fake.set("/phone", http.StatusInternalServerError, "down")
	status, reply = api(t, ts, "alice", "application/json", `{"title":"None"}`)
	if status != http.StatusBadGateway || reply["sent"] != 0.0 || strings.Contains(fmt.Sprint(reply["devices"]), "down") {
		t.Fatalf("all failing: %d %v", status, reply)
	}

	for _, c := range []struct{ body, code string }{
		{`{"title":"  "}`, "E-API-TITLE"},
		{`{"title":"x","url":"javascript:alert(1)"}`, "E-API-URL"},
		{`{"title":"x","body":"` + strings.Repeat("a", 4000) + `"}`, "E-API-TOO-LARGE"},
		{`{"title":`, "E-API-JSON"},
	} {
		if _, reply := api(t, ts, "alice", "application/json", c.body); reply["error"] != c.code {
			t.Fatalf("%s: %v, want %s", c.body, reply, c.code)
		}
	}
	if status, reply := api(t, ts, "alice", "application/json", `{"title":"x"}`, "Sec-Fetch-Site", "cross-site"); status != http.StatusForbidden || reply["error"] != "E-UI-CROSS-ORIGIN" {
		t.Fatalf("cross-site API call: %d %v", status, reply)
	}
}

func TestEventAlarms(t *testing.T) {
	s, ts, fake := newPushTestServer(t)
	resp, body := subscribe(t, ts, "alice", iPhoneUA, fake.device("/phone"), "")
	expectUIStatus(t, "subscribe", resp, body, http.StatusOK)
	ics := func(props string, alarm string) string {
		if alarm != "" {
			alarm = "BEGIN:VALARM\r\nACTION:DISPLAY\r\n" + alarm + "\r\nEND:VALARM\r\n"
		}
		return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:x\r\n" + props + "\r\n" + alarm + "END:VEVENT\r\nEND:VCALENDAR\r\n"
	}
	put := func(user, path, data string) {
		t.Helper()
		got := do(t, ts, user, "PUT", path, data)
		if got.status != http.StatusCreated && got.status != http.StatusNoContent {
			t.Fatalf("put %s: %d %s", path, got.status, got.body)
		}
	}
	expectStatus(t, "mkcalendar", do(t, ts, "alice", "MKCALENDAR", "/alice/work/", ""), http.StatusCreated)
	put("alice", "/alice/calendar/reminded.ics", ics("SUMMARY:Dentist\r\nDTSTART:20990304T100000\r\nDTEND:20990304T110000", "TRIGGER:-PT15M"))
	put("alice", "/alice/work/plain.ics", ics("SUMMARY:Standup\r\nDTSTART:20990304T103000", ""))
	put("alice", "/alice/calendar/allday.ics", ics("SUMMARY:Holiday\r\nDTSTART;VALUE=DATE:20990305", ""))
	put("alice", "/alice/calendar/absolute.ics", ics("SUMMARY:Call\r\nDTSTART:20990306T100000Z", "TRIGGER;VALUE=DATE-TIME:20990306T080000Z"))
	put("bob", "/bob/calendar/bobs.ics", ics("SUMMARY:Bob's\r\nDTSTART:20990304T100000", ""))

	at := func(day, hour, min int) time.Time { return time.Date(2099, 3, day, hour, min, 0, 0, time.Local) }
	sent := func() []string {
		var titles []string
		for _, p := range fake.received() {
			titles = append(titles, p.msg.Title+" | "+p.msg.Body)
		}
		return titles
	}
	check := func(now time.Time, want ...string) {
		t.Helper()
		before := len(fake.received())
		s.notifyDue(context.Background(), now)
		got := sent()[before:]
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("at %s sent %q, want %q", now.Format(time.Stamp), got, want)
		}
	}
	check(at(4, 9, 40))
	check(at(4, 9, 45), "Dentist | Today · 10:00–11:00")
	check(at(4, 9, 46)) // only once
	check(at(4, 10, 30), "Standup | Today · 10:30")
	check(at(4, 10, 39))
	check(at(5, 9, 0), "Holiday | Today · All day")
	check(time.Date(2099, 3, 6, 8, 0, 30, 0, time.UTC), "Call | Today · 10:00 · UTC")

	// A moved event alarms again; one that was missed by more than the
	// catch-up window doesn't.
	put("alice", "/alice/calendar/reminded.ics", ics("SUMMARY:Dentist\r\nDTSTART:20990310T100000", "TRIGGER:-PT15M"))
	check(at(10, 9, 50), "Dentist | Today · 10:00")
	put("alice", "/alice/calendar/late.ics", ics("SUMMARY:Late\r\nDTSTART:20990311T100000", ""))
	check(at(11, 10, 11))
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"-PT15M": -15 * time.Minute, "PT0S": 0, "P1D": 24 * time.Hour, "-P1DT2H30M": -(26*time.Hour + 30*time.Minute),
		"+PT1H": time.Hour, "-P1W": -7 * 24 * time.Hour,
	} {
		if got, ok := parseDuration(in); !ok || got != want {
			t.Errorf("%s: %v %v, want %v", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "P", "-PT", "PT15", "15M", "PTXM"} {
		if _, ok := parseDuration(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
	ev := parseEvent([]byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nDTSTART:20990304T100000\r\nDTEND:20990304T110000\r\n" +
		"BEGIN:VALARM\r\nTRIGGER;RELATED=END:-PT5M\r\nEND:VALARM\r\nBEGIN:VALARM\r\nTRIGGER:nonsense\r\nEND:VALARM\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"))
	if got := ev.alarms(); len(got) != 1 || !got[0].Equal(time.Date(2099, 3, 4, 10, 55, 0, 0, time.Local)) {
		t.Fatalf("alarms %v", got)
	}
}
