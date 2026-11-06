package site

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"html/template"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	adminCookieName = "olla_admin"
	sessionLifetime = 8 * time.Hour
	loginLifetime   = 15 * time.Minute
	limitWindow     = 15 * time.Minute
	maxSessions     = 2048
	maxLoginIPs     = 4096
	maxFormBytes    = maxPhotoBytes + 64<<10
)

type adminSession struct {
	csrf          string
	expires       time.Time
	authenticated bool
}

type attemptWindow struct {
	start time.Time
	count int
}

type adminHandler struct {
	store      *productStore
	configured bool
	password   [32]byte
	secure     bool
	mu         sync.Mutex
	sessions   map[string]adminSession
	attempts   map[string]attemptWindow
	global     attemptWindow
	creates    map[string]attemptWindow
}

func newAdminHandlerFromEnv(store *productStore) *adminHandler {
	secure := os.Getenv("ADMIN_COOKIE_SECURE") != "false"
	return newAdminHandler(store, os.Getenv("ADMIN_PASSWORD"), secure)
}

func newAdminHandler(store *productStore, password string, secure bool) *adminHandler {
	return &adminHandler{
		store: store, configured: len(password) > 0 && len(password) <= 1024, password: sha256.Sum256([]byte(password)), secure: secure,
		sessions: make(map[string]adminSession), attempts: make(map[string]attemptWindow), creates: make(map[string]attemptWindow),
	}
}

var adminTemplate = template.Must(template.New("admin").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Product administration</title><style>
body{box-sizing:border-box;max-width:42rem;margin:3rem auto;padding:0 1.25rem;background:#f5f0e9;color:#193b47;font:15px/1.6 'Trebuchet MS',Arial,sans-serif}
h1{font:normal clamp(2.5rem,6vw,4rem)/1.1 Georgia,serif;letter-spacing:-.04em}label{display:block;font-weight:600;margin-top:1.2rem}input,textarea{box-sizing:border-box;width:100%;padding:.75rem;font:inherit;color:#193b47;background:#fff;border:1px solid #9aaca9;border-radius:.2rem}textarea{min-height:9rem}button{margin-top:1.2rem;padding:.8rem 1.2rem;border:1px solid #193b47;background:#193b47;color:#fff;font:inherit;font-weight:bold;cursor:pointer}button:hover{background:#315c68} .message{padding:.75rem;background:#e8ccca}a{color:inherit} :focus-visible{outline:3px solid #b65b5c;outline-offset:3px}
</style></head><body><main><h1>Product administration</h1>
{{if .Message}}<p class="message" role="status">{{.Message}}</p>{{end}}
{{if .Disabled}}<p>Admin is disabled. Set ADMIN_PASSWORD to 1–1024 bytes on the server to enable it.</p>
{{else if .Authenticated}}
<p>Add a product that appears only on this website.</p>
<form action="/admin/products" method="post" enctype="multipart/form-data">
<input type="hidden" name="csrf" value="{{.CSRF}}">
<label for="title">Title</label><input id="title" name="title" maxlength="200" required>
<label for="description">Description</label><textarea id="description" name="description" maxlength="5000" required></textarea>
<label for="price">Price</label><input id="price" name="price" maxlength="100" placeholder="e.g. £25.00" required>
<label for="photo">Photo (JPEG or PNG, up to 5 MiB)</label><input id="photo" name="photo" type="file" accept="image/jpeg,image/png" required>
<button type="submit">Create product</button></form>
<form action="/admin/logout" method="post"><input type="hidden" name="csrf" value="{{.CSRF}}"><button type="submit">Log out</button></form>
{{else}}
<form action="/admin/login" method="post"><input type="hidden" name="csrf" value="{{.CSRF}}">
<label for="password">Password</label><input id="password" name="password" type="password" autocomplete="current-password" maxlength="1024" required>
<button type="submit">Log in</button></form>
{{end}}<p><a href="/">Back to website</a></p></main></body></html>`))

type adminPage struct {
	Message       string
	CSRF          string
	Disabled      bool
	Authenticated bool
}

func renderAdmin(w http.ResponseWriter, status int, page adminPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = adminTemplate.Execute(w, page)
}

func (h *adminHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")

	if h.secure && !secureAdminRequest(r) {
		http.Error(w, "HTTPS is required for admin access", http.StatusForbidden)
		return
	}

	var method string
	switch r.URL.Path {
	case "/admin":
		method = http.MethodGet
	case "/admin/login", "/admin/logout", "/admin/products":
		method = http.MethodPost
	default:
		http.NotFound(w, r)
		return
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.configured {
		renderAdmin(w, http.StatusServiceUnavailable, adminPage{Disabled: true})
		return
	}
	if r.URL.Path == "/admin" {
		h.show(w, r)
		return
	}

	id, session, ok := h.session(r)
	if !ok {
		http.Error(w, "Session expired; reload the admin page", http.StatusForbidden)
		return
	}
	if r.URL.Path != "/admin/login" && !session.authenticated {
		http.Error(w, "Login required", http.StatusForbidden)
		return
	}
	if r.URL.Path == "/admin/products" {
		r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
		h.mu.Lock()
		window, allowed := consumeWindow(h.creates[id], time.Now(), 20)
		if allowed {
			h.creates[id] = window
		}
		h.mu.Unlock()
		if !allowed {
			w.Header().Set("Retry-After", "900")
			http.Error(w, "Too many product submissions; try again later", http.StatusTooManyRequests)
			return
		}
		if err := r.ParseMultipartForm(maxFormBytes); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "Upload is too large", http.StatusRequestEntityTooLarge)
			} else {
				http.Error(w, "Invalid upload", http.StatusBadRequest)
			}
			return
		}
		if r.MultipartForm != nil {
			defer r.MultipartForm.RemoveAll()
		}
	} else {
		r.Body = http.MaxBytesReader(w, r.Body, 8192)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Invalid form", http.StatusBadRequest)
			return
		}
	}
	if !equalToken(r.PostForm.Get("csrf"), session.csrf) {
		http.Error(w, "Invalid CSRF token", http.StatusForbidden)
		return
	}
	switch r.URL.Path {
	case "/admin/login":
		h.login(w, r, id)
	case "/admin/logout":
		h.mu.Lock()
		delete(h.sessions, id)
		delete(h.creates, id)
		h.mu.Unlock()
		h.clearCookie(w)
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
	case "/admin/products":
		h.create(w, r, session)
	}
}

func secureAdminRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	forwarded := r.Header.Values("X-Forwarded-Proto")
	return ip != nil && ip.IsLoopback() && len(forwarded) == 1 && forwarded[0] == "https"
}

func equalToken(a, b string) bool {
	return len(a) == 64 && len(b) == 64 && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func (h *adminHandler) session(r *http.Request) (string, adminSession, bool) {
	cookie, err := r.Cookie(adminCookieName)
	if err != nil || len(cookie.Value) != 64 {
		return "", adminSession{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sessions[cookie.Value]
	if !ok || !time.Now().Before(s.expires) {
		delete(h.sessions, cookie.Value)
		delete(h.creates, cookie.Value)
		return "", adminSession{}, false
	}
	return cookie.Value, s, true
}

func (h *adminHandler) newSession(authenticated bool) (string, adminSession, error) {
	id, err := randomToken(32)
	if err != nil {
		return "", adminSession{}, err
	}
	csrf, err := randomToken(32)
	if err != nil {
		return "", adminSession{}, err
	}
	now := time.Now()
	lifetime := loginLifetime
	if authenticated {
		lifetime = sessionLifetime
	}
	s := adminSession{csrf: csrf, expires: now.Add(lifetime), authenticated: authenticated}
	h.mu.Lock()
	defer h.mu.Unlock()
	for key, existing := range h.sessions {
		if !now.Before(existing.expires) {
			delete(h.sessions, key)
			delete(h.creates, key)
		}
	}
	if len(h.sessions) >= maxSessions {
		// Unauthenticated visitors must not be able to permanently fill the session pool.
		oldestID := ""
		var oldest time.Time
		for key, existing := range h.sessions {
			if !existing.authenticated && (oldestID == "" || existing.expires.Before(oldest)) {
				oldestID, oldest = key, existing.expires
			}
		}
		if oldestID == "" {
			return "", adminSession{}, errors.New("too many sessions")
		}
		delete(h.sessions, oldestID)
		delete(h.creates, oldestID)
	}
	h.sessions[id] = s
	return id, s, nil
}

func (h *adminHandler) setCookie(w http.ResponseWriter, id string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{Name: adminCookieName, Value: id, Path: "/admin", HttpOnly: true, Secure: h.secure, SameSite: http.SameSiteStrictMode, Expires: expires})
}

func (h *adminHandler) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: adminCookieName, Value: "", Path: "/admin", HttpOnly: true, Secure: h.secure, SameSite: http.SameSiteStrictMode, MaxAge: -1})
}

func (h *adminHandler) show(w http.ResponseWriter, r *http.Request) {
	_, session, ok := h.session(r)
	if !ok {
		id, s, err := h.newSession(false)
		if err != nil {
			http.Error(w, "Admin is temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		session = s
		h.setCookie(w, id, session.expires)
	}
	message := ""
	if session.authenticated && r.URL.Query().Get("created") == "1" {
		message = "Product created."
	}
	renderAdmin(w, http.StatusOK, adminPage{CSRF: session.csrf, Authenticated: session.authenticated, Message: message})
}

func consumeWindow(window attemptWindow, now time.Time, limit int) (attemptWindow, bool) {
	if window.start.IsZero() || now.Sub(window.start) >= limitWindow {
		window = attemptWindow{start: now}
	}
	if window.count >= limit {
		return window, false
	}
	window.count++
	return window, true
}

func (h *adminHandler) allowLogin(r *http.Request) bool {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	now := time.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	for key, window := range h.attempts {
		if now.Sub(window.start) >= limitWindow {
			delete(h.attempts, key)
		}
	}
	if _, ok := h.attempts[ip]; !ok && len(h.attempts) >= maxLoginIPs {
		return false
	}
	global, globalOK := consumeWindow(h.global, now, 200)
	perIP, ipOK := consumeWindow(h.attempts[ip], now, 5)
	if !globalOK || !ipOK {
		return false
	}
	h.global, h.attempts[ip] = global, perIP
	return true
}

func (h *adminHandler) login(w http.ResponseWriter, r *http.Request, oldID string) {
	if !h.allowLogin(r) {
		w.Header().Set("Retry-After", "900")
		http.Error(w, "Too many login attempts; try again later", http.StatusTooManyRequests)
		return
	}
	password := r.PostForm.Get("password")
	candidate := sha256.Sum256([]byte(password))
	if len(password) > 1024 || subtle.ConstantTimeCompare(candidate[:], h.password[:]) != 1 {
		_, s, ok := h.session(r)
		if !ok {
			http.Error(w, "Session expired", http.StatusForbidden)
			return
		}
		renderAdmin(w, http.StatusUnauthorized, adminPage{CSRF: s.csrf, Message: "Incorrect password."})
		return
	}
	id, session, err := h.newSession(true)
	if err != nil {
		http.Error(w, "Admin is temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	h.mu.Lock()
	delete(h.sessions, oldID)
	delete(h.creates, oldID)
	h.mu.Unlock()
	h.setCookie(w, id, session.expires)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (h *adminHandler) create(w http.ResponseWriter, r *http.Request, session adminSession) {
	page := adminPage{CSRF: session.csrf, Authenticated: true}
	file, _, err := r.FormFile("photo")
	if err != nil {
		page.Message = "Choose a JPEG or PNG photo."
		renderAdmin(w, http.StatusBadRequest, page)
		return
	}
	defer file.Close()
	photo, err := readPhoto(file)
	if err != nil {
		page.Message = "Photo must be at most 5 MiB."
		renderAdmin(w, http.StatusBadRequest, page)
		return
	}
	p := product{Title: strings.TrimSpace(r.PostForm.Get("title")), Description: strings.TrimSpace(r.PostForm.Get("description")), Price: strings.TrimSpace(r.PostForm.Get("price"))}
	_, err = h.store.create(p, photo)
	if errors.Is(err, errInvalidProduct) {
		page.Message = "Enter a title, description and price within the limits, and a valid JPEG or PNG photo (maximum 4096 pixels per side and 16 megapixels)."
		renderAdmin(w, http.StatusBadRequest, page)
		return
	}
	if err != nil {
		page.Message = "Product could not be saved. Check server storage."
		renderAdmin(w, http.StatusServiceUnavailable, page)
		return
	}
	http.Redirect(w, r, "/admin?created=1", http.StatusSeeOther)
}
