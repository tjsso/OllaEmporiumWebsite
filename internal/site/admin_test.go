package site

import (
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

var csrfPattern = regexp.MustCompile(`name="csrf" value="([a-f0-9]{64})"`)

func adminRequest(h http.Handler, method, path string, body io.Reader, contentType string, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "https://example.com"+path, body)
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func adminPageSession(t *testing.T, h http.Handler) (*http.Cookie, string) {
	t.Helper()
	w := adminRequest(h, "GET", "/admin", nil, "", nil)
	matches := csrfPattern.FindStringSubmatch(w.Body.String())
	if w.Code != http.StatusOK || len(matches) != 2 || len(w.Result().Cookies()) != 1 {
		t.Fatalf("admin page: %d %s", w.Code, w.Body.String())
	}
	return w.Result().Cookies()[0], matches[1]
}

func postForm(h http.Handler, path string, values url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	return adminRequest(h, "POST", path, strings.NewReader(values.Encode()), "application/x-www-form-urlencoded", cookie)
}

func loginAdmin(t *testing.T, h http.Handler, password string) (*http.Cookie, string) {
	t.Helper()
	cookie, csrf := adminPageSession(t, h)
	w := postForm(h, "/admin/login", url.Values{"csrf": {csrf}, "password": {password}}, cookie)
	if w.Code != http.StatusSeeOther || len(w.Result().Cookies()) != 1 {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	authCookie := w.Result().Cookies()[0]
	if cookie.Value == authCookie.Value {
		t.Fatal("session was not rotated")
	}
	w = adminRequest(h, "GET", "/admin", nil, "", authCookie)
	matches := csrfPattern.FindStringSubmatch(w.Body.String())
	if w.Code != http.StatusOK || len(matches) != 2 || matches[1] == csrf || !strings.Contains(w.Body.String(), "Create product") {
		t.Fatalf("authenticated page: %d %s", w.Code, w.Body.String())
	}
	return authCookie, matches[1]
}

func productForm(t *testing.T, fields map[string]string, photo []byte) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for key, value := range fields {
		if err := mw.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	if photo != nil {
		file, err := mw.CreateFormFile("photo", "../../danger.svg")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(photo); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &body, mw.FormDataContentType()
}

func TestAdminDisabledAndMethods(t *testing.T) {
	t.Setenv("PRODUCT_DATA_DIR", t.TempDir())
	t.Setenv("ADMIN_PASSWORD", "")
	t.Setenv("ADMIN_COOKIE_SECURE", "false")
	h := newHandler()
	w := request(h, "GET", "/admin")
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "ADMIN_PASSWORD") || len(w.Result().Cookies()) != 0 {
		t.Fatalf("disabled: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "GET", "/api/products"); w.Code != http.StatusOK {
		t.Fatalf("public products unavailable: %d", w.Code)
	}
	for path, method := range map[string]string{"/admin": "POST", "/admin/login": "GET", "/admin/logout": "GET", "/admin/products": "GET"} {
		if w = request(h, method, path); w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: %d", method, path, w.Code)
		}
	}
	if w = request(h, "GET", "/admin/unknown"); w.Code != http.StatusNotFound {
		t.Fatalf("unknown admin path: %d", w.Code)
	}
}

func TestAdminCookieSecureEnvironment(t *testing.T) {
	tests := []struct {
		name   string
		value  string
		unset  bool
		secure bool
	}{
		{name: "unset", unset: true, secure: true},
		{name: "empty", value: "", secure: true},
		{name: "invalid", value: "invalid", secure: true},
		{name: "numeric false", value: "0", secure: true},
		{name: "uppercase false", value: "FALSE", secure: true},
		{name: "padded false", value: "false ", secure: true},
		{name: "true", value: "true", secure: true},
		{name: "explicit false", value: "false", secure: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ADMIN_PASSWORD", "secret")
			t.Setenv("ADMIN_COOKIE_SECURE", tt.value)
			if tt.unset {
				if err := os.Unsetenv("ADMIN_COOKIE_SECURE"); err != nil {
					t.Fatal(err)
				}
			}
			h := newAdminHandlerFromEnv(newProductStore(t.TempDir()))
			for _, request := range []struct {
				name, target, remote, forwarded string
				allow                           bool
			}{
				{"direct HTTP", "http://example.com/admin", "203.0.113.1:1234", "", !tt.secure},
				{"direct HTTPS", "https://example.com/admin", "203.0.113.1:1234", "", true},
				{"loopback proxy", "http://example.com/admin", "127.0.0.1:1234", "https", true},
			} {
				r := httptest.NewRequest("GET", request.target, nil)
				r.RemoteAddr = request.remote
				if request.forwarded != "" {
					r.Header.Set("X-Forwarded-Proto", request.forwarded)
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if !request.allow {
					if w.Code != http.StatusForbidden || len(w.Result().Cookies()) != 0 {
						t.Fatalf("%s: expected HTTPS rejection, got %d", request.name, w.Code)
					}
					continue
				}
				cookies := w.Result().Cookies()
				if w.Code != http.StatusOK || len(cookies) != 1 || cookies[0].Secure != tt.secure {
					t.Fatalf("%s: status=%d cookies=%v, want secure=%v", request.name, w.Code, cookies, tt.secure)
				}
			}
		})
	}
}

func TestAdminSecureRequestEnforcement(t *testing.T) {
	h := newAdminHandler(newProductStore(t.TempDir()), "secret", true)
	tests := []struct {
		name, target, remote string
		forwarded            []string
		want                 int
	}{
		{"direct TLS", "https://example.com/admin", "203.0.113.1:1234", nil, http.StatusOK},
		{"TLS ignores forwarded header", "https://example.com/admin", "203.0.113.1:1234", []string{"http"}, http.StatusOK},
		{"public HTTP", "http://example.com/admin", "203.0.113.1:1234", nil, http.StatusForbidden},
		{"spoofed forwarding", "http://example.com/admin", "203.0.113.1:1234", []string{"https"}, http.StatusForbidden},
		{"loopback HTTP", "http://example.com/admin", "127.0.0.1:1234", nil, http.StatusForbidden},
		{"IPv4 loopback proxy", "http://example.com/admin", "127.0.0.1:1234", []string{"https"}, http.StatusOK},
		{"IPv6 loopback proxy", "http://example.com/admin", "[::1]:1234", []string{"https"}, http.StatusOK},
		{"IPv4-mapped loopback", "http://example.com/admin", "[::ffff:127.0.0.1]:1234", []string{"https"}, http.StatusOK},
		{"wrong scheme", "http://example.com/admin", "127.0.0.1:1234", []string{"http"}, http.StatusForbidden},
		{"case variant", "http://example.com/admin", "127.0.0.1:1234", []string{"HTTPS"}, http.StatusForbidden},
		{"forwarded chain", "http://example.com/admin", "127.0.0.1:1234", []string{"https, http"}, http.StatusForbidden},
		{"duplicate headers", "http://example.com/admin", "127.0.0.1:1234", []string{"https", "https"}, http.StatusForbidden},
		{"hostname is not an IP", "http://example.com/admin", "localhost:1234", []string{"https"}, http.StatusForbidden},
		{"invalid address", "http://example.com/admin", "127.0.0.1", []string{"https"}, http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", tt.target, nil)
			r.RemoteAddr = tt.remote
			for _, value := range tt.forwarded {
				r.Header.Add("X-Forwarded-Proto", value)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, tt.want, w.Body.String())
			}
			if tt.want == http.StatusForbidden && (len(w.Result().Cookies()) != 0 || !strings.Contains(w.Body.String(), "HTTPS")) {
				t.Fatalf("insecure response: %s", w.Body.String())
			}
		})
	}

	for _, path := range []string{"/admin", "/admin/login", "/admin/logout", "/admin/products", "/admin/unknown"} {
		probe := &readProbe{}
		r := httptest.NewRequest("POST", "http://example.com"+path, probe)
		r.RemoteAddr = "203.0.113.1:1234"
		r.Header.Set("X-Forwarded-Proto", "https")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "HTTPS") || probe.read {
			t.Errorf("insecure POST %s: %d %s (body read: %v)", path, w.Code, w.Body.String(), probe.read)
		}
	}

	insecureAllowed := newAdminHandler(newProductStore(t.TempDir()), "secret", false)
	r := httptest.NewRequest("GET", "http://example.com/admin", nil)
	w := httptest.NewRecorder()
	insecureAllowed.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP with secure cookies disabled: %d", w.Code)
	}
}

func TestAdminLoginThroughLoopbackProxy(t *testing.T) {
	h := newAdminHandler(newProductStore(t.TempDir()), "secret", true)
	do := func(method, path string, body io.Reader, cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://example.com"+path, body)
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("X-Forwarded-Proto", "https")
		if method == http.MethodPost {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	w := do("GET", "/admin", nil, nil)
	matches := csrfPattern.FindStringSubmatch(w.Body.String())
	if w.Code != http.StatusOK || len(matches) != 2 || len(w.Result().Cookies()) != 1 {
		t.Fatalf("proxy page: %d %s", w.Code, w.Body.String())
	}
	values := url.Values{"csrf": {matches[1]}, "password": {"secret"}}
	w = do("POST", "/admin/login", strings.NewReader(values.Encode()), w.Result().Cookies()[0])
	if w.Code != http.StatusSeeOther || len(w.Result().Cookies()) != 1 || !w.Result().Cookies()[0].Secure {
		t.Fatalf("proxy login: %d %s", w.Code, w.Body.String())
	}
}

func TestAdminLoginCreateLogoutAndRestart(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PRODUCT_DATA_DIR", dir)
	t.Setenv("ADMIN_PASSWORD", "a private password")
	t.Setenv("ADMIN_COOKIE_SECURE", "true")
	h := newHandler()
	cookie, csrf := loginAdmin(t, h, "a private password")
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/admin" || cookie.Expires.IsZero() {
		t.Fatalf("unsafe cookie: %+v", cookie)
	}
	fields := map[string]string{"csrf": csrf, "title": "<Mug>", "description": "Handmade", "price": "€25"}
	body, contentType := productForm(t, fields, testPhoto(t, "png"))
	w := adminRequest(h, "POST", "/admin/products", body, contentType, cookie)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/admin?created=1" {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	w = request(h, "GET", "/api/products")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `\u003cMug\u003e`) || strings.Contains(w.Body.String(), "danger.svg") {
		t.Fatalf("products: %d %s", w.Code, w.Body.String())
	}
	restarted := newHandler()
	w = request(restarted, "GET", "/api/products")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `\u003cMug\u003e`) {
		t.Fatalf("restart: %d %s", w.Code, w.Body.String())
	}
	w = postForm(restarted, "/admin/logout", url.Values{"csrf": {csrf}}, cookie)
	if w.Code != http.StatusForbidden {
		t.Fatalf("session survived restart: %d", w.Code)
	}
	w = postForm(h, "/admin/logout", url.Values{"csrf": {csrf}}, cookie)
	if w.Code != http.StatusSeeOther || len(w.Result().Cookies()) != 1 || w.Result().Cookies()[0].MaxAge >= 0 {
		t.Fatalf("logout: %d %v", w.Code, w.Result().Cookies())
	}
	body, contentType = productForm(t, fields, testPhoto(t, "png"))
	if w = adminRequest(h, "POST", "/admin/products", body, contentType, cookie); w.Code != http.StatusForbidden {
		t.Fatalf("logged out session accepted: %d", w.Code)
	}
}

func TestAdminCSRFAndValidation(t *testing.T) {
	h := newAdminHandler(newProductStore(t.TempDir()), "secret", false)
	anon, token := adminPageSession(t, h)
	if w := postForm(h, "/admin/login", url.Values{"password": {"secret"}}, anon); w.Code != http.StatusForbidden {
		t.Fatalf("login without csrf: %d", w.Code)
	}
	if w := postForm(h, "/admin/login", url.Values{"csrf": {token}, "password": {"wrong"}}, anon); w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), "wrong") {
		t.Fatalf("wrong password: %d %s", w.Code, w.Body.String())
	}
	if w := postForm(h, "/admin/logout", url.Values{"csrf": {token}}, anon); w.Code != http.StatusForbidden {
		t.Fatalf("anonymous logout: %d", w.Code)
	}
	cookie, csrf := loginAdmin(t, h, "secret")
	if w := postForm(h, "/admin/logout", url.Values{"csrf": {token}}, cookie); w.Code != http.StatusForbidden {
		t.Fatalf("old CSRF accepted: %d", w.Code)
	}
	fields := map[string]string{"csrf": "bad", "title": "t", "description": "d", "price": "p"}
	body, contentType := productForm(t, fields, testPhoto(t, "png"))
	if w := adminRequest(h, "POST", "/admin/products", body, contentType, cookie); w.Code != http.StatusForbidden {
		t.Fatalf("bad product csrf: %d", w.Code)
	}
	fields["csrf"] = csrf
	for _, photo := range [][]byte{nil, []byte("<svg/>"), bytes.Repeat([]byte("x"), maxPhotoBytes+1)} {
		body, contentType = productForm(t, fields, photo)
		if w := adminRequest(h, "POST", "/admin/products", body, contentType, cookie); w.Code != http.StatusBadRequest {
			t.Errorf("invalid photo: %d %s", w.Code, w.Body.String())
		}
	}
	body, contentType = productForm(t, fields, bytes.Repeat([]byte("x"), maxFormBytes+1))
	if w := adminRequest(h, "POST", "/admin/products", body, contentType, cookie); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized form: %d", w.Code)
	}
	if w := postForm(h, "/admin/logout", url.Values{}, cookie); w.Code != http.StatusForbidden {
		t.Fatalf("logout without csrf: %d", w.Code)
	}
}

func TestAdminCreateThrottleAndStorageFailure(t *testing.T) {
	dir := t.TempDir()
	s := newProductStore(dir)
	h := newAdminHandler(s, "secret", false)
	cookie, csrf := loginAdmin(t, h, "secret")
	fields := map[string]string{"csrf": csrf, "title": "t", "description": "d", "price": "p"}
	// The per-session limit applies to rejected submissions too.
	for i := 0; i < 21; i++ {
		body, contentType := productForm(t, fields, nil)
		w := adminRequest(h, "POST", "/admin/products", body, contentType, cookie)
		want := http.StatusBadRequest
		if i == 20 {
			want = http.StatusTooManyRequests
		}
		if w.Code != want {
			t.Fatalf("submission %d: %d", i, w.Code)
		}
	}

	h2 := newAdminHandler(newProductStore(dir), "secret", false)
	cookie, csrf = loginAdmin(t, h2, "secret")
	h2.store.mu.Lock()
	h2.store.loadErr = errors.New("sensitive internal details")
	h2.store.mu.Unlock()
	fields["csrf"] = csrf
	body, contentType := productForm(t, fields, testPhoto(t, "png"))
	w := adminRequest(h2, "POST", "/admin/products", body, contentType, cookie)
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "sensitive internal details") {
		t.Fatalf("storage failure: %d %s", w.Code, w.Body.String())
	}
}

type readProbe struct {
	read bool
}

func (p *readProbe) Read([]byte) (int, error) {
	p.read = true
	return 0, io.EOF
}

func TestAdminCreateThrottleBeforeParsing(t *testing.T) {
	h := newAdminHandler(newProductStore(t.TempDir()), "secret", false)
	anon, _ := adminPageSession(t, h)
	for i := 0; i < 21; i++ {
		if w := adminRequest(h, "POST", "/admin/products", strings.NewReader("bad"), "multipart/form-data; boundary=broken", anon); w.Code != http.StatusForbidden {
			t.Fatalf("anonymous request %d: %d", i, w.Code)
		}
	}
	cookie, csrf := loginAdmin(t, h, "secret")
	for i := 0; i < 20; i++ {
		var body io.Reader
		var contentType string
		want := http.StatusForbidden
		if i%2 == 0 {
			body, contentType = productForm(t, map[string]string{"csrf": "invalid"}, nil)
		} else {
			body = strings.NewReader("malformed")
			contentType = "multipart/form-data; boundary=broken"
			want = http.StatusBadRequest
		}
		w := adminRequest(h, "POST", "/admin/products", body, contentType, cookie)
		if w.Code != want {
			t.Fatalf("attempt %d: got %d, want %d", i, w.Code, want)
		}
	}
	probe := &readProbe{}
	w := adminRequest(h, "POST", "/admin/products", probe, "multipart/form-data; boundary=broken", cookie)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "900" || probe.read {
		t.Fatalf("limited request: %d, read body: %v", w.Code, probe.read)
	}
	body, contentType := productForm(t, map[string]string{"csrf": csrf, "title": "t", "description": "d", "price": "p"}, testPhoto(t, "png"))
	if w = adminRequest(h, "POST", "/admin/products", body, contentType, cookie); w.Code != http.StatusTooManyRequests {
		t.Fatalf("valid request after limit: %d", w.Code)
	}
}

func TestAdminLoginThrottleAndExpiry(t *testing.T) {
	h := newAdminHandler(newProductStore(t.TempDir()), "secret", false)
	cookie, csrf := adminPageSession(t, h)
	for i := 0; i < 6; i++ {
		w := postForm(h, "/admin/login", url.Values{"csrf": {csrf}, "password": {"wrong"}}, cookie)
		want := http.StatusUnauthorized
		if i == 5 {
			want = http.StatusTooManyRequests
		}
		if w.Code != want {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
	h.mu.Lock()
	s := h.sessions[cookie.Value]
	s.expires = time.Now().Add(-time.Second)
	h.sessions[cookie.Value] = s
	h.mu.Unlock()
	if w := postForm(h, "/admin/login", url.Values{"csrf": {csrf}, "password": {"secret"}}, cookie); w.Code != http.StatusForbidden {
		t.Fatalf("expired session accepted: %d", w.Code)
	}
}
