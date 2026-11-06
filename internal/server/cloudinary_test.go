package server_test

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wearwhat-be/internal/config"
	"wearwhat-be/internal/media"
)

const (
	cldKey    = "cld-key"
	cldSecret = "cld-secret"
	delivery  = "c_limit,w_1600,f_auto,q_auto"
)

type cldFile struct {
	resource, delivery string
	data               []byte
}

// fakeCloudinary follows Cloudinary's documented rules, and rejects anything badly signed.
type fakeCloudinary struct {
	t     *testing.T
	mu    sync.Mutex
	files map[string]cldFile
	fail  atomic.Bool
	srv   *httptest.Server
}

// cldSign is written separately from the real code on purpose, so a mistake in one shows up.
func cldSign(params map[string][]string) string {
	var pairs []string
	for name, values := range params {
		switch name {
		case "file", "cloud_name", "resource_type", "api_key", "signature":
			continue
		}
		pairs = append(pairs, name+"="+values[0])
	}
	slices.Sort(pairs)
	sum := sha1.Sum([]byte(strings.Join(pairs, "&") + cldSecret))
	return hex.EncodeToString(sum[:])
}

func newFakeCloudinary(t *testing.T) *fakeCloudinary {
	f := &fakeCloudinary{t: t, files: map[string]cldFile{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1_1/demo/{resource}/upload", func(w http.ResponseWriter, r *http.Request) {
		if f.fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":{"message":"storage is unavailable"}}`)
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("upload wasn't a readable form: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		form := r.MultipartForm.Value
		signature := ""
		if len(form["signature"]) > 0 {
			signature = form["signature"][0]
		}
		if signature != cldSign(form) || len(form["api_key"]) == 0 || form["api_key"][0] != cldKey {
			t.Errorf("badly signed upload: %v", form)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			t.Errorf("upload has no file: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		defer file.Close()
		var data bytes.Buffer
		if _, err := data.ReadFrom(file); err != nil {
			t.Error(err)
		}
		id := form["public_id"][0]
		kind := "upload"
		if len(form["type"]) > 0 {
			kind = form["type"][0]
		}
		resource := r.PathValue("resource")
		f.mu.Lock()
		f.files[id] = cldFile{resource: resource, delivery: kind, data: data.Bytes()}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"public_id":  id,
			"secure_url": fmt.Sprintf("https://res.cloudinary.test/demo/%s/%s/v1/%s.bin", resource, kind, id),
		})
	})
	mux.HandleFunc("GET /v1_1/demo/raw/download", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		expires, _ := strconv.ParseInt(q.Get("expires_at"), 10, 64)
		if q.Get("signature") != cldSign(q) || q.Get("api_key") != cldKey || q.Get("type") != "authenticated" || expires <= time.Now().Unix() {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		file, ok := f.files[q.Get("public_id")]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(file.data)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCloudinary) file(id string) (cldFile, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	file, ok := f.files[id]
	return file, ok
}

func (f *fakeCloudinary) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.files)
}

func withCloudinary(f *fakeCloudinary) func(*options) {
	return func(o *options) {
		o.configure = func(c *config.Config) {
			c.CloudinaryCloud, c.CloudinaryKey, c.CloudinarySecret, c.CloudinaryBase = "demo", cldKey, cldSecret, f.srv.URL
		}
	}
}

// mp4Bytes starts the way a real MP4 does, which is what the upload check looks at.
func mp4Bytes() []byte {
	return append([]byte{0, 0, 0, 24}, []byte("ftypmp42\x00\x00\x00\x00mp42isom-video-bytes")...)
}

func TestCloudinaryUploads(t *testing.T) {
	fake := newFakeCloudinary(t)
	e := newEnv(t, withCloudinary(fake))
	owner := e.brandOwner()
	alice := e.shopper()
	bob := e.shopper()
	pid := owner.addProduct(5000, map[string]int{"M": 5})

	original := pngBytes(t)
	img := alice.upload("image", original)
	wear := func(c *client, url string) (int, []byte) {
		return c.raw("POST", "/wears", m{"imageUrl": url, "productId": pid, "fit": "True to size", "caption": "x", "hashtags": []string{}})
	}

	t.Run("an image reaches Cloudinary and comes back as a resized URL", func(t *testing.T) {
		stored, ok := fake.file("wearwhat/" + img.ID)
		if !ok {
			t.Fatalf("not stored under its id: %v", img.ID)
		}
		if stored.resource != "image" || stored.delivery != "upload" {
			t.Errorf("stored as %s/%s", stored.resource, stored.delivery)
		}
		if !bytes.Equal(stored.data, original) {
			t.Error("Cloudinary didn't get the original bytes")
		}
		want := "https://res.cloudinary.test/demo/image/upload/" + delivery + "/v1/wearwhat/" + img.ID + ".bin"
		if img.URL != want {
			t.Errorf("url:\n got %s\nwant %s", img.URL, want)
		}
		if img.Aspect != 0.8 {
			t.Errorf("aspect: %v", img.Aspect)
		}
	})

	t.Run("a video keeps its original URL", func(t *testing.T) {
		video := owner.upload("video", mp4Bytes())
		stored, ok := fake.file("wearwhat/" + video.ID)
		if !ok || stored.resource != "video" {
			t.Fatalf("stored as %+v", stored)
		}
		if strings.Contains(video.URL, delivery) || video.Kind != "video" {
			t.Errorf("got %+v", video)
		}
		var added struct {
			Image struct {
				URL  string `json:"url"`
				Kind string `json:"kind"`
			} `json:"image"`
		}
		owner.ok("POST", "/studio/media", m{"url": video.URL}, &added)
		if added.Image.URL != video.URL || added.Image.Kind != "video" {
			t.Errorf("media library has %+v", added)
		}
	})

	t.Run("only the exact URL we stored, from its owner, can be posted", func(t *testing.T) {
		if status, raw := wear(bob, img.URL); status != 400 {
			t.Errorf("someone else's upload: %d %s", status, raw)
		}
		for name, url := range map[string]string{
			"foreign Cloudinary file": "https://res.cloudinary.test/demo/image/upload/v1/wearwhat/u_someoneelse.bin",
			"changed transformation":  strings.Replace(img.URL, delivery, "w_4000", 1),
			"transformation removed":  strings.Replace(img.URL, delivery+"/", "", 1),
			"another site":            "https://evil.example/x.jpg",
			"the local-disk form":     "/uploads/" + img.ID,
		} {
			if status, raw := wear(alice, url); status != 400 {
				t.Errorf("%s: %d %s", name, status, raw)
			}
		}
		if status, raw := wear(alice, img.URL); status != 200 {
			t.Errorf("your own upload: %d %s", status, raw)
		}
	})

	t.Run("a product photo reaches the brand's media library", func(t *testing.T) {
		photo := owner.upload("image", pngBytes(t))
		owner.ok("POST", "/studio/products", m{
			"name": "Shirt", "price": 4000, "description": "desc", "category": "Tops", "colors": []string{"Black"},
			"sizes": []string{"M"}, "sizeStock": m{"M": 2}, "images": []string{photo.URL}, "live": true,
		}, nil)
		_, raw := owner.raw("GET", "/studio/media", nil)
		if !bytes.Contains(raw, []byte(photo.URL)) {
			t.Errorf("library doesn't include %s:\n%s", photo.URL, raw)
		}
	})

	t.Run("nothing is served from our own /uploads route", func(t *testing.T) {
		res, err := http.Get(e.srv.URL + "/uploads/" + img.ID)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 404 {
			t.Errorf("status %d", res.StatusCode)
		}
	})

	t.Run("a file over the limit never reaches Cloudinary", func(t *testing.T) {
		before := fake.count()
		big := append(pngBytes(t), make([]byte, 9<<20)...)
		if status, raw := alice.uploadRaw("image", big); status != 400 {
			t.Errorf("status %d: %s", status, raw)
		}
		if fake.count() != before {
			t.Error("it was sent anyway")
		}
	})

	t.Run("a Cloudinary failure gets a clear message and saves nothing", func(t *testing.T) {
		fake.fail.Store(true)
		defer fake.fail.Store(false)
		var before, after int
		if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM uploads`).Scan(&before); err != nil {
			t.Fatal(err)
		}
		status, raw := alice.uploadRaw("image", pngBytes(t))
		if status != http.StatusBadGateway || !strings.Contains(string(raw), "couldn’t be saved") {
			t.Errorf("status %d: %s", status, raw)
		}
		if strings.Contains(string(raw), "storage is unavailable") {
			t.Errorf("the provider's error reached the browser: %s", raw)
		}
		if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM uploads`).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if after != before {
			t.Errorf("rows went from %d to %d", before, after)
		}
	})
}

func documentPath(t *testing.T, e *env, id string) (path string, url *string) {
	t.Helper()
	if err := e.pool.QueryRow(context.Background(), `SELECT path, url FROM uploads WHERE id = $1`, id).Scan(&path, &url); err != nil {
		t.Fatal(err)
	}
	return path, url
}

func TestCloudinaryDocumentsAreEncrypted(t *testing.T) {
	fake := newFakeCloudinary(t)
	e := newEnv(t, withCloudinary(fake))
	alice := e.shopper()
	original := []byte("%PDF-1.4 national id number 12345678901")

	doc := alice.upload("document", original)
	if doc.URL != "" {
		t.Fatalf("document has a URL: %q", doc.URL)
	}
	path, url := documentPath(t, e, doc.ID)
	if url != nil {
		t.Errorf("a URL was stored: %q", *url)
	}
	if path != "wearwhat/private/"+doc.ID+".enc" {
		t.Errorf("stored at %q", path)
	}
	stored, ok := fake.file(path)
	if !ok {
		t.Fatal("not stored")
	}
	if stored.resource != "raw" || stored.delivery != "authenticated" {
		t.Errorf("stored as %s/%s: anything else can be fetched without a signature", stored.resource, stored.delivery)
	}
	if bytes.Contains(stored.data, []byte("12345678901")) || bytes.Contains(stored.data, []byte("%PDF")) {
		t.Error("Cloudinary received the document unencrypted")
	}

	store := media.NewStore(e.deps.Cfg)
	plain, err := media.ReadDocument(context.Background(), store, e.deps.Cfg.DataKey, path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plain, original) {
		t.Errorf("read back %q", plain)
	}
	wrong := bytes.Repeat([]byte{7}, 32)
	if _, err := media.ReadDocument(context.Background(), store, wrong, path); err == nil {
		t.Error("the wrong key read the document")
	}

	// the application still works with it
	alice.onboard()
}

func TestLocalDiskDocumentsAreEncrypted(t *testing.T) {
	e := newEnv(t)
	alice := e.shopper()
	original := []byte("%PDF-1.4 national id number 12345678901")
	doc := alice.upload("document", original)
	path, _ := documentPath(t, e, doc.ID)
	onDisk, err := os.ReadFile(filepath.Join(e.deps.Cfg.UploadDir, path))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(onDisk, []byte("12345678901")) {
		t.Error("the document is unencrypted on disk")
	}
	plain, err := media.ReadDocument(context.Background(), media.NewStore(e.deps.Cfg), e.deps.Cfg.DataKey, path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plain, original) {
		t.Errorf("read back %q", plain)
	}
}

// Rate limits must count the address the proxy saw, whatever a client adds to the header.
func TestFakeForwardedHeaderCantDodgeRateLimits(t *testing.T) {
	e := newEnv(t, func(o *options) {
		o.env = "production" // development multiplies per-address limits
		o.configure = func(c *config.Config) { c.TrustedProxyHops = 1 }
	})
	forgot := func(forwarded string) int {
		req, err := http.NewRequest("POST", e.srv.URL+api+"/auth/forgot", strings.NewReader(`{"email":"nobody@test.dev"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", forwarded)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	limited := 0
	for i := range 21 { // the limit is 20 an hour
		if forgot(fmt.Sprintf("10.9.8.%d, 203.0.113.7", i)) == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited != 1 {
		t.Errorf("a fake address dodged the limit: %d of 21 requests were limited, want 1", limited)
	}
	if status := forgot("203.0.113.7, 198.51.100.8"); status != 200 {
		t.Errorf("someone else got caught by that limit: status %d", status)
	}
}
