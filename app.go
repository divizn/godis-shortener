package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	requestTimeout = 3 * time.Second
	maxBody        = 8 << 10
)

//go:embed static/style.css
var styleCSS []byte

type app struct {
	cfg   config
	base  *url.URL
	kv    kv
	sb    *safeBrowsing
	log   *slog.Logger
	tmpl  *template.Template
	now   func() time.Time
	codes func() (string, error)
}

func newApp(cfg config, kv kv, log *slog.Logger) (*app, error) {
	base, err := url.Parse(cfg.baseURL)
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") {
		return nil, fmt.Errorf("base url %q must be an absolute http(s) url", cfg.baseURL)
	}
	tmpl, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	a := &app{cfg: cfg, base: base, kv: kv, log: log, tmpl: tmpl, now: time.Now, codes: newCode}
	if cfg.sbKey != "" {
		a.sb = newSafeBrowsing(cfg.sbEndpoint, cfg.sbKey)
	}
	return a, nil
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.index)
	mux.HandleFunc("GET /static/style.css", serveCSS)
	mux.HandleFunc("GET /healthz", a.healthz)
	mux.HandleFunc("POST /links", a.create)
	mux.HandleFunc("GET /{code}", a.preview)
	mux.HandleFunc("POST /{code}/report", a.report)
	mux.HandleFunc("GET /m/{code}/{secret}", a.manage)
	mux.HandleFunc("POST /m/{code}/{secret}/delete", a.remove)
	mux.HandleFunc("GET /admin", a.admin(a.adminPage))
	mux.HandleFunc("POST /admin/takedown/{code}", a.admin(a.adminTakedown))
	return secure(http.NewCrossOriginProtection().Handler(mux))
}

func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		next.ServeHTTP(w, r)
	})
}

func serveCSS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(styleCSS)
}

func (a *app) ctx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), requestTimeout)
}

func wantsJSON(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") || strings.Contains(r.Header.Get("Accept"), "application/json")
}

type page struct {
	Title string
	Msg   string
	Code  string
	URL   string
	Host  string
}

func (a *app) render(w http.ResponseWriter, status int, name string, data any) {
	var buf strings.Builder
	if err := a.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		a.log.Error("render", "template", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(buf.String()))
}

func (a *app) fail(w http.ResponseWriter, r *http.Request, status int, msg string) {
	if wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
		return
	}
	a.render(w, status, "error", page{Title: strconv.Itoa(status), Msg: msg})
}

func (a *app) unavailable(w http.ResponseWriter, r *http.Request, err error) {
	a.log.Error("backend", "method", r.Method, "route", r.Pattern, "err", err)
	a.fail(w, r, http.StatusServiceUnavailable, "the service is temporarily unavailable, try again shortly")
}

func (a *app) notFound(w http.ResponseWriter, r *http.Request) {
	a.fail(w, r, http.StatusNotFound, "no such link")
}

func (a *app) limited(w http.ResponseWriter, r *http.Request, kind string) bool {
	ctx, cancel := a.ctx(r)
	defer cancel()
	ok, retry, err := a.allow(ctx, kind, a.clientIP(r))
	if err != nil {
		a.unavailable(w, r, err)
		return true
	}
	if !ok {
		secs := int((retry + time.Second - 1) / time.Second)
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		a.fail(w, r, http.StatusTooManyRequests, "too many requests, try again later")
		return true
	}
	return false
}

func (a *app) index(w http.ResponseWriter, _ *http.Request) {
	a.render(w, http.StatusOK, "index", page{})
}

func (a *app) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	if err := a.kv.Ping(ctx); err != nil {
		a.unavailable(w, r, err)
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

const maxExpiry = 365 * 24 * time.Hour

func parseExpiry(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 || d > maxExpiry {
		return 0, errors.New("expires_in must be a duration between 1s and 8760h")
	}
	return d, nil
}

type createRequest struct {
	URL       string `json:"url"`
	ExpiresIn string `json:"expires_in"`
}

func readCreate(r *http.Request) (createRequest, error) {
	var req createRequest
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return req, err
		}
		return req, nil
	}
	if err := r.ParseForm(); err != nil {
		return req, err
	}
	return createRequest{URL: r.PostFormValue("url"), ExpiresIn: r.PostFormValue("expires_in")}, nil
}

func (a *app) create(w http.ResponseWriter, r *http.Request) {
	if a.limited(w, r, "create") {
		return
	}
	req, err := readCreate(r)
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		a.fail(w, r, http.StatusRequestEntityTooLarge, "request body is too large")
		return
	case err != nil:
		a.fail(w, r, http.StatusBadRequest, "could not read the request")
		return
	}
	target, err := validateURL(strings.TrimSpace(req.URL), a.base)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	ttl, err := parseExpiry(req.ExpiresIn)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if a.sb != nil {
		bad, err := a.sb.flagged(r.Context(), target)
		if err != nil {
			a.unavailable(w, r, err)
			return
		}
		if bad {
			a.fail(w, r, http.StatusForbidden, "this url is flagged as unsafe")
			return
		}
	}
	secret, hash, err := newSecret()
	if err != nil {
		a.unavailable(w, r, err)
		return
	}
	ctx, cancel := a.ctx(r)
	defer cancel()
	now := a.now()
	code, err := a.claim(ctx, link{URL: target, Created: now.Unix(), Secret: hash}, ttl)
	if err != nil {
		a.unavailable(w, r, err)
		return
	}
	res := struct {
		Code      string `json:"code"`
		ShortURL  string `json:"short_url"`
		ManageURL string `json:"manage_url"`
		ExpiresAt string `json:"expires_at,omitempty"`
	}{Code: code, ShortURL: a.base.JoinPath(code).String(), ManageURL: a.base.JoinPath("m", code, secret).String()}
	if ttl > 0 {
		res.ExpiresAt = now.Add(ttl).UTC().Format(time.RFC3339)
	}
	w.Header().Set("Cache-Control", "no-store")
	if wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(res)
		return
	}
	a.render(w, http.StatusCreated, "created", res)
}

// lookup answers 404 for a bad or unknown code, 410 for a taken down one, 503 if godis fails; ok is false once it has answered
func (a *app) lookup(w http.ResponseWriter, r *http.Request, code string) (link, bool) {
	if !validCode(code) {
		a.notFound(w, r)
		return link{}, false
	}
	ctx, cancel := a.ctx(r)
	defer cancel()
	l, found, err := a.getLink(ctx, code)
	switch {
	case err != nil:
		a.unavailable(w, r, err)
	case !found:
		a.notFound(w, r)
	case l.Removed:
		a.fail(w, r, http.StatusGone, "this link was taken down")
	default:
		return l, true
	}
	return link{}, false
}

func (a *app) preview(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	l, ok := a.lookup(w, r, code)
	if !ok {
		return
	}
	ctx, cancel := a.ctx(r)
	defer cancel()
	if _, err := a.kv.Incr(ctx, clicksKey(code), 1); err != nil {
		a.log.Error("count click", "code", code, "err", err)
	}
	u, err := url.Parse(l.URL)
	if err != nil {
		a.unavailable(w, r, fmt.Errorf("stored url of %s: %w", code, err))
		return
	}
	a.render(w, http.StatusOK, "preview", page{Code: code, URL: l.URL, Host: u.Host})
}

func (a *app) report(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	if a.limited(w, r, "report") {
		return
	}
	if _, ok := a.lookup(w, r, code); !ok {
		return
	}
	ctx, cancel := a.ctx(r)
	defer cancel()
	if _, err := a.kv.Incr(ctx, reportsKey(code), 1); err != nil {
		a.unavailable(w, r, err)
		return
	}
	if err := a.addReported(ctx, code); err != nil {
		a.unavailable(w, r, err)
		return
	}
	a.render(w, http.StatusOK, "reported", page{Code: code})
}

// owned finds the link a management url points at; a wrong secret answers exactly like an unknown code
func (a *app) owned(w http.ResponseWriter, r *http.Request) (link, string, bool) {
	code := r.PathValue("code")
	if !validCode(code) {
		a.notFound(w, r)
		return link{}, "", false
	}
	ctx, cancel := a.ctx(r)
	defer cancel()
	l, found, err := a.getLink(ctx, code)
	if err != nil {
		a.unavailable(w, r, err)
		return link{}, "", false
	}
	if !secretMatches(l.Secret, r.PathValue("secret")) || !found {
		a.notFound(w, r)
		return link{}, "", false
	}
	return l, code, true
}

func (a *app) manage(w http.ResponseWriter, r *http.Request) {
	l, code, ok := a.owned(w, r)
	if !ok {
		return
	}
	ctx, cancel := a.ctx(r)
	defer cancel()
	clicks, err := a.counter(ctx, clicksKey(code))
	if err != nil {
		a.unavailable(w, r, err)
		return
	}
	reports, err := a.counter(ctx, reportsKey(code))
	if err != nil {
		a.unavailable(w, r, err)
		return
	}
	ttl, _, err := a.kv.TTL(ctx, linkKey(code))
	if err != nil {
		a.unavailable(w, r, err)
		return
	}
	d := struct {
		Code, Secret, URL, Created, Expires, Short string
		Clicks, Reports                            int64
		Removed                                    bool
	}{
		Code: code, Secret: r.PathValue("secret"), URL: l.URL, Created: time.Unix(l.Created, 0).UTC().Format(time.RFC3339), Expires: "never",
		Short: a.base.JoinPath(code).String(), Clicks: clicks, Reports: reports, Removed: l.Removed,
	}
	if ttl > 0 {
		d.Expires = a.now().Add(ttl).UTC().Format(time.RFC3339)
	}
	w.Header().Set("Cache-Control", "no-store")
	a.render(w, http.StatusOK, "manage", d)
}

func (a *app) remove(w http.ResponseWriter, r *http.Request) {
	_, code, ok := a.owned(w, r)
	if !ok {
		return
	}
	ctx, cancel := a.ctx(r)
	defer cancel()
	if err := a.deleteLink(ctx, code); err != nil {
		a.unavailable(w, r, err)
		return
	}
	a.render(w, http.StatusOK, "deleted", page{Code: code})
}

func (a *app) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.cfg.adminToken == "" {
			a.notFound(w, r)
			return
		}
		_, pw, ok := r.BasicAuth()
		want, got := sha256.Sum256([]byte(a.cfg.adminToken)), sha256.Sum256([]byte(pw))
		if !ok || subtle.ConstantTimeCompare(want[:], got[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="admin", charset="UTF-8"`)
			a.fail(w, r, http.StatusUnauthorized, "admin password required")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		next(w, r)
	}
}

func (a *app) adminPage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := a.ctx(r)
	defer cancel()
	codes, err := a.reportedCodes(ctx)
	if err != nil {
		a.unavailable(w, r, err)
		return
	}
	type row struct {
		Code, URL string
		Reports   int64
		Removed   bool
	}
	var rows []row
	for _, code := range codes {
		l, found, err := a.getLink(ctx, code)
		if err != nil {
			a.unavailable(w, r, err)
			return
		}
		if !found {
			continue
		}
		n, err := a.counter(ctx, reportsKey(code))
		if err != nil {
			a.unavailable(w, r, err)
			return
		}
		rows = append(rows, row{Code: code, URL: l.URL, Reports: n, Removed: l.Removed})
	}
	a.render(w, http.StatusOK, "admin", rows)
}

func (a *app) adminTakedown(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	if !validCode(code) {
		a.notFound(w, r)
		return
	}
	ctx, cancel := a.ctx(r)
	defer cancel()
	found, err := a.takedown(ctx, code)
	switch {
	case err != nil:
		a.unavailable(w, r, err)
	case !found:
		a.notFound(w, r)
	default:
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
	}
}
