package media

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/joho/godotenv"

	"wearwhat-be/internal/config"
	"wearwhat-be/internal/secure"
)

// These talk to your real Cloudinary account, using CLOUDINARY_URL from .env, and delete what
// they upload. They only run with LIVE_TESTS=1:
//
//	LIVE_TESTS=1 go test ./internal/media -run Live -v
func liveCloudinary(t *testing.T) *Cloudinary {
	t.Helper()
	if os.Getenv("LIVE_TESTS") != "1" {
		t.Skip("set LIVE_TESTS=1 to use your real Cloudinary account")
	}
	_ = godotenv.Load("../../.env")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.CloudinaryEnabled() {
		t.Fatal("LIVE_TESTS=1 needs CLOUDINARY_URL in .env")
	}
	return NewStore(cfg).(*Cloudinary)
}

// destroy removes a test file from the account.
func destroy(t *testing.T, c *Cloudinary, resource, id, delivery string) {
	t.Helper()
	form := url.Values{}
	for name, value := range c.signed(map[string]string{"public_id": id, "type": delivery, "invalidate": "true"}) {
		form.Set(name, value)
	}
	res, err := http.PostForm(c.endpoint(resource, "destroy"), form)
	if err != nil {
		t.Errorf("clean up %s: couldn't reach Cloudinary", id)
		return
	}
	defer res.Body.Close()
	var out struct {
		Result string `json:"result"`
	}
	_ = json.NewDecoder(res.Body).Decode(&out)
	if out.Result != "ok" {
		t.Errorf("clean up %s: status %d, result %q: delete it by hand in the Media Library", id, res.StatusCode, out.Result)
	}
}

func get(t *testing.T, target string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "https://app.example.com")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res, body
}

func TestLiveCloudinaryImage(t *testing.T) {
	c := liveCloudinary(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var picture bytes.Buffer
	if err := png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 40, 50))); err != nil {
		t.Fatal(err)
	}
	id := secure.ID("u")
	saved, err := c.Save(ctx, NewFile{ID: id, Ext: ".png", Kind: "image"}, &picture)
	if err != nil {
		t.Fatal(err)
	}
	defer destroy(t, c, "image", saved.Path, "upload")
	if !strings.Contains(saved.URL, imageDelivery) {
		t.Errorf("no transformation in %s", saved.URL)
	}
	res, _ := get(t, saved.URL)
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "image/") {
		t.Errorf("fetching %s: status %d, type %s", saved.URL, res.StatusCode, res.Header.Get("Content-Type"))
	}
	if res.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("try-on capture needs Access-Control-Allow-Origin: *, got %q", res.Header.Get("Access-Control-Allow-Origin"))
	}
}

func TestLiveCloudinaryVideo(t *testing.T) {
	c := liveCloudinary(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	sample, err := http.Get("https://res.cloudinary.com/demo/video/upload/dog.mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer sample.Body.Close()
	if sample.StatusCode != 200 {
		t.Fatalf("sample video: status %d", sample.StatusCode)
	}
	id := secure.ID("u")
	saved, err := c.Save(ctx, NewFile{ID: id, Ext: ".mp4", Kind: "video"}, sample.Body)
	if err != nil {
		t.Fatal(err)
	}
	defer destroy(t, c, "video", saved.Path, "upload")
	res, _ := get(t, saved.URL)
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "video/") {
		t.Errorf("fetching %s: status %d, type %s", saved.URL, res.StatusCode, res.Header.Get("Content-Type"))
	}
}

func TestLiveCloudinaryPrivateDocument(t *testing.T) {
	c := liveCloudinary(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sealed := []byte("not a real document: wearwhat live test")
	id := secure.ID("u")
	saved, err := c.Save(ctx, NewFile{ID: id, Ext: ".enc", Kind: "document", Private: true}, bytes.NewReader(sealed))
	if err != nil {
		t.Fatal(err)
	}
	defer destroy(t, c, "raw", saved.Path, "authenticated")
	if saved.URL != "" {
		t.Errorf("a private file got a URL: %s", saved.URL)
	}

	// no way of asking for it without a signature may work
	for _, kind := range []string{"authenticated", "upload", "private"} {
		target := "https://res.cloudinary.com/" + c.Cloud + "/raw/" + kind + "/" + saved.Path
		if res, _ := get(t, target); res.StatusCode == 200 {
			t.Errorf("fetched without a signature: %s", target)
		}
	}

	got, err := c.Fetch(ctx, saved.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, sealed) {
		t.Errorf("read back %q", got)
	}
}
