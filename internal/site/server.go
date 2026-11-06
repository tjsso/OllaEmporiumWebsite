package site

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

//go:embed web/index.html
var indexHTML []byte

//go:embed web/styles.css
var stylesCSS []byte

//go:embed web/script.js
var scriptJS []byte

//go:embed web/product.html
var productHTML []byte

//go:embed web/product.js
var productJS []byte

//go:embed web/ollaemporiumbanner.webp
var bannerWebP []byte

//go:embed web/ollamug.webp
var mugWebP []byte

//go:embed web/ollaplate.webp
var plateWebP []byte

type asset struct {
	name        string
	contentType string
	content     []byte
}

func newHandler() http.Handler {
	return newHandlerWithFeatured(newFeaturedHandlerFromEnv())
}

func newHandlerWithFeatured(featured http.Handler) http.Handler {
	store := newProductStoreFromEnv()
	admin := newAdminHandlerFromEnv(store)
	index := asset{"index.html", "text/html; charset=utf-8", indexHTML}
	assets := map[string]asset{
		"/":                        index,
		"/index.html":              index,
		"/styles.css":              {"styles.css", "text/css; charset=utf-8", stylesCSS},
		"/script.js":               {"script.js", "text/javascript; charset=utf-8", scriptJS},
		"/product":                 {"product.html", "text/html; charset=utf-8", productHTML},
		"/product.js":              {"product.js", "text/javascript; charset=utf-8", productJS},
		"/ollaemporiumbanner.webp": {"ollaemporiumbanner.webp", "image/webp", bannerWebP},
		"/ollamug.webp":            {"ollamug.webp", "image/webp", mugWebP},
		"/ollaplate.webp":          {"ollaplate.webp", "image/webp", plateWebP},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/featured-listings":
			featured.ServeHTTP(w, r)
			return
		case r.URL.Path == "/api/products":
			store.serveProducts(w, r)
			return
		case strings.HasPrefix(r.URL.Path, "/product-images/"):
			store.serveImage(w, r)
			return
		case r.URL.Path == "/admin" || strings.HasPrefix(r.URL.Path, "/admin/"):
			admin.ServeHTTP(w, r)
			return
		}
		file, ok := assets[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", file.contentType)
		http.ServeContent(w, r, file.name, time.Time{}, bytes.NewReader(file.content))
	})
}

// Run starts the HTTP server and shuts it down when the process receives a signal.
func Run() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	server := &http.Server{
		Addr:              ":" + port,
		Handler:           newHandler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("Listening on %s", server.Addr)
		errCh <- server.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	case <-ctx.Done():
		stop()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("Graceful shutdown failed: %v", err)
			if err := server.Close(); err != nil {
				log.Printf("Closing server: %v", err)
			}
		}
		if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}
}
