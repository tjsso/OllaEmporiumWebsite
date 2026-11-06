package site

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testPhoto(t *testing.T, format string) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var b bytes.Buffer
	var err error
	if format == "jpeg" {
		err = jpeg.Encode(&b, img, nil)
	} else {
		err = png.Encode(&b, img)
	}
	if err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func request(handler http.Handler, method, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w
}

func TestProductsEmptyAndMethods(t *testing.T) {
	t.Setenv("PRODUCT_DATA_DIR", t.TempDir())
	t.Setenv("ADMIN_PASSWORD", "")
	h := newHandler()
	w := request(h, "GET", "/api/products")
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"products":[]}` {
		t.Fatalf("empty response: %d %s", w.Code, w.Body.String())
	}
	w = request(h, "POST", "/api/products")
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET" {
		t.Fatalf("method response: %d %s", w.Code, w.Header().Get("Allow"))
	}
	for _, path := range []string{"/product", "/product.js"} {
		if w := request(h, "GET", path); w.Code != http.StatusOK {
			t.Errorf("%s: %d", path, w.Code)
		}
		if w := request(h, "POST", path); w.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: %d", path, w.Code)
		}
	}
}

type blockedProductWriter struct {
	*httptest.ResponseRecorder
	started chan struct{}
	release chan struct{}
}

func (w *blockedProductWriter) Write(data []byte) (int, error) {
	close(w.started)
	<-w.release
	return w.ResponseRecorder.Write(data)
}

func TestProductsReleaseLockBeforeResponseWrite(t *testing.T) {
	for _, broken := range []bool{false, true} {
		t.Run(fmt.Sprintf("loadError=%t", broken), func(t *testing.T) {
			s := newProductStore(t.TempDir())
			s.records = append(s.records, productRecord{Product: product{Title: "original"}})
			if broken {
				s.loadErr = errors.New("storage error")
			}
			w := &blockedProductWriter{ResponseRecorder: httptest.NewRecorder(), started: make(chan struct{}), release: make(chan struct{})}
			var once sync.Once
			unblock := func() { once.Do(func() { close(w.release) }) }
			defer unblock()
			finished := make(chan struct{})
			go func() {
				s.serveProducts(w, httptest.NewRequest("GET", "/api/products", nil))
				close(finished)
			}()
			select {
			case <-w.started:
			case <-time.After(5 * time.Second):
				t.Fatal("response did not start")
			}
			modified := make(chan struct{})
			go func() {
				s.mu.Lock()
				s.records = append(s.records, productRecord{Product: product{Title: "new"}})
				s.mu.Unlock()
				close(modified)
			}()
			select {
			case <-modified:
			case <-time.After(5 * time.Second):
				unblock()
				<-finished
				<-modified
				t.Fatal("response write held product store lock")
			}
			unblock()
			<-finished
			if !broken {
				var response struct {
					Products []product `json:"products"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || len(response.Products) != 1 || response.Products[0].Title != "original" {
					t.Fatalf("response did not preserve snapshot: %s (%v)", w.Body.String(), err)
				}
			} else if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("error response: %d", w.Code)
			}
		})
	}
}

func TestProductStorePersistenceAndImages(t *testing.T) {
	dir := t.TempDir()
	s := newProductStore(dir)
	photo := append(testPhoto(t, "png"), []byte("<script>not an image</script>")...)
	p, err := s.create(product{Title: "A mug", Description: "Handmade", Price: "£25"}, photo)
	if err != nil {
		t.Fatal(err)
	}
	if !validProductID(p.ID) || p.ImageURL != "/product-images/"+p.ID {
		t.Fatalf("invalid product: %+v", p)
	}
	restarted := newProductStore(dir)
	w := httptest.NewRecorder()
	restarted.serveProducts(w, httptest.NewRequest("GET", "/api/products", nil))
	var response struct {
		Products []product `json:"products"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || len(response.Products) != 1 || response.Products[0] != p {
		t.Fatalf("round trip: %s (%v)", w.Body.String(), err)
	}
	for _, method := range []string{"GET", "HEAD"} {
		w = httptest.NewRecorder()
		restarted.serveImage(w, httptest.NewRequest(method, p.ImageURL, nil))
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/png" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("image: %d %v", w.Code, w.Header())
		}
		if bytes.Contains(w.Body.Bytes(), []byte("<script>")) || method == "HEAD" && w.Body.Len() != 0 {
			t.Fatal("unsafe or unexpected image body")
		}
	}
	for _, path := range []string{"/product-images/../products/" + p.ID, "/product-images/" + p.ID + ".png", "/product-images/00000000000000000000000000000000"} {
		w = httptest.NewRecorder()
		restarted.serveImage(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: %d", path, w.Code)
		}
	}
	w = httptest.NewRecorder()
	restarted.serveImage(w, httptest.NewRequest("POST", p.ImageURL, nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST image: %d", w.Code)
	}
}

func TestProductImageDoesNotFollowSymlinks(t *testing.T) {
	dir := t.TempDir()
	s := newProductStore(dir)
	p, err := s.create(product{Title: "t", Description: "d", Price: "p"}, testPhoto(t, "png"))
	if err != nil {
		t.Fatal(err)
	}
	imagePath := filepath.Join(dir, "images", p.ID+".png")
	secretPath := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(secretPath, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(imagePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secretPath, imagePath); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	w := httptest.NewRecorder()
	s.serveImage(w, httptest.NewRequest("GET", p.ImageURL, nil))
	if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("symlink served: %d %s", w.Code, w.Body.String())
	}
}

func TestPhotoValidation(t *testing.T) {
	for _, format := range []string{"jpeg", "png"} {
		data, got, err := sanitizePhoto(testPhoto(t, format))
		if err != nil || got != format || len(data) == 0 {
			t.Fatalf("%s: %s %v", format, got, err)
		}
	}
	for _, photo := range [][]byte{nil, []byte("<svg></svg>"), bytes.Repeat([]byte("x"), maxPhotoBytes+1), testPhoto(t, "png")[:20]} {
		if _, _, err := sanitizePhoto(photo); err == nil {
			t.Error("invalid photo accepted")
		}
	}
	img := image.NewRGBA(image.Rect(0, 0, maxImageSide+1, 1))
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	if _, _, err := sanitizePhoto(b.Bytes()); err == nil {
		t.Error("oversized dimensions accepted")
	}
}

func TestProductWithMaxEscapedTextSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s := newProductStore(dir)
	p := product{Title: strings.Repeat("<", 200), Description: strings.Repeat("<", 5000), Price: strings.Repeat("<", 100)}
	created, err := s.create(p, testPhoto(t, "png"))
	if err != nil {
		t.Fatal(err)
	}
	reloaded := newProductStore(dir)
	if reloaded.loadErr != nil || len(reloaded.records) != 1 || reloaded.records[0].Product != created {
		t.Fatalf("failed to reload escaped fields: %v", reloaded.loadErr)
	}
}

func TestProductStorageErrorsAndBounds(t *testing.T) {
	dir := t.TempDir()
	s := newProductStore(dir)
	photo := testPhoto(t, "jpeg")
	for _, p := range []product{
		{Title: "", Description: "d", Price: "p"},
		{Title: strings.Repeat("a", 201), Description: "d", Price: "p"},
		{Title: "t", Description: strings.Repeat("a", 5001), Price: "p"},
		{Title: "t", Description: "d", Price: strings.Repeat("a", 101)},
		{Title: "t\x00", Description: "d", Price: "p"},
	} {
		if _, err := s.create(p, photo); err != errInvalidProduct {
			t.Errorf("invalid fields accepted: %+v (%v)", p, err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "products"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "products", "bad.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	s = newProductStore(dir)
	w := httptest.NewRecorder()
	s.serveProducts(w, httptest.NewRequest("GET", "/api/products", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("corrupt storage: %d", w.Code)
	}
	if _, err := s.create(product{Title: "t", Description: "d", Price: "p"}, photo); err == nil {
		t.Fatal("wrote to corrupt store")
	}
}

func TestConcurrentProductCreation(t *testing.T) {
	dir := t.TempDir()
	s := newProductStore(dir)
	photo := testPhoto(t, "png")
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.create(product{Title: "t", Description: "d", Price: "p"}, photo); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	restarted := newProductStore(dir)
	if restarted.loadErr != nil || len(restarted.records) != 10 {
		t.Fatalf("reloaded %d products: %v", len(restarted.records), restarted.loadErr)
	}
}
