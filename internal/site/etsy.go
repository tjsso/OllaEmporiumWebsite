package site

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	etsyAPIBase        = "https://api.etsy.com/v3/application"
	etsyTimeout        = 8 * time.Second
	etsyRefreshTimeout = 45 * time.Second
	etsyCacheTTL       = 5 * time.Minute
	etsyMaxBody        = 2 << 20
	shopURL            = "https://www.etsy.com/shop/OllaEmporiumFR"
)

type featuredListing struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	URL         string `json:"url"`
	ImageURL    string `json:"image_url"`
	Price       string `json:"price,omitempty"`
}

type featuredResponse struct {
	Listings []featuredListing `json:"listings"`
	Source   string            `json:"source"`
}

type etsyClient struct {
	baseURL     string
	shopID      int64
	apiKey      string
	accessToken string
	client      *http.Client
}

type featuredHandler struct {
	mode      string
	etsy      *etsyClient
	configErr error

	mu         sync.Mutex
	cached     []featuredListing
	cacheUntil time.Time
	flight     *featuredFlight
}

type featuredFlight struct {
	done     chan struct{}
	cancel   context.CancelFunc
	waiters  int
	listings []featuredListing
	err      error
}

// Keep transport errors available for timeout detection without logging URLs or
// other details that may contain credentials or listing data.
type etsyRequestError struct {
	message string
	cause   error
}

func (e *etsyRequestError) Error() string { return e.message }
func (e *etsyRequestError) Unwrap() error { return e.cause }

// Etsy's public active-listings and batch endpoints need a keystring:shared_secret;
// ETSY_ACCESS_TOKEN is optional. Read configuration once at handler creation.
func newFeaturedHandlerFromEnv() http.Handler {
	mode := strings.TrimSpace(os.Getenv("ETSY_LISTINGS_MODE"))
	if mode == "" {
		mode = "mock"
	}
	h := &featuredHandler{mode: mode}
	switch mode {
	case "mock":
	case "etsy":
		shopID, err := strconv.ParseInt(os.Getenv("ETSY_SHOP_ID"), 10, 64)
		key := strings.TrimSpace(os.Getenv("ETSY_KEYSTRING"))
		secret := strings.TrimSpace(os.Getenv("ETSY_SHARED_SECRET"))
		token := os.Getenv("ETSY_ACCESS_TOKEN")
		if err != nil || shopID <= 0 || key == "" || secret == "" || strings.ContainsAny(key+secret, ":\r\n") || !validBearerToken(token) {
			h.configErr = errors.New("invalid Etsy configuration")
			break
		}
		h.etsy = &etsyClient{
			baseURL:     etsyAPIBase,
			shopID:      shopID,
			apiKey:      key + ":" + secret,
			accessToken: token,
			client:      newEtsyHTTPClient(),
		}
	default:
		h.configErr = errors.New("invalid listings mode")
	}
	return h
}

// RFC 6750 bearer credentials contain only b64token characters, with optional
// trailing padding. Reject whitespace and control characters rather than trim.
func validBearerToken(token string) bool {
	if token == "" {
		return true
	}
	padding := false
	for i := 0; i < len(token); i++ {
		c := token[i]
		if c == '=' {
			if i == 0 {
				return false
			}
			padding = true
			continue
		}
		if padding || !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.ContainsRune("-._~+/", rune(c))) {
			return false
		}
	}
	return true
}

func newEtsyHTTPClient() *http.Client {
	return &http.Client{
		Timeout: etsyTimeout,
		// x-api-key is a custom header, so never forward it on redirects.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (h *featuredHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeAPIError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	if h.configErr != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "Etsy listings are not configured")
		return
	}

	response := featuredResponse{Source: h.mode}
	if h.mode == "mock" {
		response.Listings = []featuredListing{
			{ID: "demo-mug", Title: "Demo item: mug", Description: "Demo item: an example mug, not an actual listing.", URL: shopURL, ImageURL: "/ollamug.webp"},
			{ID: "demo-plate", Title: "Demo item: plate", Description: "Demo item: an example plate, not an actual listing.", URL: shopURL, ImageURL: "/ollaplate.webp"},
			{ID: "demo-banner", Title: "Demo item: shop banner", Description: "Demo item: an example shop banner, not an actual listing.", URL: shopURL, ImageURL: "/ollaemporiumbanner.webp"},
		}
	} else {
		listings, err := h.cachedFeatured(r.Context())
		if err != nil {
			var netErr net.Error
			if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
				writeAPIError(w, http.StatusGatewayTimeout, "Etsy request timed out")
			} else {
				writeAPIError(w, http.StatusBadGateway, "Etsy listings are unavailable")
			}
			return
		}
		response.Listings = listings
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(response)
}

// A refresh belongs to its waiters rather than any one request: an individual
// cancellation does not interrupt other waiters, but the last one cancels it.
func (h *featuredHandler) cachedFeatured(ctx context.Context) ([]featuredListing, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h.mu.Lock()
	if time.Now().Before(h.cacheUntil) {
		listings := h.cached
		h.mu.Unlock()
		return listings, nil
	}
	flight := h.flight
	if flight == nil {
		fetchCtx, cancel := context.WithTimeout(context.Background(), etsyRefreshTimeout)
		flight = &featuredFlight{done: make(chan struct{}), cancel: cancel}
		h.flight = flight
		go h.refreshFeatured(fetchCtx, flight)
	}
	flight.waiters++
	h.mu.Unlock()

	select {
	case <-ctx.Done():
		h.mu.Lock()
		flight.waiters--
		if flight.waiters == 0 && h.flight == flight {
			h.flight = nil
			flight.cancel()
		}
		h.mu.Unlock()
		return nil, ctx.Err()
	case <-flight.done:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return flight.listings, flight.err
	}
}

func (h *featuredHandler) refreshFeatured(ctx context.Context, flight *featuredFlight) {
	defer flight.cancel()
	listings, err := h.etsy.activeListings(ctx)
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("Etsy listings refresh failed: %s", err)
	}
	h.mu.Lock()
	flight.listings, flight.err = listings, err
	if h.flight == flight {
		if err == nil && ctx.Err() == nil {
			h.cached = listings
			h.cacheUntil = time.Now().Add(etsyCacheTTL)
		}
		h.flight = nil
	}
	close(flight.done)
	h.mu.Unlock()
}

func writeAPIError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{message})
}

type etsyMoney struct {
	Amount   *int64 `json:"amount"`
	Divisor  int64  `json:"divisor"`
	Currency string `json:"currency_code"`
}

type etsyListing struct {
	ID          int64      `json:"listing_id"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	URL         string     `json:"url"`
	Price       *etsyMoney `json:"price"`
}

type etsyImage struct {
	Rank    int    `json:"rank"`
	URL570  string `json:"url_570xN"`
	URLFull string `json:"url_fullxfull"`
	URL170  string `json:"url_170x135"`
	URL75   string `json:"url_75x75"`
}

func (c *etsyClient) activeListings(ctx context.Context) ([]featuredListing, error) {
	const pageSize = 100
	const maxOffset = 12000 // Etsy's documented pagination limit.

	listings := make([]featuredListing, 0)
	seen := make(map[int64]bool)
	for offset := 0; ; {
		if offset > maxOffset {
			return nil, errors.New("Etsy listings exceed the pagination offset limit")
		}
		var page struct {
			Count   *int          `json:"count"`
			Results []etsyListing `json:"results"`
		}
		path := fmt.Sprintf("/shops/%d/listings/active?limit=%d&offset=%d", c.shopID, pageSize, offset)
		if err := c.getJSON(ctx, path, &page); err != nil {
			return nil, fmt.Errorf("active listings: %w", err)
		}
		if page.Count == nil || *page.Count < 0 || page.Results == nil || len(page.Results) > pageSize || offset+len(page.Results) > *page.Count {
			return nil, errors.New("invalid active listings response")
		}
		if *page.Count > maxOffset+pageSize {
			return nil, errors.New("Etsy listings exceed the pagination offset limit")
		}
		if len(page.Results) == 0 && offset < *page.Count {
			return nil, errors.New("incomplete active listings response")
		}
		for _, item := range page.Results {
			if item.ID <= 0 || seen[item.ID] || strings.TrimSpace(item.Title) == "" || !httpsURL(item.URL) {
				return nil, errors.New("invalid active listing")
			}
			seen[item.ID] = true
			price, err := formatEtsyMoney(item.Price)
			if err != nil {
				return nil, err
			}
			listings = append(listings, featuredListing{
				ID: strconv.FormatInt(item.ID, 10), Title: item.Title, Description: item.Description, URL: item.URL, Price: price,
			})
		}
		offset += len(page.Results)
		if offset == *page.Count {
			break
		}
	}

	// The active-listings endpoint does not document image associations.
	// Join batch images by ID, preserving the active-listings order and price.
	for start := 0; start < len(listings); start += pageSize {
		end := start + pageSize
		if end > len(listings) {
			end = len(listings)
		}
		ids := make([]string, 0, end-start)
		positions := make(map[string]int, end-start)
		for i := start; i < end; i++ {
			ids = append(ids, listings[i].ID)
			positions[listings[i].ID] = i
		}
		var batch struct {
			Count   *int `json:"count"`
			Results []struct {
				ID     int64       `json:"listing_id"`
				Images []etsyImage `json:"images"`
			} `json:"results"`
		}
		query := url.Values{"listing_ids": {strings.Join(ids, ",")}, "includes": {"Images"}}
		if err := c.getJSON(ctx, "/listings/batch?"+query.Encode(), &batch); err != nil {
			return nil, fmt.Errorf("image batch: %w", err)
		}
		if batch.Count == nil || batch.Results == nil || *batch.Count != len(batch.Results) {
			return nil, errors.New("invalid image response")
		}
		for _, item := range batch.Results {
			id := strconv.FormatInt(item.ID, 10)
			position, ok := positions[id]
			if !ok {
				return nil, errors.New("unexpected or duplicate listing in image response")
			}
			listings[position].ImageURL = firstImage(item.Images)
			delete(positions, id)
		}
		if len(positions) != 0 {
			return nil, errors.New("missing listing in image response")
		}
	}
	return listings, nil
}

func (c *etsyClient) getJSON(ctx context.Context, path string, target interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return &etsyRequestError{message: "invalid request", cause: err}
	}
	req.Header.Set("x-api-key", c.apiKey)
	if c.accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.accessToken)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return &etsyRequestError{message: "network error", cause: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Etsy API returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, etsyMaxBody+1))
	if err != nil {
		return &etsyRequestError{message: "error reading Etsy response", cause: err}
	}
	if len(body) > etsyMaxBody {
		return errors.New("Etsy response exceeds size limit")
	}
	if err := json.Unmarshal(body, target); err != nil {
		return errors.New("invalid JSON response")
	}
	return nil
}

func httpsURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil
}

func firstImage(images []etsyImage) string {
	bestRank := int(^uint(0) >> 1)
	best := ""
	for _, image := range images {
		candidate := ""
		for _, raw := range []string{image.URL570, image.URLFull, image.URL170, image.URL75} {
			if httpsURL(raw) {
				candidate = raw
				break
			}
		}
		if candidate != "" && (best == "" || (image.Rank > 0 && image.Rank < bestRank)) {
			best = candidate
			if image.Rank > 0 {
				bestRank = image.Rank
			}
		}
	}
	return best
}

func formatEtsyMoney(money *etsyMoney) (string, error) {
	if money == nil {
		return "", nil
	}
	if money.Amount == nil || *money.Amount < 0 || money.Divisor <= 0 || len(money.Currency) != 3 {
		return "", errors.New("invalid Etsy price")
	}
	for _, c := range money.Currency {
		if c < 'A' || c > 'Z' {
			return "", errors.New("invalid Etsy currency")
		}
	}
	// Use exact rational arithmetic; never round a non-terminating amount.
	divisor, twos, fives := money.Divisor, 0, 0
	for divisor%2 == 0 {
		divisor /= 2
		twos++
	}
	for divisor%5 == 0 {
		divisor /= 5
		fives++
	}
	if divisor != 1 {
		return "", errors.New("unsupported Etsy price divisor")
	}
	precision := twos
	if fives > precision {
		precision = fives
	}
	amount := new(big.Rat).SetFrac(big.NewInt(*money.Amount), big.NewInt(money.Divisor))
	return money.Currency + " " + amount.FloatString(precision), nil
}
