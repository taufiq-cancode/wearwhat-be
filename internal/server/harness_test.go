package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"wearwhat-be/internal/app"
	"wearwhat-be/internal/config"
	"wearwhat-be/internal/db"
	"wearwhat-be/internal/notify"
	"wearwhat-be/internal/payments"
	"wearwhat-be/internal/secure"
	"wearwhat-be/internal/server"
)

const defaultTestDB = "postgres://wearwhat:wearwhat@localhost:5433/wearwhat?sslmode=disable"

var schemaCounter atomic.Int64

// clock lets tests move time forward for expiry and scheduling.
type clock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

// env is a complete running API against an isolated Postgres schema.
type env struct {
	t       *testing.T
	pool    *pgxpool.Pool
	deps    *app.Deps
	console *notify.Console
	clock   *clock
	srv     *httptest.Server
	server  *server.Server
	seq     atomic.Int64
}

type options struct {
	env      string
	pay      payments.Provider
	apple    string
	appleURL string
	// configure changes settings before the server starts
	configure func(*config.Config)
}

func newEnv(t *testing.T, opt ...func(*options)) *env {
	t.Helper()
	o := options{env: "development", pay: payments.Testing{}}
	for _, f := range opt {
		f(&o)
	}
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		url = defaultTestDB
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect (is `docker compose up -d` running?): %v", err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("t_%d_%d", time.Now().UnixNano(), schemaCounter.Add(1))
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create schema (is `docker compose up -d` running?): %v", err)
	}
	pool, err := db.Open(ctx, url, schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		cleanup, err := pgxpool.New(context.Background(), url)
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
	})
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	console := &notify.Console{Log: log}
	clk := &clock{}
	cfg := &config.Config{
		Env:             o.env,
		Origins:         []string{"http://app.test"},
		Location:        time.UTC,
		UploadDir:       t.TempDir(),
		DataKey:         []byte("0123456789abcdef0123456789abcdef"),
		PaymentCurrency: "USD",
		FXRate:          1,
		AppleClientID:   o.apple,
		AppleKeysURL:    o.appleURL,
	}
	if o.configure != nil {
		o.configure(cfg)
	}
	d := &app.Deps{DB: pool, Cfg: cfg, Log: log, Mailer: console, SMS: notify.NoSMS{}, Payments: o.pay, Now: clk.now}
	srv := server.New(d)
	ts := httptest.NewServer(srv.Handler)
	t.Cleanup(ts.Close)
	return &env{t: t, pool: pool, deps: d, console: console, clock: clk, srv: ts, server: srv}
}

/* ---------- HTTP client ---------- */

type client struct {
	e    *env
	http *http.Client
	// filled in by signUp helpers
	id, handle, email, password, brandID, brandHandle string
}

func (e *env) anon() *client {
	jar, err := cookiejar.New(nil)
	if err != nil {
		e.t.Fatal(err)
	}
	return &client{e: e, http: &http.Client{Jar: jar, Timeout: 30 * time.Second}}
}

const api = "/api/v1"

// raw sends a request and returns the status and body.
func (c *client) raw(method, path string, body any) (int, []byte) {
	c.e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.e.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.e.srv.URL+api+path, rd)
	if err != nil {
		c.e.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		c.e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		c.e.t.Fatal(err)
	}
	return res.StatusCode, raw
}

// ok sends a request, fails the test on a non-2xx status, and decodes the response.
func (c *client) ok(method, path string, body any, out any) {
	c.e.t.Helper()
	status, raw := c.raw(method, path, body)
	if status < 200 || status >= 300 {
		c.e.t.Fatalf("%s %s: status %d: %s", method, path, status, raw)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			c.e.t.Fatalf("%s %s: decode: %v\n%s", method, path, err, raw)
		}
	}
}

// status asserts a specific status and returns the error message, if any.
func (c *client) status(want int, method, path string, body any) string {
	c.e.t.Helper()
	got, raw := c.raw(method, path, body)
	if got != want {
		c.e.t.Fatalf("%s %s: status %d, want %d: %s", method, path, got, want, raw)
	}
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	return e.Error
}

/* ---------- account helpers ---------- */

type m = map[string]any

func (e *env) next() int64 { return e.seq.Add(1) }

// shopper registers a real account through the real sign-up flow.
func (e *env) shopper() *client {
	e.t.Helper()
	c := e.anon()
	n := e.next()
	c.email = fmt.Sprintf("user%d@test.dev", n)
	c.password = "correct-horse-battery"
	var signup struct {
		DevCode string `json:"devCode"`
	}
	c.ok("POST", "/auth/signup", m{"method": "Email", "kind": "shopper", "email": c.email, "password": c.password}, &signup)
	c.ok("POST", "/auth/verify", m{"code": signup.DevCode}, nil)
	c.handle = fmt.Sprintf("user.%d", n)
	var me struct {
		ID string `json:"id"`
	}
	c.ok("PATCH", "/me", m{"name": fmt.Sprintf("User %d", n), "handle": c.handle}, &me)
	c.id = me.ID
	return c
}

// withAddress gives an account a default delivery address.
func (c *client) withAddress() *client {
	c.e.t.Helper()
	c.ok("POST", "/me/addresses", m{"name": "Test Person", "line1": "1 Test Street", "city": "Lagos", "phone": "+2348030000000"}, nil)
	return c
}

// brandOwner signs up and onboards a brand through the real flow (auto-verified in development).
func (e *env) brandOwner() *client {
	e.t.Helper()
	c := e.shopper()
	c.onboard()
	return c
}

func (c *client) onboard() {
	c.e.t.Helper()
	n := c.e.next()
	doc := c.upload("document", pngBytes(c.e.t))
	c.brandHandle = fmt.Sprintf("brand.%d", n)
	body := m{
		"store":    m{"name": fmt.Sprintf("Brand %d", n), "handle": c.brandHandle, "categories": []string{"Streetwear"}, "location": "Lagos", "bio": "A brand"},
		"business": m{"type": "Registered business", "legalName": "Brand Ltd", "registration": "RC 123", "documentId": doc.ID},
		"payouts":  m{"provider": "Paystack", "bank": "GTBank", "account": "0123456789", "currency": "NGN"},
		"shipping": m{"shipsFrom": "Lagos", "zones": m{"lagos": true}, "processing": "48h", "returns": "14 days"},
	}
	c.ok("POST", "/studio/onboarding", body, nil)
	var me struct {
		Brand struct {
			ID string `json:"id"`
		} `json:"brand"`
	}
	c.ok("GET", "/me", nil, &me)
	c.brandID = me.Brand.ID
}

type product struct {
	ID string `json:"id"`
}

// addProduct creates a live product with the given stock per size.
func (c *client) addProduct(price int, stock map[string]int) string {
	c.e.t.Helper()
	sizes := make([]string, 0, len(stock))
	for z := range stock {
		sizes = append(sizes, z)
	}
	var p product
	c.ok("POST", "/studio/products", m{
		"name": fmt.Sprintf("Piece %d", c.e.next()), "price": price, "description": "desc", "category": "Tops",
		"colors": []string{"Black"}, "sizes": sizes, "sizeStock": stock, "images": []string{}, "live": true,
	}, &p)
	return p.ID
}

/* ---------- uploads ---------- */

func pngBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 10))
	img.Set(1, 1, color.RGBA{255, 0, 0, 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

type uploaded struct {
	ID     string  `json:"id"`
	URL    string  `json:"url"`
	Kind   string  `json:"kind"`
	Aspect float64 `json:"aspect"`
}

func (c *client) uploadRaw(purpose string, content []byte) (int, []byte) {
	c.e.t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("file", "file.bin")
	if err != nil {
		c.e.t.Fatal(err)
	}
	if _, err := part.Write(content); err != nil {
		c.e.t.Fatal(err)
	}
	w.Close()
	req, err := http.NewRequest("POST", c.e.srv.URL+api+"/uploads?purpose="+purpose, &body)
	if err != nil {
		c.e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	res, err := c.http.Do(req)
	if err != nil {
		c.e.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, raw
}

func (c *client) upload(purpose string, content []byte) uploaded {
	c.e.t.Helper()
	status, raw := c.uploadRaw(purpose, content)
	if status != 200 {
		c.e.t.Fatalf("upload: %d %s", status, raw)
	}
	var u uploaded
	if err := json.Unmarshal(raw, &u); err != nil {
		c.e.t.Fatal(err)
	}
	return u
}

/* ---------- commerce helpers ---------- */

type checkoutResult struct {
	Number      string   `json:"number"`
	Total       int      `json:"total"`
	Status      string   `json:"status"`
	OrderIDs    []string `json:"orderIds"`
	RedirectURL string   `json:"redirectUrl"`
}

// buy puts a piece in the bag and checks out with the test provider.
func (c *client) buy(productID, size string) checkoutResult {
	c.e.t.Helper()
	c.ok("POST", "/cart/items", m{"productId": productID, "size": size, "color": "Black"}, nil)
	var out checkoutResult
	c.ok("POST", "/checkout", m{"shipping": "standard", "payment": "card"}, &out)
	return out
}

func studioID(shopperID string) string { return strings.TrimPrefix(shopperID, "WW-") }

// deliver walks an order through shipping to delivered, as the brand.
func (c *client) deliver(orderID string) {
	c.e.t.Helper()
	id := studioID(orderID)
	c.ok("PATCH", "/studio/orders/"+id, m{"status": "SHIPPED", "carrier": "GIG", "trackingCode": "GIG-1"}, nil)
	c.ok("PATCH", "/studio/orders/"+id, m{"status": "DELIVERED"}, nil)
}

/* ---------- mail ---------- */

// mailTo waits for an email (they send in the background) and returns its body.
func (e *env) mailTo(to, subjectContains string) string {
	e.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, msg := range e.console.Outbox(to) {
			if strings.Contains(msg.Subject, subjectContains) {
				return msg.Body
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("no email to %s with subject containing %q", to, subjectContains)
	return ""
}

var tokenRe = regexp.MustCompile(`[A-Za-z0-9_-]{40,}`)

func extractToken(t *testing.T, body string) string {
	t.Helper()
	tok := tokenRe.FindString(body)
	if tok == "" {
		t.Fatalf("no token in %q", body)
	}
	return tok
}

// countRows is a direct database assertion helper.
func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// helpers kept with the harness so individual tests stay readable
var _ = secure.Hash
