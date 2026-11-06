package site

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAssetRoutes(t *testing.T) {
	tests := []struct {
		path        string
		contentType string
		content     []byte
	}{
		{"/", "text/html; charset=utf-8", indexHTML},
		{"/index.html", "text/html; charset=utf-8", indexHTML},
		{"/styles.css", "text/css; charset=utf-8", stylesCSS},
		{"/script.js", "text/javascript; charset=utf-8", scriptJS},
		{"/ollaemporiumbanner.webp", "image/webp", bannerWebP},
		{"/ollamug.webp", "image/webp", mugWebP},
		{"/ollaplate.webp", "image/webp", plateWebP},
	}

	handler := newHandler()
	for _, tt := range tests {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			t.Run(method+" "+tt.path, func(t *testing.T) {
				req := httptest.NewRequest(method, tt.path+"?v=1", nil)
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, req)

				if w.Code != http.StatusOK {
					t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
				}
				if got := w.Header().Get("Content-Type"); got != tt.contentType {
					t.Errorf("Content-Type = %q, want %q", got, tt.contentType)
				}
				if got, want := w.Header().Get("Content-Length"), fmt.Sprint(len(tt.content)); got != want {
					t.Errorf("Content-Length = %q, want %q", got, want)
				}
				if method == http.MethodHead {
					if w.Body.Len() != 0 {
						t.Errorf("HEAD body has %d bytes, want none", w.Body.Len())
					}
				} else if !bytes.Equal(w.Body.Bytes(), tt.content) {
					t.Error("response body does not match embedded asset")
				}
			})
		}
	}
}

func TestUnknownRoutes(t *testing.T) {
	handler := newHandler()
	for _, path := range []string{
		"/missing", "/missing.css", "/index.html/", "/images/ollamug.webp", "/../index.html", "/.git/config",
	} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			t.Run(method+" "+path, func(t *testing.T) {
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, httptest.NewRequest(method, path, nil))
				if w.Code != http.StatusNotFound {
					t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
				}
			})
		}
	}
}

func TestUnsupportedMethods(t *testing.T) {
	handler := newHandler()
	for _, path := range []string{"/", "/styles.css", "/ollamug.webp"} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions} {
			t.Run(method+" "+path, func(t *testing.T) {
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, httptest.NewRequest(method, path, nil))
				if w.Code != http.StatusMethodNotAllowed {
					t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
				}
				if got := w.Header().Get("Allow"); got != "GET, HEAD" {
					t.Errorf("Allow = %q, want %q", got, "GET, HEAD")
				}
			})
		}
	}
}

func TestAssetRangeRequest(t *testing.T) {
	if len(mugWebP) < 4 {
		t.Fatal("embedded mug image is too small for range test")
	}

	req := httptest.NewRequest(http.MethodGet, "/ollamug.webp", nil)
	req.Header.Set("Range", "bytes=1-3")
	w := httptest.NewRecorder()
	newHandler().ServeHTTP(w, req)

	if w.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusPartialContent)
	}
	if got, want := w.Header().Get("Content-Range"), fmt.Sprintf("bytes 1-3/%d", len(mugWebP)); got != want {
		t.Errorf("Content-Range = %q, want %q", got, want)
	}
	if !bytes.Equal(w.Body.Bytes(), mugWebP[1:4]) {
		t.Error("response body does not match requested range")
	}
}
