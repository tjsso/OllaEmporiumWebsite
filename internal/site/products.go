package site

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxPhotoBytes  = 5 << 20
	maxImageSide   = 4096
	maxImagePixels = 16_000_000
)

var errInvalidProduct = errors.New("invalid product")

type product struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Price       string `json:"price"`
	ImageURL    string `json:"image_url"`
}

type productRecord struct {
	Product product `json:"product"`
	Format  string  `json:"format"`
}

type productStore struct {
	dir     string
	mu      sync.RWMutex
	records []productRecord
	loadErr error
}

func newProductStoreFromEnv() *productStore {
	dir := os.Getenv("PRODUCT_DATA_DIR")
	if dir == "" {
		dir = "./data"
	}
	return newProductStore(dir)
}

func newProductStore(dir string) *productStore {
	s := &productStore{dir: dir, records: make([]productRecord, 0)}
	entries, err := os.ReadDir(filepath.Join(dir, "products"))
	if errors.Is(err, os.ErrNotExist) {
		return s
	}
	if err != nil {
		s.loadErr = err
		return s
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !validProductID(id) || !entry.Type().IsRegular() {
			s.loadErr = errors.New("invalid product record")
			return s
		}
		path := filepath.Join(dir, "products", entry.Name())
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			s.loadErr = errors.New("invalid product record")
			return s
		}
		f, err := os.Open(path)
		if err != nil {
			s.loadErr = err
			return s
		}
		openedInfo, statErr := f.Stat()
		if statErr != nil || !os.SameFile(info, openedInfo) {
			_ = f.Close()
			s.loadErr = errors.New("invalid product record")
			return s
		}
		data, readErr := io.ReadAll(io.LimitReader(f, (64<<10)+1))
		closeErr := f.Close()
		var record productRecord
		if readErr != nil || closeErr != nil || len(data) > 64<<10 || json.Unmarshal(data, &record) != nil ||
			record.Product.ID != id || record.Product.ImageURL != "/product-images/"+id ||
			(record.Format != "jpeg" && record.Format != "png") || !validProductFields(record.Product) {
			s.loadErr = errors.New("invalid product record")
			return s
		}
		s.records = append(s.records, record)
	}
	return s
}

func validProductID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func randomToken(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func validText(value string, limit int, multiline bool) bool {
	if value == "" || len(value) > limit || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) && !(multiline && (r == '\n' || r == '\r' || r == '\t')) {
			return false
		}
	}
	return true
}

func validProductFields(p product) bool {
	return validText(p.Title, 200, false) && validText(p.Description, 5000, true) && validText(p.Price, 100, false)
}

// Decode and re-encode supported images, removing any appended or embedded payload.
func sanitizePhoto(data []byte) ([]byte, string, error) {
	if len(data) == 0 || len(data) > maxPhotoBytes {
		return nil, "", errors.New("invalid photo size")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "jpeg" && format != "png") || config.Width <= 0 || config.Height <= 0 ||
		config.Width > maxImageSide || config.Height > maxImageSide || config.Width*config.Height > maxImagePixels {
		return nil, "", errors.New("invalid photo")
	}
	img, decodedFormat, err := image.Decode(bytes.NewReader(data))
	if err != nil || decodedFormat != format {
		return nil, "", errors.New("invalid photo")
	}
	var out bytes.Buffer
	if format == "jpeg" {
		err = jpeg.Encode(&out, img, &jpeg.Options{Quality: 90})
	} else {
		err = png.Encode(&out, img)
	}
	if err != nil {
		return nil, "", err
	}
	return out.Bytes(), format, nil
}

func (s *productStore) create(p product, photo []byte) (product, error) {
	if !validProductFields(p) {
		return product{}, errInvalidProduct
	}
	imageData, format, err := sanitizePhoto(photo)
	if err != nil {
		return product{}, errInvalidProduct
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return product{}, s.loadErr
	}
	id, err := randomToken(16)
	if err != nil {
		return product{}, err
	}
	p.ID, p.ImageURL = id, "/product-images/"+id
	productsDir, imagesDir := filepath.Join(s.dir, "products"), filepath.Join(s.dir, "images")
	for _, dir := range []string{productsDir, imagesDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return product{}, err
		}
	}
	imagePath := filepath.Join(imagesDir, id+"."+format)
	if err := writeNewFile(imagePath, imageData); err != nil {
		return product{}, err
	}
	data, err := json.Marshal(productRecord{Product: p, Format: format})
	if err == nil {
		err = writeAtomicRecord(productsDir, id+".json", data)
	}
	if err != nil {
		_ = os.Remove(imagePath)
		return product{}, err
	}
	s.records = append(s.records, productRecord{Product: p, Format: format})
	return p, nil
}

func writeNewFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(path)
	}
	return err
}

func writeAtomicRecord(dir, name string, data []byte) error {
	f, err := os.CreateTemp(dir, ".product-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}

func (s *productStore) serveProducts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeAPIError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	s.mu.RLock()
	if s.loadErr != nil {
		s.mu.RUnlock()
		writeAPIError(w, http.StatusServiceUnavailable, "Products are unavailable")
		return
	}
	products := make([]product, 0, len(s.records))
	for _, record := range s.records {
		products = append(products, record.Product)
	}
	s.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(struct {
		Products []product `json:"products"`
	}{products})
}

func (s *productStore) serveImage(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/product-images/")
	if !validProductID(id) {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.RLock()
	var format string
	if s.loadErr == nil {
		for _, record := range s.records {
			if record.Product.ID == id {
				format = record.Format
				break
			}
		}
	}
	s.mu.RUnlock()
	if format == "" {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(s.dir, "images", id+"."+format)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	openedInfo, err := f.Stat()
	if err != nil || !os.SameFile(info, openedInfo) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/"+format)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, id, time.Time{}, f)
}

// readPhoto is separate so multipart file names and declared content types are ignored.
func readPhoto(file io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(file, maxPhotoBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxPhotoBytes {
		return nil, errors.New("invalid photo size")
	}
	return data, nil
}
