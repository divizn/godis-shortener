package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testBase = "http://short.test"

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type env struct {
	a    *app
	kv   *fakeKV
	clk  *clock
	h    http.Handler
	logs *syncBuf
}

func newEnv(t *testing.T, mods ...func(*config)) *env {
	t.Helper()
	clk := newClock()
	kv := newFakeKV(clk.Now)
	cfg := config{baseURL: testBase, createLimit: 20}
	for _, m := range mods {
		m(&cfg)
	}
	logs := &syncBuf{}
	a, err := newApp(cfg, kv, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	a.now = clk.Now
	return &env{a: a, kv: kv, clk: clk, h: a.routes(), logs: logs}
}

func (e *env) do(method, path string, body string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = "short.test"
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i] == "Host" {
			r.Host = hdr[i+1]
			continue
		}
		r.Header.Set(hdr[i], hdr[i+1])
	}
	if method == http.MethodPost && r.Header.Get("Content-Type") == "" {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

type created struct {
	Code      string `json:"code"`
	ShortURL  string `json:"short_url"`
	ManageURL string `json:"manage_url"`
	ExpiresAt string `json:"expires_at"`
}

func (e *env) create(t *testing.T, target, expires string, hdr ...string) created {
	t.Helper()
	w := e.createRaw(target, expires, hdr...)
	if w.Code != http.StatusCreated {
		t.Fatalf("create %s: status %d: %s", target, w.Code, w.Body)
	}
	var c created
	if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
		t.Fatalf("decode create response %q: %v", w.Body, err)
	}
	return c
}

func (e *env) createRaw(target, expires string, hdr ...string) *httptest.ResponseRecorder {
	b, _ := json.Marshal(map[string]string{"url": target, "expires_in": expires})
	return e.do(http.MethodPost, "/links", string(b), append([]string{"Content-Type", "application/json"}, hdr...)...)
}

func managePath(c created) string { return strings.TrimPrefix(c.ManageURL, testBase) }

func ctxT() context.Context { return context.Background() }

type scriptedCodes struct {
	codes []string
	i     int
}

func (s *scriptedCodes) next() (string, error) {
	c := s.codes[min(s.i, len(s.codes)-1)]
	s.i++
	return c, nil
}

func TestClaimCollisionRetriesAndNeverOverwrites(t *testing.T) {
	e := newEnv(t)
	existing := []byte(`{"url":"https://old.example/","created":1,"secret":"x"}`)
	e.kv.put("link/AAAAAAA", existing)
	sc := &scriptedCodes{codes: []string{"AAAAAAA", "AAAAAAA", "BBBBBBB"}}
	e.a.codes = sc.next

	c := e.create(t, "https://new.example/", "")
	if c.Code != "BBBBBBB" {
		t.Fatalf("code = %q, want BBBBBBB", c.Code)
	}
	got, _, _ := e.kv.Get(ctxT(), "link/AAAAAAA")
	if !bytes.Equal(got, existing) {
		t.Fatalf("existing link was overwritten: %s", got)
	}
}

func TestClaimGivesUpAfterFiveCollisions(t *testing.T) {
	e := newEnv(t)
	existing := []byte(`{"url":"https://old.example/","created":1,"secret":"x"}`)
	e.kv.put("link/AAAAAAA", existing)
	sc := &scriptedCodes{codes: []string{"AAAAAAA"}}
	e.a.codes = sc.next

	w := e.createRaw("https://new.example/", "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", w.Code)
	}
	if sc.i != 5 {
		t.Fatalf("drew %d codes, want 5", sc.i)
	}
	got, _, _ := e.kv.Get(ctxT(), "link/AAAAAAA")
	if !bytes.Equal(got, existing) {
		t.Fatalf("existing link was overwritten: %s", got)
	}
}

func TestRandomCodeRejectsBiasedBytes(t *testing.T) {
	src := bytes.NewReader([]byte{255, 248, 250, 0, 1, 2, 3, 4, 5, 6})
	got, err := randomCode(src)
	if err != nil {
		t.Fatal(err)
	}
	if got != "ABCDEFG" {
		t.Fatalf("code = %q, want ABCDEFG (bytes at or over 248 must be skipped)", got)
	}
}

func TestExpiringLinkAndCountersAllExpire(t *testing.T) {
	e := newEnv(t)
	c := e.create(t, "https://example.com/page", "1h")
	if c.ExpiresAt == "" {
		t.Fatal("expires_at missing")
	}
	e.do(http.MethodGet, "/"+c.Code, "")
	if w := e.do(http.MethodPost, "/"+c.Code+"/report", ""); w.Code != http.StatusOK {
		t.Fatalf("report status %d", w.Code)
	}
	for _, k := range []string{"link/", "clicks/", "reports/"} {
		if len(e.kv.keys(k)) != 1 {
			t.Fatalf("no key with prefix %s before expiry", k)
		}
	}
	e.clk.Add(time.Hour + time.Second)
	for _, k := range []string{"link/", "clicks/", "reports/"} {
		if got := e.kv.keys(k); len(got) != 0 {
			t.Fatalf("keys with prefix %s outlive the link: %v", k, got)
		}
	}
	if w := e.do(http.MethodGet, "/"+c.Code, ""); w.Code != http.StatusNotFound {
		t.Fatalf("expired link status %d, want 404", w.Code)
	}
}

func TestExpireFailureDeletesLinkAnd503(t *testing.T) {
	e := newEnv(t)
	e.kv.expireErr = func(key string) error {
		if strings.HasPrefix(key, "link/") {
			return errors.New("boom")
		}
		return nil
	}
	w := e.createRaw("https://example.com/", "1h")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", w.Code)
	}
	for _, p := range []string{"link/", "clicks/", "reports/"} {
		if got := e.kv.keys(p); len(got) != 0 {
			t.Fatalf("immortal key left behind: %v", got)
		}
	}
}

func TestCreateStoresOnlyHashOfSecret(t *testing.T) {
	e := newEnv(t)
	c := e.create(t, "https://example.com/a", "")
	secret := strings.TrimPrefix(managePath(c), "/m/"+c.Code+"/")
	if len(secret) < 20 {
		t.Fatalf("secret %q is short", secret)
	}
	raw, _, _ := e.kv.Get(ctxT(), "link/"+c.Code)
	if bytes.Contains(raw, []byte(secret)) {
		t.Fatalf("stored record holds the plaintext secret: %s", raw)
	}
}

func TestCreateFormAnswersHTMLWithBothURLs(t *testing.T) {
	e := newEnv(t)
	w := e.do(http.MethodPost, "/links", url.Values{"url": {"https://example.com/x"}, "expires_in": {"24h"}}.Encode())
	if w.Code != http.StatusCreated || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("status %d type %q", w.Code, w.Header().Get("Content-Type"))
	}
	body := w.Body.String()
	if !strings.Contains(body, testBase+"/m/") || strings.Count(body, testBase+"/") < 2 {
		t.Fatalf("page lacks short and management urls: %s", body)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("create result must not be cached, it holds the management secret")
	}
}

func TestCreateRejectsBadExpiry(t *testing.T) {
	e := newEnv(t)
	for _, in := range []string{"-1h", "0s", "soon", "100000h"} {
		if w := e.createRaw("https://example.com/", in); w.Code != http.StatusBadRequest {
			t.Errorf("expires_in %q: status %d, want 400", in, w.Code)
		}
	}
}

func TestWrongSecretLooksLikeUnknownCode(t *testing.T) {
	e := newEnv(t)
	c := e.create(t, "https://example.com/", "")
	unknown := "/m/ZZZZZZZ/" + strings.Repeat("a", 22)
	wrong := "/m/" + c.Code + "/" + strings.Repeat("a", 22)

	a, b := e.do(http.MethodGet, unknown, ""), e.do(http.MethodGet, wrong, "")
	if a.Code != http.StatusNotFound || b.Code != a.Code || a.Body.String() != b.Body.String() {
		t.Fatalf("unknown %d %q vs wrong secret %d %q", a.Code, a.Body, b.Code, b.Body)
	}
	a, b = e.do(http.MethodPost, unknown+"/delete", ""), e.do(http.MethodPost, wrong+"/delete", "")
	if a.Code != http.StatusNotFound || b.Code != a.Code || a.Body.String() != b.Body.String() {
		t.Fatalf("delete: unknown %d vs wrong secret %d", a.Code, b.Code)
	}
	if w := e.do(http.MethodGet, "/"+c.Code, ""); w.Code != http.StatusOK {
		t.Fatalf("a wrong secret deleted the link: status %d", w.Code)
	}
	if w := e.do(http.MethodGet, managePath(c), ""); w.Code != http.StatusOK {
		t.Fatalf("right secret status %d", w.Code)
	}
}

func TestRateLimit(t *testing.T) {
	e := newEnv(t, func(c *config) { c.createLimit = 3 })
	for i := range 3 {
		if w := e.createRaw(fmt.Sprintf("https://example.com/%d", i), ""); w.Code != http.StatusCreated {
			t.Fatalf("create %d: status %d", i, w.Code)
		}
	}
	w := e.createRaw("https://example.com/4", "")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("limit+1th create: status %d, want 429", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "1800" {
		t.Fatalf("Retry-After = %q, want 1800 (30 min left in the hour)", got)
	}
	other := e.createRaw("https://example.com/5", "", "X-Forwarded-For", "198.51.100.9")
	if other.Code != http.StatusTooManyRequests {
		t.Fatalf("trust-proxy is off, XFF must not matter: status %d", other.Code)
	}
	e.a.cfg.trustProxy = true
	if other = e.createRaw("https://example.com/5", "", "X-Forwarded-For", "198.51.100.9"); other.Code != http.StatusCreated {
		t.Fatalf("another ip shares the limit: status %d", other.Code)
	}
	e.a.cfg.trustProxy = false

	c := e.kv.keys("link/")
	r := e.do(http.MethodPost, "/"+strings.TrimPrefix(c[0], "link/")+"/report", "")
	if r.Code != http.StatusOK {
		t.Fatalf("report shares the create budget: status %d", r.Code)
	}

	e.clk.Add(30 * time.Minute)
	if w := e.createRaw("https://example.com/6", ""); w.Code != http.StatusCreated {
		t.Fatalf("new window still limited: status %d", w.Code)
	}
	if got := e.kv.keys("ratelimit/create/192.0.2.1/"); len(got) != 1 {
		t.Fatalf("window keys must expire with their window, have %v", got)
	}
}

func TestReportRateLimited(t *testing.T) {
	e := newEnv(t, func(c *config) { c.createLimit = 2 })
	c := e.create(t, "https://example.com/", "")
	for range 2 {
		if w := e.do(http.MethodPost, "/"+c.Code+"/report", ""); w.Code != http.StatusOK {
			t.Fatalf("report status %d", w.Code)
		}
	}
	if w := e.do(http.MethodPost, "/"+c.Code+"/report", ""); w.Code != http.StatusTooManyRequests {
		t.Fatalf("third report status %d, want 429", w.Code)
	}
}

func TestTrustProxy(t *testing.T) {
	cases := []struct {
		name  string
		trust bool
		xff   string
		want  string
	}{
		{"off ignores header", false, "198.51.100.1", "192.0.2.1"},
		{"on takes first hop", true, "198.51.100.1, 203.0.113.7", "198.51.100.1"},
		{"on garbage falls back", true, "x/../y", "192.0.2.1"},
		{"on no header", true, "", "192.0.2.1"},
	}
	for _, tc := range cases {
		e := newEnv(t, func(c *config) { c.trustProxy = tc.trust })
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if tc.xff != "" {
			r.Header.Set("X-Forwarded-For", tc.xff)
		}
		if got := e.a.clientIP(r); got != tc.want {
			t.Errorf("%s: ip = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestValidateURL(t *testing.T) {
	self, _ := url.Parse("https://short.example")
	long := "https://example.com/" + strings.Repeat("a", 2040)
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"https://example.com/a?b=c#d", "https://example.com/a?b=c#d", true},
		{"HTTP://Example.COM/x", "http://example.com/x", true},
		{"ftp://example.com/", "", false},
		{"javascript:alert(1)", "", false},
		{"data:text/html,hi", "", false},
		{"//example.com/x", "", false},
		{"/relative", "", false},
		{"https://", "", false},
		{"https://user@example.com/", "", false},
		{"https://user:pw@example.com/", "", false},
		{"https://short.example/abc", "", false},
		{"https://SHORT.example:443/abc", "", false},
		{"https://short.example./abc", "", false},
		{"https://short.example:8443/abc", "https://short.example:8443/abc", true},
		{"https://exa mple.com/", "", false},
		{"https://example.com/\x00", "", false},
		{long[:2048], long[:2048], true},
		{long[:2048] + "a", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, err := validateURL(tc.in, self)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("validateURL(%.60q) = %.60q, %v; want %.60q ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
	}
}

func TestSafeBrowsing(t *testing.T) {
	var hits atomic.Int32
	var gotKey, gotBody atomic.Value
	status, reply := http.StatusOK, `{}`
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		gotKey.Store(r.URL.Query().Get("key"))
		b := new(bytes.Buffer)
		b.ReadFrom(r.Body)
		gotBody.Store(b.String())
		mu.Lock()
		defer mu.Unlock()
		w.WriteHeader(status)
		fmt.Fprint(w, reply)
	}))
	defer srv.Close()
	set := func(s int, r string) { mu.Lock(); status, reply = s, r; mu.Unlock() }
	with := func(c *config) { c.sbKey, c.sbEndpoint = "k3y", srv.URL }

	t.Run("match is 403", func(t *testing.T) {
		e := newEnv(t, with)
		set(200, `{"matches":[{"threatType":"MALWARE"}]}`)
		w := e.createRaw("https://bad.example/x", "")
		if w.Code != http.StatusForbidden {
			t.Fatalf("status %d, want 403", w.Code)
		}
		if got := e.kv.keys("link/"); len(got) != 0 {
			t.Fatalf("flagged url was stored: %v", got)
		}
		if gotKey.Load() != "k3y" || !strings.Contains(gotBody.Load().(string), "https://bad.example/x") ||
			!strings.Contains(gotBody.Load().(string), "SOCIAL_ENGINEERING") {
			t.Fatalf("bad lookup request: key %v body %v", gotKey.Load(), gotBody.Load())
		}
	})
	t.Run("clean passes", func(t *testing.T) {
		e := newEnv(t, with)
		set(200, `{}`)
		if w := e.createRaw("https://fine.example/", ""); w.Code != http.StatusCreated {
			t.Fatalf("status %d", w.Code)
		}
	})
	t.Run("api error fails closed", func(t *testing.T) {
		e := newEnv(t, with)
		set(500, `oops`)
		w := e.createRaw("https://fine.example/", "")
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status %d, want 503", w.Code)
		}
		if got := e.kv.keys("link/"); len(got) != 0 {
			t.Fatalf("link stored while the check was down: %v", got)
		}
	})
	t.Run("unreachable fails closed", func(t *testing.T) {
		e := newEnv(t, func(c *config) { c.sbKey, c.sbEndpoint = "k3y", "http://127.0.0.1:1" })
		if w := e.createRaw("https://fine.example/", ""); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status %d, want 503", w.Code)
		}
		if logs := e.logs.String(); !strings.Contains(logs, "safe browsing lookup") || strings.Contains(logs, "k3y") {
			t.Fatalf("the failure must be logged without the api key: %s", logs)
		}
	})
	t.Run("no key skips the call", func(t *testing.T) {
		before := hits.Load()
		e := newEnv(t, func(c *config) { c.sbEndpoint = srv.URL })
		set(200, `{"matches":[{}]}`)
		if w := e.createRaw("https://fine.example/", ""); w.Code != http.StatusCreated {
			t.Fatalf("status %d", w.Code)
		}
		if hits.Load() != before {
			t.Fatal("safe browsing was called without a key")
		}
	})
}

func TestTakedown(t *testing.T) {
	e := newEnv(t, func(c *config) { c.adminToken = "tok" })
	c := e.create(t, "https://example.com/", "1h")
	auth := []string{"Authorization", basic("tok")}
	if w := e.do(http.MethodPost, "/admin/takedown/"+c.Code, "", auth...); w.Code != http.StatusSeeOther {
		t.Fatalf("takedown status %d", w.Code)
	}
	if w := e.do(http.MethodGet, "/"+c.Code, ""); w.Code != http.StatusGone {
		t.Fatalf("status %d after takedown, want 410", w.Code)
	}
	if w := e.do(http.MethodPost, "/"+c.Code+"/report", ""); w.Code != http.StatusGone {
		t.Fatalf("report of a taken down link: status %d, want 410", w.Code)
	}
	e.clk.Add(2 * time.Hour)
	if w := e.do(http.MethodGet, "/"+c.Code, ""); w.Code != http.StatusGone {
		t.Fatalf("status %d after the old deadline, want 410 (the takedown must not lapse)", w.Code)
	}
	if w := e.do(http.MethodPost, "/admin/takedown/AAAAAAA", "", auth...); w.Code != http.StatusNotFound {
		t.Fatalf("takedown of an unknown code: status %d, want 404", w.Code)
	}
}

func TestConcurrentReportsAllLandInIndex(t *testing.T) {
	e := newEnv(t, func(c *config) { c.createLimit = 1000 })
	var links []created
	for i := range 10 {
		links = append(links, e.create(t, fmt.Sprintf("https://example.com/%d", i), ""))
	}
	var wg sync.WaitGroup
	for i := range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := httptest.NewRequest(http.MethodPost, "/"+links[i%10].Code+"/report", nil)
			r.Host = "short.test"
			r.RemoteAddr = fmt.Sprintf("192.0.2.%d:1", i+1)
			w := httptest.NewRecorder()
			e.h.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Errorf("report %d: status %d", i, w.Code)
			}
		}()
	}
	wg.Wait()

	codes, err := e.a.reportedCodes(ctxT())
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, c := range codes {
		if seen[c] {
			t.Errorf("code %s listed twice", c)
		}
		seen[c] = true
	}
	for _, l := range links {
		if !seen[l.Code] {
			t.Errorf("reported link %s missing from the index %v", l.Code, codes)
		}
	}
	var total int64
	for _, l := range links {
		n, err := e.a.counter(ctxT(), "reports/"+l.Code)
		if err != nil {
			t.Fatal(err)
		}
		total += n
	}
	if total != 30 {
		t.Fatalf("report count %d, want 30", total)
	}
}

func TestReportedIndexIsCappedNewestFirstWithoutDuplicates(t *testing.T) {
	e := newEnv(t)
	for i := range maxReported + 5 {
		if err := e.a.addReported(ctxT(), fmt.Sprintf("c%06d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.a.addReported(ctxT(), fmt.Sprintf("c%06d", maxReported+4)); err != nil {
		t.Fatal(err)
	}
	codes, err := e.a.reportedCodes(ctxT())
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != maxReported {
		t.Fatalf("index holds %d codes, want %d", len(codes), maxReported)
	}
	if codes[0] != fmt.Sprintf("c%06d", maxReported+4) || codes[1] != fmt.Sprintf("c%06d", maxReported+3) {
		t.Fatalf("not newest first: %v", codes[:3])
	}
	if codes[len(codes)-1] != "c000005" {
		t.Fatalf("oldest kept = %s, want c000005", codes[len(codes)-1])
	}
}

func TestAdmin(t *testing.T) {
	t.Run("no token configured is 404", func(t *testing.T) {
		e := newEnv(t)
		for _, req := range [][2]string{{"GET", "/admin"}, {"POST", "/admin/takedown/AAAAAAA"}} {
			w := e.do(req[0], req[1], "", "Authorization", basic(""))
			if w.Code != http.StatusNotFound {
				t.Errorf("%s %s: status %d, want 404", req[0], req[1], w.Code)
			}
		}
	})
	t.Run("credentials", func(t *testing.T) {
		e := newEnv(t, func(c *config) { c.adminToken = "tok" })
		c := e.create(t, "https://example.com/reported", "")
		e.do(http.MethodPost, "/"+c.Code+"/report", "")

		if w := e.do(http.MethodGet, "/admin", ""); w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") == "" {
			t.Fatalf("no credentials: status %d", w.Code)
		}
		if w := e.do(http.MethodGet, "/admin", "", "Authorization", basic("nope")); w.Code != http.StatusUnauthorized {
			t.Fatalf("wrong password: status %d", w.Code)
		}
		if w := e.do(http.MethodGet, "/admin", "", "Authorization", basic("toktok")); w.Code != http.StatusUnauthorized {
			t.Fatalf("longer password: status %d", w.Code)
		}
		if w := e.do(http.MethodPost, "/admin/takedown/"+c.Code, "", "Authorization", basic("nope")); w.Code != http.StatusUnauthorized {
			t.Fatalf("takedown with wrong password: status %d", w.Code)
		}
		if w := e.do(http.MethodGet, "/"+c.Code, ""); w.Code != http.StatusOK {
			t.Fatalf("unauthorized takedown took the link down: status %d", w.Code)
		}
		w := e.do(http.MethodGet, "/admin", "", "Authorization", basic("tok"))
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), c.Code) || !strings.Contains(w.Body.String(), "https://example.com/reported") {
			t.Fatalf("admin page: status %d body %s", w.Code, w.Body)
		}
	})
}

func basic(pw string) string {
	r, _ := http.NewRequest("GET", "/", nil)
	r.SetBasicAuth("anyone", pw)
	return r.Header.Get("Authorization")
}

func TestCrossOriginPostRefused(t *testing.T) {
	e := newEnv(t)
	form := url.Values{"url": {"https://example.com/"}}.Encode()
	for _, h := range [][]string{
		{"Sec-Fetch-Site", "cross-site"},
		{"Origin", "http://evil.test"},
	} {
		if w := e.do(http.MethodPost, "/links", form, h...); w.Code != http.StatusForbidden {
			t.Errorf("%v: status %d, want 403", h, w.Code)
		}
	}
	if got := e.kv.keys("link/"); len(got) != 0 {
		t.Fatalf("cross-origin post created %v", got)
	}
	c := e.create(t, "https://example.com/", "")
	if w := e.do(http.MethodPost, managePath(c)+"/delete", "", "Sec-Fetch-Site", "cross-site"); w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin delete: status %d", w.Code)
	}
	if w := e.do(http.MethodPost, "/links", form, "Origin", testBase); w.Code != http.StatusCreated {
		t.Fatalf("same-origin post: status %d", w.Code)
	}
}

func TestPreviewEscapesHostileURL(t *testing.T) {
	e := newEnv(t)
	hostile := `https://example.com/?q="><script>alert(1)</script>&r='onmouseover='x`
	raw, _ := json.Marshal(link{URL: hostile, Created: 1, Secret: "x"})
	e.kv.put("link/ABCDEFG", raw)
	for _, p := range []string{"/ABCDEFG"} {
		w := e.do(http.MethodGet, p, "")
		body := w.Body.String()
		if w.Code != http.StatusOK || strings.Contains(body, "<script") || strings.Contains(body, "'onmouseover=") || strings.Contains(body, `"><`) {
			t.Fatalf("hostile url reached the page unescaped: %d %s", w.Code, body)
		}
	}
}

func TestPreviewShowsFullDestinationHostAndSafeLink(t *testing.T) {
	e := newEnv(t)
	c := e.create(t, "https://sub.example.com/a/b?c=d", "")
	body := e.do(http.MethodGet, "/"+c.Code, "").Body.String()
	for _, want := range []string{"https://sub.example.com/a/b?c=d", "<strong>sub.example.com</strong>", `rel="noreferrer noopener"`, "/" + c.Code + "/report"} {
		if !strings.Contains(body, want) {
			t.Errorf("preview lacks %q:\n%s", want, body)
		}
	}
}

func TestUnknownAndMalformedCodes(t *testing.T) {
	e := newEnv(t)
	if w := e.do(http.MethodGet, "/AAAAAAA", ""); w.Code != http.StatusNotFound {
		t.Fatalf("unknown code: %d", w.Code)
	}
	e.kv.failAll = errors.New("down")
	if w := e.do(http.MethodGet, "/not-a-code", ""); w.Code != http.StatusNotFound {
		t.Fatalf("malformed code must not reach godis: %d", w.Code)
	}
}

func TestGodisDownIs503NotNotFound(t *testing.T) {
	e := newEnv(t)
	c := e.create(t, "https://example.com/", "")
	e.kv.failAll = errors.New("down")
	for _, req := range [][2]string{
		{"GET", "/" + c.Code}, {"GET", managePath(c)}, {"POST", managePath(c) + "/delete"},
		{"POST", "/" + c.Code + "/report"}, {"GET", "/healthz"},
	} {
		if w := e.do(req[0], req[1], ""); w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s with godis down: status %d, want 503", req[0], req[1], w.Code)
		}
	}
	if w := e.createRaw("https://example.com/2", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("create with godis down: status %d", w.Code)
	}
}

func TestHealthz(t *testing.T) {
	e := newEnv(t)
	if w := e.do(http.MethodGet, "/healthz", ""); w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
}

func TestBodyOver8KiBRefused(t *testing.T) {
	e := newEnv(t)
	w := e.do(http.MethodPost, "/links", "url="+url.QueryEscape("https://example.com/"+strings.Repeat("a", 9000)))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413", w.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/", "/AAAAAAA", "/static/style.css"} {
		h := e.do(http.MethodGet, p, "").Header()
		if !strings.Contains(h.Get("Content-Security-Policy"), "default-src 'none'") ||
			h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s: missing security headers: %v", p, h)
		}
	}
}

func TestLifecycleOnFake(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewServer(e.a.routes())
	defer srv.Close()
	e.a.cfg.baseURL = srv.URL
	base, _ := url.Parse(srv.URL)
	e.a.base = base
	lifecycle(t, srv.URL)
	for _, p := range []string{"link/", "clicks/", "reports/"} {
		if got := e.kv.keys(p); len(got) != 0 {
			t.Fatalf("delete left %v behind", got)
		}
	}
}

// lifecycle drives create, visit, manage and delete over real HTTP
func lifecycle(t *testing.T, base string) {
	t.Helper()
	cl := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Timeout: 20 * time.Second}
	get := func(u string) (int, string) {
		resp, err := cl.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b := new(bytes.Buffer)
		b.ReadFrom(resp.Body)
		return resp.StatusCode, b.String()
	}
	post := func(u, ctype, body string) (int, string) {
		resp, err := cl.Post(u, ctype, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b := new(bytes.Buffer)
		b.ReadFrom(resp.Body)
		return resp.StatusCode, b.String()
	}

	dest := fmt.Sprintf("https://example.com/lifecycle/%d", time.Now().UnixNano())
	status, body := post(base+"/links", "application/json", fmt.Sprintf(`{"url":%q,"expires_in":"1h"}`, dest))
	if status != http.StatusCreated {
		t.Fatalf("create: %d %s", status, body)
	}
	var c created
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(c.ShortURL, base+"/") || !strings.HasPrefix(c.ManageURL, base+"/m/") {
		t.Fatalf("urls %+v do not start with %s", c, base)
	}
	for i := 1; i <= 2; i++ {
		if status, body = get(c.ShortURL); status != http.StatusOK || !strings.Contains(body, dest) {
			t.Fatalf("visit %d: %d %s", i, status, body)
		}
	}
	if status, body = get(c.ManageURL); status != http.StatusOK || !strings.Contains(body, `<dd id="clicks">2</dd>`) || !strings.Contains(body, dest) {
		t.Fatalf("manage: %d %s", status, body)
	}
	if status, _ = post(c.ShortURL+"/report", "application/x-www-form-urlencoded", ""); status != http.StatusOK {
		t.Fatalf("report: %d", status)
	}
	if status, body = get(c.ManageURL); !strings.Contains(body, `<dd id="reports">1</dd>`) {
		t.Fatalf("manage after report: %d %s", status, body)
	}
	if status, body = post(c.ManageURL+"/delete", "application/x-www-form-urlencoded", ""); status != http.StatusOK {
		t.Fatalf("delete: %d %s", status, body)
	}
	if status, _ = get(c.ShortURL); status != http.StatusNotFound {
		t.Fatalf("visit after delete: %d", status)
	}
	if status, _ = get(c.ManageURL); status != http.StatusNotFound {
		t.Fatalf("manage after delete: %d", status)
	}
}
