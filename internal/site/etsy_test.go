package site

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func mockEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("ETSY_LISTINGS_MODE", "")
	t.Setenv("ETSY_SHOP_ID", "")
	t.Setenv("ETSY_KEYSTRING", "")
	t.Setenv("ETSY_SHARED_SECRET", "")
	t.Setenv("ETSY_ACCESS_TOKEN", "")
}

func liveHandler(server *httptest.Server) http.Handler {
	return newHandlerWithFeatured(&featuredHandler{
		mode: "etsy",
		etsy: &etsyClient{baseURL: server.URL + "/v3/application", shopID: 123, apiKey: "key:secret", client: newEtsyHTTPClient()},
	})
}

const (
	activeListingsPath = "/v3/application/shops/123/listings/active"
	batchListingsPath  = "/v3/application/listings/batch"
)

func checkEtsyHeaders(t *testing.T, r *http.Request, token string) {
	t.Helper()
	wantAuth := ""
	if token != "" {
		wantAuth = "Bearer " + token
	}
	if r.Method != http.MethodGet || r.Header.Get("x-api-key") != "key:secret" || r.Header.Get("Authorization") != wantAuth || r.Header.Get("Accept") != "application/json" {
		t.Error("unexpected Etsy request method or headers")
	}
}

func checkActiveRequest(t *testing.T, r *http.Request, offset int, token string) {
	t.Helper()
	checkEtsyHeaders(t, r, token)
	if r.URL.Path != activeListingsPath {
		t.Errorf("unexpected Etsy path: %s", r.URL.Path)
	}
	query := r.URL.Query()
	if len(query) != 2 || !reflect.DeepEqual(query["limit"], []string{"100"}) ||
		!reflect.DeepEqual(query["offset"], []string{strconv.Itoa(offset)}) {
		t.Errorf("active listings query = %v, offset = %d", query, offset)
	}
}

func checkBatchRequest(t *testing.T, r *http.Request, token string) []string {
	t.Helper()
	checkEtsyHeaders(t, r, token)
	query := r.URL.Query()
	if r.URL.Path != batchListingsPath || len(query) != 2 || len(query["listing_ids"]) != 1 || !reflect.DeepEqual(query["includes"], []string{"Images"}) {
		t.Errorf("unexpected batch request: %s %v", r.URL.Path, query)
	}
	ids := strings.Split(query.Get("listing_ids"), ",")
	if len(ids) == 0 || len(ids) > 100 {
		t.Errorf("batch contains %d IDs, want 1–100", len(ids))
	}
	seen := make(map[string]bool)
	for _, id := range ids {
		value, err := strconv.ParseInt(id, 10, 64)
		if err != nil || value <= 0 || seen[id] {
			t.Errorf("invalid or duplicate batch ID %q", id)
		}
		seen[id] = true
	}
	return ids
}

func activePage(total, start, end int) string {
	var results []string
	for id := start + 1; id <= end; id++ {
		results = append(results, fmt.Sprintf(`{"listing_id":%d,"title":"Item %d","description":"Description %d","url":"https://www.etsy.com/listing/%d","price":{"amount":%d,"divisor":100,"currency_code":"EUR"}}`, id, id, id, id, id))
	}
	return fmt.Sprintf(`{"count":%d,"results":[%s]}`, total, strings.Join(results, ","))
}

// Reverse response order to verify images are joined by ID, not position.
func imageBatch(ids []string) string {
	var results []string
	for i := len(ids) - 1; i >= 0; i-- {
		results = append(results, fmt.Sprintf(`{"listing_id":%s,"images":[{"rank":1,"url_570xN":"https://i.etsystatic.com/%s.jpg"}]}`, ids[i], ids[i]))
	}
	return fmt.Sprintf(`{"count":%d,"results":[%s]}`, len(ids), strings.Join(results, ","))
}

func requestFeatured(handler http.Handler) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/featured-listings?unused=1", nil))
	return w
}

func decodeFeatured(t *testing.T, w *httptest.ResponseRecorder) featuredResponse {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
	var response featuredResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func TestFeaturedMockAndRouting(t *testing.T) {
	mockEnvironment(t)
	handler := newHandler()
	w := requestFeatured(handler)
	response := decodeFeatured(t, w)
	if response.Source != "mock" || len(response.Listings) != 3 {
		t.Fatalf("unexpected response: %+v", response)
	}
	paths := []string{"/ollamug.webp", "/ollaplate.webp", "/ollaemporiumbanner.webp"}
	var raw struct {
		Listings []map[string]interface{} `json:"listings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for i, item := range response.Listings {
		if item.ID == "" || seen[item.ID] || !strings.Contains(strings.ToLower(item.Title), "demo") || !strings.Contains(strings.ToLower(item.Description), "demo") || item.URL != shopURL || item.ImageURL != paths[i] {
			t.Errorf("unexpected demo item: %+v", item)
		}
		seen[item.ID] = true
		if id, ok := raw.Listings[i]["id"].(string); !ok || id != item.ID {
			t.Errorf("demo id must be a JSON string: %v", raw.Listings[i]["id"])
		}
		if description, ok := raw.Listings[i]["description"].(string); !ok || description != item.Description {
			t.Errorf("demo description must be a JSON string: %v", raw.Listings[i]["description"])
		}
		if _, present := raw.Listings[i]["price"]; present {
			t.Error("demo item should not have a price")
		}
		asset := httptest.NewRecorder()
		handler.ServeHTTP(asset, httptest.NewRequest(http.MethodGet, item.ImageURL, nil))
		if asset.Code != http.StatusOK {
			t.Errorf("demo image %q returned %d", item.ImageURL, asset.Code)
		}
	}
	for _, other := range []http.Handler{handler, newHandler()} {
		if again := decodeFeatured(t, requestFeatured(other)); !reflect.DeepEqual(response, again) {
			t.Errorf("demo listings changed across requests or handlers: %+v", again)
		}
	}
	for _, method := range []string{http.MethodHead, http.MethodPost, http.MethodPut, http.MethodOptions} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(method, "/api/featured-listings", nil))
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET" {
			t.Errorf("%s: status = %d, Allow = %q", method, w.Code, w.Header().Get("Allow"))
		}
	}
	for _, path := range []string{"/api/featured-listings/", "/api/featured-listings-extra", "/api/other"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d", path, w.Code)
		}
	}
}

func TestFeaturedMockIgnoresAccessToken(t *testing.T) {
	mockEnvironment(t)
	t.Setenv("ETSY_ACCESS_TOKEN", "fake-token\r\nInjected: value")
	h := newFeaturedHandlerFromEnv().(*featuredHandler)
	if h.configErr != nil || h.etsy != nil {
		t.Fatal("mock mode should not configure an Etsy client")
	}
	w := requestFeatured(newHandlerWithFeatured(h))
	response := decodeFeatured(t, w)
	if response.Source != "mock" || len(response.Listings) != 3 || strings.Contains(w.Body.String(), "fake-token") {
		t.Error("mock response was altered or exposed the token")
	}
}

func TestFeaturedLive(t *testing.T) {
	var calls int32
	var mu sync.Mutex
	var batchIDs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		switch r.URL.Path {
		case activeListingsPath:
			checkActiveRequest(t, r, 0, "")
			fmt.Fprint(w, `{"count":2,"results":[{"listing_id":42,"title":"Tea & mug","description":"  Handmade & unique\nCafé <mug>  ","url":"https://www.etsy.com/listing/42","price":{"amount":12345,"divisor":100,"currency_code":"EUR"}},{"listing_id":9007199254740993,"title":"Plate","description":"A plate description","url":"https://www.etsy.com/listing/27","price":{"amount":999,"divisor":1000,"currency_code":"USD"}}]}`)
		case batchListingsPath:
			ids := checkBatchRequest(t, r, "")
			mu.Lock()
			batchIDs = append(batchIDs, ids...)
			mu.Unlock()
			var results []string
			for i := len(ids) - 1; i >= 0; i-- {
				switch ids[i] {
				case "42":
					results = append(results, `{"listing_id":42,"images":[{"rank":2,"url_570xN":"https://i.etsystatic.com/second.jpg"},{"rank":1,"url_570xN":"https://i.etsystatic.com/first.jpg"}]}`)
				case "9007199254740993":
					results = append(results, `{"listing_id":9007199254740993,"images":[]}`)
				default:
					t.Errorf("unexpected batch ID %q", ids[i])
				}
			}
			fmt.Fprintf(w, `{"count":%d,"results":[%s]}`, len(results), strings.Join(results, ","))
		default:
			t.Errorf("unexpected Etsy endpoint %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	w := requestFeatured(liveHandler(server))
	response := decodeFeatured(t, w)
	want := featuredResponse{Source: "etsy", Listings: []featuredListing{
		{ID: "42", Title: "Tea & mug", Description: "  Handmade & unique\nCafé <mug>  ", URL: "https://www.etsy.com/listing/42", ImageURL: "https://i.etsystatic.com/first.jpg", Price: "EUR 123.45"},
		{ID: "9007199254740993", Title: "Plate", Description: "A plate description", URL: "https://www.etsy.com/listing/27", ImageURL: "", Price: "USD 0.999"},
	}}
	if response.Source != want.Source || len(response.Listings) != len(want.Listings) {
		t.Fatalf("response = %+v, want %+v", response, want)
	}
	byID := make(map[string]featuredListing)
	for _, item := range response.Listings {
		byID[item.ID] = item
	}
	for _, item := range want.Listings {
		if byID[item.ID] != item {
			t.Errorf("listing %s = %+v, want %+v", item.ID, byID[item.ID], item)
		}
	}
	var raw struct {
		Listings []map[string]interface{} `json:"listings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for i, item := range response.Listings {
		if raw.Listings[i]["id"] != item.ID || raw.Listings[i]["description"] != item.Description {
			t.Errorf("listing JSON = %v, want string id %q and description %q", raw.Listings[i], item.ID, item.Description)
		}
	}
	mu.Lock()
	seen := make(map[string]int)
	for _, id := range batchIDs {
		seen[id]++
	}
	mu.Unlock()
	if !reflect.DeepEqual(seen, map[string]int{"42": 1, "9007199254740993": 1}) {
		t.Errorf("image batch IDs = %v", seen)
	}
	if got := atomic.LoadInt32(&calls); got < 2 || got > 3 {
		t.Errorf("upstream requests = %d, want one page and one or two batches", got)
	}
	if strings.Contains(w.Body.String(), "key:secret") || w.Header().Get("Cache-Control") != "no-store" {
		t.Error("response leaked credentials or omitted no-store")
	}
}

func TestFeaturedActivePagination(t *testing.T) {
	for _, total := range []int{100, 101, 200, 205} {
		t.Run(strconv.Itoa(total), func(t *testing.T) {
			var mu sync.Mutex
			var offsets []int
			var batches [][]string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case activeListingsPath:
					offset, err := strconv.Atoi(r.URL.Query().Get("offset"))
					if err != nil {
						t.Errorf("invalid offset: %v", err)
						http.Error(w, "invalid offset", http.StatusBadRequest)
						return
					}
					checkActiveRequest(t, r, offset, "12345.token")
					mu.Lock()
					offsets = append(offsets, offset)
					mu.Unlock()
					if offset < 0 || offset >= total || offset%100 != 0 {
						http.Error(w, "unexpected offset", http.StatusBadRequest)
						return
					}
					end := offset + 100
					if end > total {
						end = total
					}
					fmt.Fprint(w, activePage(total, offset, end))
				case batchListingsPath:
					ids := checkBatchRequest(t, r, "12345.token")
					mu.Lock()
					batches = append(batches, ids)
					mu.Unlock()
					fmt.Fprint(w, imageBatch(ids))
				default:
					t.Errorf("unexpected Etsy endpoint %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			h := &featuredHandler{mode: "etsy", etsy: &etsyClient{
				baseURL: server.URL + "/v3/application", shopID: 123, apiKey: "key:secret", accessToken: "12345.token", client: newEtsyHTTPClient(),
			}}
			handler := newHandlerWithFeatured(h)
			first := decodeFeatured(t, requestFeatured(handler))
			if first.Source != "etsy" || len(first.Listings) != total {
				t.Fatalf("got %d listings from %q, want %d", len(first.Listings), first.Source, total)
			}
			listings := make(map[string]featuredListing)
			for _, item := range first.Listings {
				if _, exists := listings[item.ID]; exists {
					t.Errorf("duplicate listing ID %q", item.ID)
				}
				listings[item.ID] = item
			}
			for id := 1; id <= total; id++ {
				want := featuredListing{
					ID: strconv.Itoa(id), Title: fmt.Sprintf("Item %d", id), Description: fmt.Sprintf("Description %d", id),
					URL: fmt.Sprintf("https://www.etsy.com/listing/%d", id), ImageURL: fmt.Sprintf("https://i.etsystatic.com/%d.jpg", id),
					Price: fmt.Sprintf("EUR %d.%02d", id/100, id%100),
				}
				if item := listings[want.ID]; item != want {
					t.Errorf("listing %d = %+v, want %+v", id, item, want)
				}
			}
			second := decodeFeatured(t, requestFeatured(handler))
			if !reflect.DeepEqual(first, second) {
				t.Error("cached response differs from the full paginated response")
			}
			var wantOffsets []int
			for offset := 0; offset < total; offset += 100 {
				wantOffsets = append(wantOffsets, offset)
			}
			mu.Lock()
			defer mu.Unlock()
			if !reflect.DeepEqual(offsets, wantOffsets) {
				t.Errorf("upstream offsets = %v, want %v", offsets, wantOffsets)
			}
			if len(batches) < (total+99)/100 {
				t.Errorf("got %d image batches for %d listings", len(batches), total)
			}
			seen := make(map[string]bool)
			for _, batch := range batches {
				for _, id := range batch {
					if seen[id] {
						t.Errorf("listing %s appeared in multiple image batches", id)
					}
					seen[id] = true
				}
			}
			for id := 1; id <= total; id++ {
				if !seen[strconv.Itoa(id)] {
					t.Errorf("listing %d missing from image batches", id)
				}
			}
			if len(seen) != total {
				t.Errorf("got %d distinct batch IDs, want %d", len(seen), total)
			}
		})
	}
}

func TestActiveListingsShortPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case activeListingsPath:
			switch r.URL.Query().Get("offset") {
			case "0":
				fmt.Fprint(w, activePage(3, 0, 2))
			case "2":
				fmt.Fprint(w, activePage(3, 2, 3))
			default:
				t.Errorf("unexpected offset %q", r.URL.Query().Get("offset"))
				http.Error(w, "unexpected offset", http.StatusBadRequest)
			}
		case batchListingsPath:
			fmt.Fprint(w, imageBatch(checkBatchRequest(t, r, "")))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	response := decodeFeatured(t, requestFeatured(liveHandler(server)))
	if len(response.Listings) != 3 || response.Listings[2].ID != "3" {
		t.Fatalf("short page lost listings: %+v", response.Listings)
	}
}

func TestActiveListingsOffsetLimit(t *testing.T) {
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		checkActiveRequest(t, r, 0, "")
		fmt.Fprint(w, activePage(12101, 0, 1))
	}))
	defer server.Close()
	response := requestFeatured(liveHandler(server))
	if response.Code != http.StatusBadGateway || strings.Contains(response.Body.String(), `"listings"`) || atomic.LoadInt32(&requests) != 1 {
		t.Fatalf("offset limit: status=%d body=%s requests=%d", response.Code, response.Body.String(), atomic.LoadInt32(&requests))
	}
}

func TestFeaturedLaterPageFailureNotCached(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
	}{
		{"upstream error", http.StatusServiceUnavailable, "secret error details"},
		{"truncated json", http.StatusOK, `{"count":101,"results":[`},
		{"premature empty page", http.StatusOK, `{"count":101,"results":[]}`},

		{"count below accumulated results", http.StatusOK, activePage(100, 100, 101)},
		{"duplicate across pages", http.StatusOK, activePage(101, 0, 1)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var offsets []int
			var batches int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == batchListingsPath {
					ids := checkBatchRequest(t, r, "")
					mu.Lock()
					batches++
					mu.Unlock()
					fmt.Fprint(w, imageBatch(ids))
					return
				}
				offset, err := strconv.Atoi(r.URL.Query().Get("offset"))
				if err != nil {
					t.Errorf("invalid offset: %v", err)
					http.Error(w, "invalid offset", http.StatusBadRequest)
					return
				}
				checkActiveRequest(t, r, offset, "")
				mu.Lock()
				offsets = append(offsets, offset)
				call := len(offsets)
				mu.Unlock()
				if r.URL.Path != activeListingsPath {
					http.NotFound(w, r)
					return
				}
				switch offset {
				case 0:
					fmt.Fprint(w, activePage(101, 0, 100))
				case 100:
					if call == 2 {
						w.WriteHeader(tt.status)
						fmt.Fprint(w, tt.body)
					} else {
						fmt.Fprint(w, activePage(101, 100, 101))
					}
				default:
					http.Error(w, "unexpected offset", http.StatusBadRequest)
				}
			}))
			defer server.Close()
			handler := liveHandler(server)
			failed := requestFeatured(handler)
			if failed.Code != http.StatusBadGateway || strings.Contains(failed.Body.String(), "secret") || strings.Contains(failed.Body.String(), `"listings"`) {
				t.Errorf("partial refresh: status=%d body=%s", failed.Code, failed.Body.String())
			}
			retry := decodeFeatured(t, requestFeatured(handler))
			if len(retry.Listings) != 101 {
				t.Fatalf("retry returned %d listings, want 101", len(retry.Listings))
			}
			seen := make(map[string]bool)
			for _, item := range retry.Listings {
				seen[item.ID] = item.ImageURL == "https://i.etsystatic.com/"+item.ID+".jpg"
			}
			for id := 1; id <= 101; id++ {
				if !seen[strconv.Itoa(id)] {
					t.Errorf("retry missing listing or image for %d", id)
				}
			}
			mu.Lock()
			batchesAfterRetry := batches
			mu.Unlock()
			again := decodeFeatured(t, requestFeatured(handler))
			if !reflect.DeepEqual(retry, again) {
				t.Error("successful retry was not cached")
			}
			mu.Lock()
			defer mu.Unlock()
			if batches != batchesAfterRetry {
				t.Errorf("cached response fetched more image batches: before=%d after=%d", batchesAfterRetry, batches)
			}
			if want := []int{0, 100, 0, 100}; !reflect.DeepEqual(offsets, want) {
				t.Errorf("upstream offsets = %v, want %v", offsets, want)
			}
			if batches < 2 {
				t.Errorf("expected multiple image batches after retry, got %d", batches)
			}
		})
	}
}

func TestFeaturedIDsStableAfterRefresh(t *testing.T) {
	var activeCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case activeListingsPath:
			checkActiveRequest(t, r, 0, "")
			if atomic.AddInt32(&activeCalls, 1) == 1 {
				fmt.Fprint(w, `{"count":2,"results":[{"listing_id":42,"title":"First","description":"Original","url":"https://www.etsy.com/listing/42"},{"listing_id":27,"title":"Second","description":"Second description","url":"https://www.etsy.com/listing/27"}]}`)
			} else {
				fmt.Fprint(w, `{"count":2,"results":[{"listing_id":27,"title":"Second","description":"Second description","url":"https://www.etsy.com/listing/27"},{"listing_id":42,"title":"Renamed","description":"Updated","url":"https://www.etsy.com/listing/42"}]}`)
			}
		case batchListingsPath:
			ids := checkBatchRequest(t, r, "")
			fmt.Fprint(w, imageBatch(ids))
		default:
			t.Errorf("unexpected Etsy endpoint %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	feature := &featuredHandler{mode: "etsy", etsy: &etsyClient{
		baseURL: server.URL + "/v3/application", shopID: 123, apiKey: "key:secret", client: newEtsyHTTPClient(),
	}}
	handler := newHandlerWithFeatured(feature)
	first := decodeFeatured(t, requestFeatured(handler))
	feature.mu.Lock()
	feature.cacheUntil = time.Time{}
	feature.mu.Unlock()
	second := decodeFeatured(t, requestFeatured(handler))
	if len(first.Listings) != 2 || len(second.Listings) != 2 {
		t.Fatalf("unexpected listings: first=%+v second=%+v", first, second)
	}
	firstByID := make(map[string]featuredListing)
	secondByID := make(map[string]featuredListing)
	for _, item := range first.Listings {
		firstByID[item.ID] = item
	}
	for _, item := range second.Listings {
		secondByID[item.ID] = item
	}
	if len(firstByID) != 2 || len(secondByID) != 2 || firstByID["42"].Title != "First" || firstByID["27"].Title != "Second" || secondByID["27"].Title != "Second" || secondByID["42"].Title != "Renamed" || secondByID["42"].Description != "Updated" {
		t.Errorf("IDs changed with listing order or text: first=%+v second=%+v", first, second)
	}
	if atomic.LoadInt32(&activeCalls) != 2 {
		t.Errorf("active calls = %d, want 2", atomic.LoadInt32(&activeCalls))
	}
}

func TestFeaturedDescriptionOptional(t *testing.T) {
	for _, tt := range []struct {
		name  string
		field string
		want  string
	}{
		{"missing", "", ""},
		{"null", `,"description":null`, ""},
		{"empty", `,"description":""`, ""},
		{"whitespace", `,"description":"  \n "`, "  \n "},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case activeListingsPath:
					checkActiveRequest(t, r, 0, "")
					fmt.Fprintf(w, `{"count":1,"results":[{"listing_id":42,"title":"Item","url":"https://www.etsy.com/listing/42"%s}]}`, tt.field)
				case batchListingsPath:
					checkBatchRequest(t, r, "")
					fmt.Fprint(w, `{"count":1,"results":[{"listing_id":42,"description":"not the active description","images":[]}]}`)
				default:
					t.Errorf("unexpected Etsy endpoint %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			w := requestFeatured(liveHandler(server))
			response := decodeFeatured(t, w)
			if response.Source != "etsy" || len(response.Listings) != 1 || response.Listings[0].ID != "42" || response.Listings[0].Description != tt.want {
				t.Fatalf("unexpected response: %+v", response)
			}
			var raw struct {
				Listings []map[string]interface{} `json:"listings"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
				t.Fatal(err)
			}
			if raw.Listings[0]["description"] != tt.want {
				t.Errorf("description JSON = %v, want string %q", raw.Listings[0]["description"], tt.want)
			}
		})
	}
}

func TestFeaturedAccessToken(t *testing.T) {
	for _, tt := range []struct {
		name  string
		token string
	}{
		{"absent", ""},
		{"configured", "12345.fake-token_~+/=="},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ETSY_LISTINGS_MODE", "etsy")
			t.Setenv("ETSY_SHOP_ID", "123")
			t.Setenv("ETSY_KEYSTRING", "key")
			t.Setenv("ETSY_SHARED_SECRET", "secret")
			t.Setenv("ETSY_ACCESS_TOKEN", tt.token)
			var calls, fail int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&calls, 1)
				switch r.URL.Path {
				case activeListingsPath:
					checkActiveRequest(t, r, 0, tt.token)
					if atomic.LoadInt32(&fail) != 0 {
						http.Error(w, "upstream error: "+tt.token, http.StatusUnauthorized)
						return
					}
					fmt.Fprint(w, `{"count":1,"results":[{"listing_id":1,"title":"Item","url":"https://www.etsy.com/listing/1"}]}`)
				case batchListingsPath:
					checkBatchRequest(t, r, tt.token)
					fmt.Fprint(w, `{"count":1,"results":[{"listing_id":1,"images":[]}]}`)
				default:
					t.Errorf("unexpected Etsy endpoint %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			h := newFeaturedHandlerFromEnv().(*featuredHandler)
			if h.configErr != nil || h.etsy == nil {
				t.Fatal("valid configuration rejected")
			}
			h.etsy.baseURL = server.URL + "/v3/application"
			handler := newHandlerWithFeatured(h)
			w := requestFeatured(handler)
			response := decodeFeatured(t, w)
			if response.Source != "etsy" || len(response.Listings) != 1 || atomic.LoadInt32(&calls) != 2 {
				t.Error("expected active listings and image batch requests with a live response")
			}
			if tt.token != "" && (strings.Contains(w.Body.String(), tt.token) || strings.Contains(fmt.Sprint(w.Header()), tt.token)) {
				t.Error("response exposed access token")
			}
			atomic.StoreInt32(&fail, 1)
			h.mu.Lock()
			h.cacheUntil = time.Time{}
			h.mu.Unlock()
			failed := requestFeatured(handler)
			if failed.Code != http.StatusBadGateway || (tt.token != "" && strings.Contains(failed.Body.String(), tt.token)) {
				t.Error("upstream failure should not expose access token")
			}
		})
	}
}

func TestFeaturedInvalidAccessToken(t *testing.T) {
	for _, tt := range []struct{ name, token string }{
		{"newline", "fake-token\nInjected: value"},
		{"carriage return", "fake-token\rInjected: value"},
		{"tab", "fake-token\tvalue"},
		{"leading space", " fake-token"},
		{"trailing newline", "fake-token\n"},
		{"padding in middle", "fake=token"},
		{"padding only", "="},
		{"non-ascii", "fake-é"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ETSY_LISTINGS_MODE", "etsy")
			t.Setenv("ETSY_SHOP_ID", "123")
			t.Setenv("ETSY_KEYSTRING", "key")
			t.Setenv("ETSY_SHARED_SECRET", "secret")
			t.Setenv("ETSY_ACCESS_TOKEN", tt.token)
			h := newFeaturedHandlerFromEnv().(*featuredHandler)
			if h.configErr == nil || h.etsy != nil {
				t.Fatal("invalid token accepted or Etsy client created")
			}
			w := requestFeatured(newHandlerWithFeatured(h))
			if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "fake") || strings.Contains(w.Body.String(), "secret") {
				t.Error("invalid token should produce a generic configuration error")
			}
		})
	}
	if validBearerToken("fake\x00token") || validBearerToken("fake\x7ftoken") {
		t.Error("control characters must be rejected")
	}
}

func TestFeaturedLiveEmpty(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		checkActiveRequest(t, r, 0, "")
		fmt.Fprint(w, `{"count":0,"results":[]}`)
	}))
	defer server.Close()
	w := requestFeatured(liveHandler(server))
	response := decodeFeatured(t, w)
	if response.Source != "etsy" || response.Listings == nil || len(response.Listings) != 0 {
		t.Errorf("unexpected response: %+v", response)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("upstream requests = %d, want one", got)
	}
}

func TestFeaturedEtsyErrors(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		status int
	}{
		{"unauthorized", "secret internal details", http.StatusUnauthorized},
		{"rate limited", "secret internal details", http.StatusTooManyRequests},
		{"invalid json", `{`, http.StatusOK},
		{"truncated json", `{"count":1,"results":[{"listing_id":42`, http.StatusOK},
		{"trailing json", `{"count":0,"results":[]} {}`, http.StatusOK},
		{"missing results", `{"count":0}`, http.StatusOK},
		{"null results", `{"count":0,"results":null}`, http.StatusOK},
		{"missing count", `{"results":[]}`, http.StatusOK},
		{"null count", `{"count":null,"results":[]}`, http.StatusOK},
		{"negative count", `{"count":-1,"results":[]}`, http.StatusOK},
		{"invalid count type", `{"count":"one","results":[]}`, http.StatusOK},
		{"empty before count", `{"count":1,"results":[]}`, http.StatusOK},
		{"results exceed count", `{"count":0,"results":[{"listing_id":42,"title":"Item","url":"https://www.etsy.com/listing/42"}]}`, http.StatusOK},
		{"bad listing id", `{"count":1,"results":[{"listing_id":0}]}`, http.StatusOK},
		{"negative listing id", `{"count":1,"results":[{"listing_id":-1,"title":"Item","url":"https://www.etsy.com/listing/1"}]}`, http.StatusOK},
		{"duplicate listing id", `{"count":2,"results":[{"listing_id":42,"title":"Item","url":"https://www.etsy.com/listing/42"},{"listing_id":42,"title":"Other","url":"https://www.etsy.com/listing/42"}]}`, http.StatusOK},
		{"non-numeric listing id", `{"count":1,"results":[{"listing_id":"42","title":"Item","url":"https://www.etsy.com/listing/42"}]}`, http.StatusOK},
		{"invalid description type", `{"count":1,"results":[{"listing_id":42,"title":"Item","description":42,"url":"https://www.etsy.com/listing/42"}]}`, http.StatusOK},
		{"bad url", `{"count":1,"results":[{"listing_id":42,"title":"Item","url":"javascript:alert(1)"}]}`, http.StatusOK},
		{"bad price", `{"count":1,"results":[{"listing_id":42,"title":"Item","url":"https://www.etsy.com/listing/42","price":{"amount":1,"divisor":0,"currency_code":"EUR"}}]}`, http.StatusOK},
		{"too many results in a page", activePage(101, 0, 101), http.StatusOK},
		{"oversized response", strings.Repeat(" ", etsyMaxBody+1), http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				checkActiveRequest(t, r, 0, "")
				w.WriteHeader(tt.status)
				fmt.Fprint(w, tt.body)
			}))
			defer server.Close()
			w := requestFeatured(liveHandler(server))
			if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), `"source"`) || w.Header().Get("Cache-Control") != "no-store" {
				t.Errorf("status = %d, Cache-Control = %q, body = %q", w.Code, w.Header().Get("Cache-Control"), w.Body.String())
			}
			var result map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result["error"] == "" {
				t.Errorf("expected JSON error, got %q", w.Body.String())
			}
		})
	}
}

type etsyDiagnosticLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *etsyDiagnosticLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *etsyDiagnosticLog) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The standard logger is process-wide; callers must not run in parallel.
func captureEtsyDiagnosticLog(t *testing.T) *etsyDiagnosticLog {
	t.Helper()
	output, flags, prefix := log.Writer(), log.Flags(), log.Prefix()
	var captured etsyDiagnosticLog
	log.SetOutput(&captured)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(output)
		log.SetFlags(flags)
		log.SetPrefix(prefix)
	})
	return &captured
}

type etsyDiagnosticTransport func(*http.Request) (*http.Response, error)

func (f etsyDiagnosticTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestFeaturedRefreshFailureDiagnostics(t *testing.T) {
	const (
		apiKey      = "diagnostic-key:diagnostic-shared-secret"
		token       = "diagnostic-access-token"
		bodyMarker  = "private-upstream-body"
		title       = "private-listing-title"
		description = "private-listing-description"
		listingURL  = "https://private-listing.example/hidden"
	)
	privateBody := bodyMarker + " " + apiKey + " " + token + " " + listingURL
	activeListing := fmt.Sprintf(`{"count":1,"results":[{"listing_id":8675309,"title":%q,"description":%q,"url":%q}]}`, title, description, listingURL)
	tests := []struct {
		name       string
		activeBody string
		batchBody  string
		status     int
		network    bool
		diagnostic string
		validation bool
	}{
		{name: "active listings forbidden", activeBody: privateBody, status: http.StatusForbidden, diagnostic: "active listings: Etsy API returned HTTP 403"},
		{name: "image batch rate limited", activeBody: activeListing, batchBody: privateBody, status: http.StatusTooManyRequests, diagnostic: "image batch: Etsy API returned HTTP 429"},
		{name: "malformed active listings JSON", activeBody: `{"private":"` + privateBody + `","bad":}`, status: http.StatusOK, diagnostic: "active listings: invalid JSON response"},
		{name: "active listings network error", network: true, diagnostic: "active listings: network error"},
		{name: "invalid listing price", activeBody: fmt.Sprintf(`{"count":1,"results":[{"listing_id":8675309,"title":%q,"description":%q,"url":%q,"price":{"amount":1,"divisor":0,"currency_code":"EUR"}}]}`, title, description, listingURL), status: http.StatusOK, diagnostic: "invalid Etsy price", validation: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			captured := captureEtsyDiagnosticLog(t)
			var pages, batches, networkCalls int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("x-api-key") != apiKey || r.Header.Get("Authorization") != "Bearer "+token {
					t.Error("missing Etsy credentials on mock request")
				}
				switch r.URL.Path {
				case activeListingsPath:
					atomic.AddInt32(&pages, 1)
					if tt.batchBody == "" {
						w.WriteHeader(tt.status)
					}
					fmt.Fprint(w, tt.activeBody)
				case batchListingsPath:
					atomic.AddInt32(&batches, 1)
					w.WriteHeader(tt.status)
					fmt.Fprint(w, tt.batchBody)
				default:
					t.Errorf("unexpected Etsy endpoint: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client := newEtsyHTTPClient()
			if tt.network {
				client.Transport = etsyDiagnosticTransport(func(r *http.Request) (*http.Response, error) {
					atomic.AddInt32(&networkCalls, 1)
					return nil, errors.New("connection failed for " + r.URL.String() + " " + privateBody)
				})
			}
			handler := newHandlerWithFeatured(&featuredHandler{mode: "etsy", etsy: &etsyClient{
				baseURL: server.URL + "/v3/application", shopID: 123, apiKey: apiKey, accessToken: token, client: client,
			}})

			for attempt := 1; attempt <= 2; attempt++ {
				w := requestFeatured(handler)
				if w.Code != http.StatusBadGateway || w.Body.String() != "{\"error\":\"Etsy listings are unavailable\"}\n" || w.Header().Get("Cache-Control") != "no-store" {
					t.Errorf("attempt %d: status = %d, Cache-Control = %q, body = %q", attempt, w.Code, w.Header().Get("Cache-Control"), w.Body.String())
				}
				logs := captured.String()
				lines := strings.Split(strings.TrimSuffix(logs, "\n"), "\n")
				if len(lines) != attempt || !strings.HasSuffix(logs, "\n") {
					t.Errorf("attempt %d: expected one log per failed refresh, got %q", attempt, logs)
				}
				for _, line := range lines {
					if tt.validation {
						if line != "Etsy listings refresh failed: "+tt.diagnostic && line != "Etsy listings refresh failed: active listings: "+tt.diagnostic {
							t.Errorf("unexpected validation diagnostic: %q", line)
						}
					} else if line != "Etsy listings refresh failed: "+tt.diagnostic {
						t.Errorf("unexpected diagnostic: %q", line)
					}
				}
				for _, forbidden := range []string{apiKey, "diagnostic-key", "diagnostic-shared-secret", token, bodyMarker, title, description, "8675309", server.URL, "://"} {
					if strings.Contains(logs, forbidden) || strings.Contains(w.Body.String(), forbidden) {
						t.Errorf("attempt %d: sensitive value %q appeared in log or response", attempt, forbidden)
					}
				}
			}
			if tt.network {
				if got := atomic.LoadInt32(&networkCalls); got != 2 {
					t.Errorf("network calls = %d, want 2", got)
				}
			} else if got := atomic.LoadInt32(&pages); got != 2 {
				t.Errorf("active listings requests = %d, want 2", got)
			}
			if tt.batchBody != "" && atomic.LoadInt32(&batches) != 2 {
				t.Errorf("image batch requests = %d, want 2", atomic.LoadInt32(&batches))
			}
		})
	}
}

type timeoutEtsyBody struct{}

func (timeoutEtsyBody) Read([]byte) (int, error) { return 0, context.DeadlineExceeded }
func (timeoutEtsyBody) Close() error             { return nil }

func TestFeaturedResponseBodyTimeout(t *testing.T) {
	captured := captureEtsyDiagnosticLog(t)
	client := newEtsyHTTPClient()
	client.Transport = etsyDiagnosticTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: timeoutEtsyBody{}}, nil
	})
	handler := newHandlerWithFeatured(&featuredHandler{mode: "etsy", etsy: &etsyClient{
		baseURL: "https://api.etsy.com/v3/application", shopID: 123, apiKey: "key:secret", client: client,
	}})
	response := requestFeatured(handler)
	if response.Code != http.StatusGatewayTimeout {
		t.Errorf("body timeout status = %d, want %d", response.Code, http.StatusGatewayTimeout)
	}
	if got := captured.String(); got != "Etsy listings refresh failed: active listings: error reading Etsy response\n" {
		t.Errorf("body timeout log = %q", got)
	}
}

func TestFeaturedSharedRefreshFailureLoggedOnce(t *testing.T) {
	captured := captureEtsyDiagnosticLog(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			close(started)
			<-release
		}
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "private upstream body https://private.example/secret")
	}))
	var requests sync.WaitGroup
	defer func() {
		releaseOnce.Do(func() { close(release) })
		requests.Wait()
		server.Close()
	}()
	h := &featuredHandler{mode: "etsy", etsy: &etsyClient{
		baseURL: server.URL + "/v3/application", shopID: 123, apiKey: "key:secret", client: newEtsyHTTPClient(),
	}}
	handler := newHandlerWithFeatured(h)
	responses := make(chan *httptest.ResponseRecorder, 2)
	request := func() {
		defer requests.Done()
		responses <- requestFeatured(handler)
	}
	requests.Add(1)
	go request()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request did not start")
	}
	requests.Add(1)
	go request()
	waitForFeaturedWaiters(t, h, 2)
	releaseOnce.Do(func() { close(release) })
	for i := 0; i < 2; i++ {
		select {
		case w := <-responses:
			if w.Code != http.StatusBadGateway || w.Body.String() != "{\"error\":\"Etsy listings are unavailable\"}\n" {
				t.Errorf("shared refresh response: status = %d, body = %q", w.Code, w.Body.String())
			}
		case <-time.After(2 * time.Second):
			t.Fatal("shared refresh did not complete")
		}
	}
	const entry = "Etsy listings refresh failed: active listings: Etsy API returned HTTP 403\n"
	if got := captured.String(); got != entry {
		t.Errorf("shared refresh log = %q, want %q", got, entry)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("shared refresh upstream calls = %d, want 1", got)
	}
	if w := requestFeatured(handler); w.Code != http.StatusBadGateway || w.Body.String() != "{\"error\":\"Etsy listings are unavailable\"}\n" {
		t.Errorf("retry response: status = %d, body = %q", w.Code, w.Body.String())
	}
	if got := captured.String(); got != entry+entry {
		t.Errorf("retry log = %q, want two entries", got)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("upstream calls after retry = %d, want 2", got)
	}
}

func TestFeaturedImageBatchErrorsNotCached(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
	}{
		{"unauthorized", http.StatusUnauthorized, "secret internal details"},
		{"rate limited", http.StatusTooManyRequests, "secret internal details"},
		{"invalid json", http.StatusOK, `{`},
		{"truncated json", http.StatusOK, `{"count":1,"results":[{"listing_id":42`},
		{"trailing json", http.StatusOK, `{"count":1,"results":[{"listing_id":42,"images":[]}]} {}`},
		{"missing results", http.StatusOK, `{"count":1}`},
		{"null results", http.StatusOK, `{"count":1,"results":null}`},
		{"missing count", http.StatusOK, `{"results":[{"listing_id":42,"images":[]}]}`},
		{"null count", http.StatusOK, `{"count":null,"results":[{"listing_id":42,"images":[]}]}`},
		{"negative count", http.StatusOK, `{"count":-1,"results":[]}`},
		{"invalid count", http.StatusOK, `{"count":"one","results":[]}`},
		{"count exceeds results", http.StatusOK, `{"count":2,"results":[{"listing_id":42,"images":[]}]}`},
		{"results exceed count", http.StatusOK, `{"count":0,"results":[{"listing_id":42,"images":[]}]}`},
		{"missing requested id", http.StatusOK, `{"count":0,"results":[]}`},
		{"duplicate id", http.StatusOK, `{"count":2,"results":[{"listing_id":42,"images":[]},{"listing_id":42,"images":[]}]}`},
		{"unexpected id", http.StatusOK, `{"count":2,"results":[{"listing_id":42,"images":[]},{"listing_id":43,"images":[]}]}`},
		{"missing id", http.StatusOK, `{"count":1,"results":[{"images":[]}]}`},
		{"invalid id type", http.StatusOK, `{"count":1,"results":[{"listing_id":"42","images":[]}]}`},
		{"invalid images type", http.StatusOK, `{"count":1,"results":[{"listing_id":42,"images":"image"}]}`},
		{"invalid image rank type", http.StatusOK, `{"count":1,"results":[{"listing_id":42,"images":[{"rank":"first"}]}]}`},
		{"invalid image url type", http.StatusOK, `{"count":1,"results":[{"listing_id":42,"images":[{"rank":1,"url_570xN":123}]}]}`},
		{"oversized response", http.StatusOK, strings.Repeat(" ", etsyMaxBody+1)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var pages, batches int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case activeListingsPath:
					checkActiveRequest(t, r, 0, "")
					atomic.AddInt32(&pages, 1)
					fmt.Fprint(w, `{"count":1,"results":[{"listing_id":42,"title":"Item","url":"https://www.etsy.com/listing/42"}]}`)
				case batchListingsPath:
					ids := checkBatchRequest(t, r, "")
					if !reflect.DeepEqual(ids, []string{"42"}) {
						t.Errorf("batch IDs = %v, want [42]", ids)
					}
					if atomic.AddInt32(&batches, 1) == 1 {
						w.WriteHeader(tt.status)
						fmt.Fprint(w, tt.body)
						return
					}
					fmt.Fprint(w, imageBatch(ids))
				default:
					t.Errorf("unexpected Etsy endpoint %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			handler := liveHandler(server)
			failed := requestFeatured(handler)
			if failed.Code != http.StatusBadGateway || failed.Header().Get("Cache-Control") != "no-store" || strings.Contains(failed.Body.String(), "secret") || strings.Contains(failed.Body.String(), `"listings"`) {
				t.Errorf("partial batch response: status=%d body=%s", failed.Code, failed.Body.String())
			}
			var result map[string]string
			if err := json.Unmarshal(failed.Body.Bytes(), &result); err != nil || result["error"] == "" {
				t.Errorf("expected JSON error, got %q", failed.Body.String())
			}
			retry := decodeFeatured(t, requestFeatured(handler))
			if retry.Source != "etsy" || len(retry.Listings) != 1 || retry.Listings[0].ID != "42" || retry.Listings[0].ImageURL != "https://i.etsystatic.com/42.jpg" {
				t.Fatalf("retry returned incomplete listings: %+v", retry)
			}
			again := decodeFeatured(t, requestFeatured(handler))
			if !reflect.DeepEqual(retry, again) || atomic.LoadInt32(&pages) != 2 || atomic.LoadInt32(&batches) != 2 {
				t.Errorf("failed response was cached or successful retry was not: pages=%d batches=%d", atomic.LoadInt32(&pages), atomic.LoadInt32(&batches))
			}
		})
	}
}

func TestFeaturedLaterImageBatchFailureNotCached(t *testing.T) {
	const total = 205
	var mu sync.Mutex
	var phase int
	var offsets [2][]int
	var batches [2][][]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case activeListingsPath:
			offset, err := strconv.Atoi(r.URL.Query().Get("offset"))
			if err != nil || offset < 0 || offset >= total || offset%100 != 0 {
				t.Errorf("invalid active offset %q", r.URL.Query().Get("offset"))
				http.Error(w, "invalid offset", http.StatusBadRequest)
				return
			}
			checkActiveRequest(t, r, offset, "")
			mu.Lock()
			offsets[phase] = append(offsets[phase], offset)
			mu.Unlock()
			end := offset + 100
			if end > total {
				end = total
			}
			fmt.Fprint(w, activePage(total, offset, end))
		case batchListingsPath:
			ids := checkBatchRequest(t, r, "")
			mu.Lock()
			currentPhase := phase
			batches[currentPhase] = append(batches[currentPhase], ids)
			fail := currentPhase == 0 && len(batches[currentPhase]) == 2
			mu.Unlock()
			if fail {
				http.Error(w, "secret internal details", http.StatusServiceUnavailable)
				return
			}
			fmt.Fprint(w, imageBatch(ids))
		default:
			t.Errorf("unexpected Etsy endpoint %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	handler := liveHandler(server)
	failed := requestFeatured(handler)
	if failed.Code != http.StatusBadGateway || failed.Header().Get("Cache-Control") != "no-store" || strings.Contains(failed.Body.String(), "secret") || strings.Contains(failed.Body.String(), `"listings"`) {
		t.Errorf("partial batch response: status=%d body=%s", failed.Code, failed.Body.String())
	}
	mu.Lock()
	phase = 1
	mu.Unlock()
	retry := decodeFeatured(t, requestFeatured(handler))
	if retry.Source != "etsy" || len(retry.Listings) != total {
		t.Fatalf("retry returned %d listings from %q, want %d", len(retry.Listings), retry.Source, total)
	}
	seen := make(map[string]bool)
	for _, item := range retry.Listings {
		if seen[item.ID] {
			t.Errorf("duplicate listing %q", item.ID)
		}
		seen[item.ID] = item.ImageURL == "https://i.etsystatic.com/"+item.ID+".jpg"
	}
	for id := 1; id <= total; id++ {
		if !seen[strconv.Itoa(id)] {
			t.Errorf("retry missing listing or image for %d", id)
		}
	}
	mu.Lock()
	pagesAfterRetry, batchesAfterRetry := len(offsets[1]), len(batches[1])
	mu.Unlock()
	again := decodeFeatured(t, requestFeatured(handler))
	if !reflect.DeepEqual(retry, again) {
		t.Error("successful retry was not cached")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(batches[0]) < 2 {
		t.Errorf("first attempt did not reach a later batch: %d batches", len(batches[0]))
	}
	if want := []int{0, 100, 200}; !reflect.DeepEqual(offsets[1], want) {
		t.Errorf("retry offsets = %v, want %v", offsets[1], want)
	}
	if len(batches[1]) < 3 {
		t.Errorf("retry used %d batches, want at least 3", len(batches[1]))
	}
	batchIDs := make(map[string]int)
	for _, batch := range batches[1] {
		for _, id := range batch {
			batchIDs[id]++
		}
	}
	for id := 1; id <= total; id++ {
		if batchIDs[strconv.Itoa(id)] != 1 {
			t.Errorf("retry batch ID %d occurred %d times", id, batchIDs[strconv.Itoa(id)])
		}
	}
	if len(batchIDs) != total {
		t.Errorf("retry had %d distinct batch IDs, want %d", len(batchIDs), total)
	}
	if len(offsets[1]) != pagesAfterRetry || len(batches[1]) != batchesAfterRetry {
		t.Error("cached response triggered additional upstream requests")
	}
}

func TestFeaturedConfig(t *testing.T) {
	t.Setenv("ETSY_ACCESS_TOKEN", "")
	for _, tt := range []struct{ mode, shop, key, secret string }{
		{"etsy", "", "key", "secret"},
		{"etsy", "0", "key", "secret"},
		{"etsy", "../123", "key", "secret"},
		{"etsy", "123", "", "secret"},
		{"etsy", "123", "key", ""},
		{"etsy", "123", "key\r\nsecret", "secret"},
		{"live", "123", "key", "secret"},
	} {
		t.Run(fmt.Sprintf("%q/%q/%q/%q", tt.mode, tt.shop, tt.key, tt.secret), func(t *testing.T) {
			t.Setenv("ETSY_LISTINGS_MODE", tt.mode)
			t.Setenv("ETSY_SHOP_ID", tt.shop)
			t.Setenv("ETSY_KEYSTRING", tt.key)
			t.Setenv("ETSY_SHARED_SECRET", tt.secret)
			w := requestFeatured(newHandler())
			if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), `"source"`) || strings.Contains(w.Body.String(), "secret") {
				t.Errorf("status = %d, body = %q", w.Code, w.Body.String())
			}
		})
	}
	t.Setenv("ETSY_LISTINGS_MODE", "etsy")
	t.Setenv("ETSY_SHOP_ID", "123")
	t.Setenv("ETSY_KEYSTRING", "key")
	t.Setenv("ETSY_SHARED_SECRET", "secret")
	h := newFeaturedHandlerFromEnv().(*featuredHandler)
	if h.configErr != nil || h.etsy.shopID != 123 || h.etsy.apiKey != "key:secret" || h.etsy.baseURL != etsyAPIBase || h.etsy.client.Timeout != etsyTimeout {
		t.Error("valid environment did not configure the Etsy client correctly")
	}
}

func TestFeaturedTimeoutAndRedirect(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(100 * time.Millisecond)
			fmt.Fprint(w, `{"count":0,"results":[]}`)
		}))
		defer server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		req := httptest.NewRequest(http.MethodGet, "/api/featured-listings", nil).WithContext(ctx)
		w := httptest.NewRecorder()
		liveHandler(server).ServeHTTP(w, req)
		if w.Code != http.StatusGatewayTimeout {
			t.Errorf("status = %d, body = %q", w.Code, w.Body.String())
		}
	})
	t.Run("client timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(100 * time.Millisecond)
			fmt.Fprint(w, `{"count":0,"results":[]}`)
		}))
		defer server.Close()
		client := newEtsyHTTPClient()
		client.Timeout = 20 * time.Millisecond
		handler := newHandlerWithFeatured(&featuredHandler{mode: "etsy", etsy: &etsyClient{
			baseURL: server.URL + "/v3/application", shopID: 123, apiKey: "key:secret", client: client,
		}})
		w := requestFeatured(handler)
		if w.Code != http.StatusGatewayTimeout {
			t.Errorf("status = %d, body = %q", w.Code, w.Body.String())
		}
	})
	t.Run("redirect", func(t *testing.T) {
		hit := false
		destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
		defer destination.Close()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, destination.URL, http.StatusFound)
		}))
		defer server.Close()
		h := &featuredHandler{mode: "etsy", etsy: &etsyClient{
			baseURL: server.URL + "/v3/application", shopID: 123, apiKey: "key:secret", accessToken: "fake-token", client: newEtsyHTTPClient(),
		}}
		w := requestFeatured(newHandlerWithFeatured(h))
		if w.Code != http.StatusBadGateway || hit || strings.Contains(w.Body.String(), "fake-token") {
			t.Errorf("status = %d, redirect followed = %v", w.Code, hit)
		}
	})
}

func TestFeaturedCacheReuseExpiryAndFailure(t *testing.T) {
	var calls, batches int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case activeListingsPath:
			checkActiveRequest(t, r, 0, "")
			if atomic.AddInt32(&calls, 1) == 2 {
				http.Error(w, "upstream error", http.StatusServiceUnavailable)
				return
			}
			fmt.Fprint(w, `{"count":1,"results":[{"listing_id":1,"title":"Item","description":"Cached description","url":"https://www.etsy.com/listing/1"}]}`)
		case batchListingsPath:
			ids := checkBatchRequest(t, r, "")
			atomic.AddInt32(&batches, 1)
			fmt.Fprint(w, imageBatch(ids))
		default:
			t.Errorf("unexpected Etsy endpoint %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	feature := &featuredHandler{mode: "etsy", etsy: &etsyClient{
		baseURL: server.URL + "/v3/application", shopID: 123, apiKey: "key:secret", client: newEtsyHTTPClient(),
	}}
	handler := newHandlerWithFeatured(feature)
	first := decodeFeatured(t, requestFeatured(handler))
	second := decodeFeatured(t, requestFeatured(handler))
	if first.Source != "etsy" || len(first.Listings) != 1 || first.Listings[0].ID != "1" || first.Listings[0].Description != "Cached description" || first.Listings[0].ImageURL != "https://i.etsystatic.com/1.jpg" || !reflect.DeepEqual(first, second) || atomic.LoadInt32(&calls) != 1 || atomic.LoadInt32(&batches) != 1 {
		t.Errorf("cache miss or incorrect cached response: first=%+v second=%+v", first, second)
	}
	feature.mu.Lock()
	remaining := time.Until(feature.cacheUntil)
	feature.cacheUntil = time.Now().Add(-time.Second)
	feature.mu.Unlock()
	if remaining <= 4*time.Minute || remaining > etsyCacheTTL {
		t.Errorf("unexpected cache lifetime: %s", remaining)
	}
	failure := requestFeatured(handler)
	if failure.Code != http.StatusBadGateway || strings.Contains(failure.Body.String(), `"source"`) {
		t.Errorf("expired cache masked failure: status=%d body=%s", failure.Code, failure.Body.String())
	}
	retry := decodeFeatured(t, requestFeatured(handler))
	if !reflect.DeepEqual(first, retry) || atomic.LoadInt32(&calls) != 3 || atomic.LoadInt32(&batches) != 2 {
		t.Errorf("failure was cached or refresh failed: pages=%d batches=%d response=%+v", atomic.LoadInt32(&calls), atomic.LoadInt32(&batches), retry)
	}
}

func TestFeaturedCacheEmptyResults(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		checkActiveRequest(t, r, 0, "")
		fmt.Fprint(w, `{"count":0,"results":[]}`)
	}))
	defer server.Close()
	handler := liveHandler(server)
	for i := 0; i < 2; i++ {
		response := decodeFeatured(t, requestFeatured(handler))
		if response.Source != "etsy" || response.Listings == nil || len(response.Listings) != 0 {
			t.Errorf("unexpected empty result: %+v", response)
		}
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("empty success fetched %d times", calls)
	}
}

func waitForFeaturedWaiters(t *testing.T, h *featuredHandler, count int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		ready := h.flight != nil && h.flight.waiters == count
		h.mu.Unlock()
		if ready {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d refresh waiters", count)
}

func TestFeaturedCacheConcurrentCancellation(t *testing.T) {
	var calls int32
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			close(started)
		}
		select {
		case <-release:
			fmt.Fprint(w, `{"count":0,"results":[]}`)
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	h := &featuredHandler{mode: "etsy", etsy: &etsyClient{
		baseURL: server.URL + "/v3/application", shopID: 123, apiKey: "key:secret", client: newEtsyHTTPClient(),
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan error, 1)
	go func() { _, err := h.cachedFeatured(ctx); first <- err }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request did not start")
	}
	second := make(chan error, 1)
	go func() { _, err := h.cachedFeatured(context.Background()); second <- err }()
	waitForFeaturedWaiters(t, h, 2)
	cancel()
	select {
	case err := <-first:
		if err != context.Canceled {
			t.Errorf("canceled waiter error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled waiter did not return")
	}
	waitForFeaturedWaiters(t, h, 1)
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-second:
		if err != nil {
			t.Fatalf("remaining waiter failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("remaining waiter did not complete")
	}
	if _, err := h.cachedFeatured(context.Background()); err != nil || atomic.LoadInt32(&calls) != 1 {
		t.Errorf("expected one shared and cached request, calls=%d err=%v", atomic.LoadInt32(&calls), err)
	}
}

func TestFeaturedCacheCancelsAbandonedRefresh(t *testing.T) {
	var calls int32
	started := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			close(started)
			<-r.Context().Done()
			close(upstreamCanceled)
			return
		}
		fmt.Fprint(w, `{"count":0,"results":[]}`)
	}))
	defer server.Close()
	h := &featuredHandler{mode: "etsy", etsy: &etsyClient{
		baseURL: server.URL + "/v3/application", shopID: 123, apiKey: "key:secret", client: newEtsyHTTPClient(),
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { _, err := h.cachedFeatured(ctx); finished <- err }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request did not start")
	}
	cancel()
	select {
	case err := <-finished:
		if err != context.Canceled {
			t.Errorf("error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled waiter did not return")
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("abandoned upstream request was not canceled")
	}
	if _, err := h.cachedFeatured(context.Background()); err != nil || atomic.LoadInt32(&calls) != 2 {
		t.Errorf("expected retry after cancellation, calls=%d err=%v", atomic.LoadInt32(&calls), err)
	}
}

func TestEtsyMoneyAndImages(t *testing.T) {
	amount := int64(12345)
	for _, tt := range []struct {
		money *etsyMoney
		want  string
		fail  bool
	}{
		{nil, "", false},
		{&etsyMoney{&amount, 100, "EUR"}, "EUR 123.45", false},
		{&etsyMoney{&amount, 1000, "KWD"}, "KWD 12.345", false},
		{&etsyMoney{&amount, 1, "JPY"}, "JPY 12345", false},
		{&etsyMoney{&amount, 4, "USD"}, "USD 3086.25", false},
		{&etsyMoney{&amount, 3, "USD"}, "", true},
		{&etsyMoney{nil, 100, "USD"}, "", true},
		{&etsyMoney{&amount, 100, "usd"}, "", true},
	} {
		got, err := formatEtsyMoney(tt.money)
		if got != tt.want || (err != nil) != tt.fail {
			t.Errorf("formatEtsyMoney(%+v) = %q, %v", tt.money, got, err)
		}
	}
	images := []etsyImage{
		{Rank: 0, URL570: "https://example.com/unranked"},
		{Rank: 2, URL570: "https://example.com/second"},
		{Rank: 1, URL570: "javascript:bad", URLFull: "https://example.com/first"},
	}
	if got := firstImage(images); got != "https://example.com/first" {
		t.Errorf("firstImage = %q", got)
	}
}
