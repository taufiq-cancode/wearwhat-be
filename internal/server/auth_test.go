package server_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"wearwhat-be/internal/config"
)

func TestSignupVerifyLoginLogout(t *testing.T) {
	e := newEnv(t)
	c := e.anon()

	c.status(401, "GET", "/me", nil)
	var signup struct {
		DevCode string `json:"devCode"`
	}
	c.ok("POST", "/auth/signup", m{"method": "Email", "kind": "shopper", "email": "New@Test.Dev", "password": "longenough"}, &signup)
	if len(signup.DevCode) != 6 {
		t.Fatalf("dev mode should return the code, got %q", signup.DevCode)
	}
	// a pending session is not signed in
	c.status(401, "GET", "/me", nil)
	c.status(401, "GET", "/cart", nil)

	c.status(400, "POST", "/auth/verify", m{"code": "12"})
	c.ok("POST", "/auth/verify", m{"code": signup.DevCode}, nil)

	var me struct {
		Email string `json:"email"`
	}
	c.ok("GET", "/me", nil, &me)
	if me.Email != "new@test.dev" {
		t.Errorf("email should be normalised, got %q", me.Email)
	}

	c.ok("POST", "/auth/logout", nil, nil)
	c.status(401, "GET", "/me", nil)
	if msg := c.status(401, "POST", "/auth/login", m{"email": "new@test.dev", "password": "wrong"}); msg == "" {
		t.Error("expected a message")
	}
	c.ok("POST", "/auth/login", m{"email": "NEW@test.dev", "password": "longenough"}, nil)
	c.ok("GET", "/me", nil, nil)
}

func TestLogoutEndsTheSessionOnTheServer(t *testing.T) {
	e := newEnv(t)
	c := e.shopper()
	before := e.count(`SELECT count(*) FROM sessions`)
	c.ok("POST", "/auth/logout", nil, nil)
	if after := e.count(`SELECT count(*) FROM sessions`); after != before-1 {
		t.Errorf("sessions before %d, after %d: logout must delete the row, not just the cookie", before, after)
	}
}

func TestSessionTokenIsStoredOnlyAsAHash(t *testing.T) {
	e := newEnv(t)
	c := e.shopper()
	u, _ := http.NewRequest("GET", e.srv.URL, nil)
	var token string
	for _, ck := range c.http.Jar.Cookies(u.URL) {
		if ck.Name == "ww_session" {
			token = ck.Value
		}
	}
	if token == "" {
		t.Fatal("no session cookie")
	}
	if n := e.count(`SELECT count(*) FROM sessions WHERE encode(token_hash, 'escape') LIKE '%' || $1 || '%'`, token); n != 0 {
		t.Error("the raw token must not be in the database")
	}
}

func TestSignupValidation(t *testing.T) {
	e := newEnv(t)
	c := e.anon()
	c.status(400, "POST", "/auth/signup", m{"method": "Email", "email": "not-an-email", "password": "longenough"})
	c.status(400, "POST", "/auth/signup", m{"method": "Email", "email": "a@b.co", "password": "short"})
	c.status(400, "POST", "/auth/signup", m{"method": "Teleport", "email": "a@b.co", "password": "longenough"})
	// the mock signed anyone in for "Apple"; the real endpoint must not
	c.status(400, "POST", "/auth/signup", m{"method": "Apple", "kind": "shopper"})
	c.status(401, "GET", "/me", nil)
	// no SMS provider configured
	c.status(400, "POST", "/auth/signup", m{"method": "Phone", "phone": "+2348030000000", "password": "longenough"})
}

func TestDuplicateAndAbandonedSignups(t *testing.T) {
	e := newEnv(t)
	existing := e.shopper()
	if msg := e.anon().status(409, "POST", "/auth/signup", m{"method": "Email", "email": existing.email, "password": "longenough"}); !strings.Contains(msg, "already exists") {
		t.Errorf("message: %q", msg)
	}

	// an unfinished sign-up doesn't lock the address: the next person to prove it owns it
	first, second := e.anon(), e.anon()
	first.ok("POST", "/auth/signup", m{"method": "Email", "email": "free@test.dev", "password": "first-password"}, nil)
	var again struct {
		DevCode string `json:"devCode"`
	}
	second.ok("POST", "/auth/signup", m{"method": "Email", "email": "free@test.dev", "password": "second-password"}, &again)
	second.ok("POST", "/auth/verify", m{"code": again.DevCode}, nil)
	// the abandoned attempt's password and pending session are dead
	e.anon().status(401, "POST", "/auth/login", m{"email": "free@test.dev", "password": "first-password"})
	first.status(400, "POST", "/auth/verify", m{"code": again.DevCode})
	e.anon().ok("POST", "/auth/login", m{"email": "free@test.dev", "password": "second-password"}, nil)
}

func TestVerificationCodeAttemptLimit(t *testing.T) {
	e := newEnv(t)
	c := e.anon()
	var signup struct {
		DevCode string `json:"devCode"`
	}
	c.ok("POST", "/auth/signup", m{"method": "Email", "email": "brute@test.dev", "password": "longenough"}, &signup)
	wrong := "000000"
	if signup.DevCode == wrong {
		wrong = "000001"
	}
	for i := 0; i < 5; i++ {
		c.status(400, "POST", "/auth/verify", m{"code": wrong})
	}
	// locked: even the right code is refused now
	c.status(429, "POST", "/auth/verify", m{"code": signup.DevCode})
	c.status(401, "GET", "/me", nil)

	// a new code restores access
	var resent struct {
		DevCode string `json:"devCode"`
	}
	c.ok("POST", "/auth/resend", nil, &resent)
	c.ok("POST", "/auth/verify", m{"code": resent.DevCode}, nil)
	c.ok("GET", "/me", nil, nil)
}

func TestLoginOnUnverifiedAccountResumesVerification(t *testing.T) {
	e := newEnv(t)
	first := e.anon()
	first.ok("POST", "/auth/signup", m{"method": "Email", "kind": "brand", "email": "half@test.dev", "password": "longenough"}, nil)

	c := e.anon()
	status, raw := c.raw("POST", "/auth/login", m{"email": "half@test.dev", "password": "longenough"})
	if status != 403 {
		t.Fatalf("status %d: %s", status, raw)
	}
	var body struct {
		NeedsVerification bool   `json:"needsVerification"`
		Kind              string `json:"kind"`
		DevCode           string `json:"devCode"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if !body.NeedsVerification || body.Kind != "brand" {
		t.Fatalf("body: %s", raw)
	}
	c.status(401, "GET", "/me", nil)
	c.ok("POST", "/auth/verify", m{"code": body.DevCode}, nil)
	c.ok("GET", "/me", nil, nil)

	// the right account but the wrong password learns nothing about verification state
	second := e.anon()
	second.ok("POST", "/auth/signup", m{"method": "Email", "email": "half2@test.dev", "password": "longenough"}, nil)
	e.anon().status(401, "POST", "/auth/login", m{"email": "half2@test.dev", "password": "wrongwrong"})
}

func TestLoginRateLimit(t *testing.T) {
	e := newEnv(t)
	target := e.shopper()
	attacker := e.anon()
	for i := 0; i < 10; i++ {
		attacker.status(401, "POST", "/auth/login", m{"email": target.email, "password": "guess-guess"})
	}
	// past the limit even the right password is refused for this identifier, for now
	attacker.status(429, "POST", "/auth/login", m{"email": target.email, "password": target.password})
}

func TestSuccessfulLoginsNeverLockAnAccount(t *testing.T) {
	e := newEnv(t)
	c := e.shopper()
	for i := 0; i < 15; i++ {
		e.anon().ok("POST", "/auth/login", m{"email": c.email, "password": c.password}, nil)
	}
	// and a success clears earlier mistakes: nine typos, then in, then nine more is still fine
	for round := 0; round < 2; round++ {
		for i := 0; i < 9; i++ {
			e.anon().status(401, "POST", "/auth/login", m{"email": c.email, "password": "wrong-wrong"})
		}
		e.anon().ok("POST", "/auth/login", m{"email": c.email, "password": c.password}, nil)
	}
}

func TestProductionNeverExposesDevCodes(t *testing.T) {
	e := newEnv(t, func(o *options) { o.env = "production" })
	c := e.anon()
	status, raw := c.raw("POST", "/auth/signup", m{"method": "Email", "email": "prod@test.dev", "password": "longenough"})
	if status != 200 {
		t.Fatalf("status %d: %s", status, raw)
	}
	if strings.Contains(string(raw), "devCode") {
		t.Fatalf("production leaked a code: %s", raw)
	}
	// the code really was delivered by email instead
	if body := e.mailTo("prod@test.dev", "code"); !strings.Contains(body, "code is") {
		t.Fatalf("email: %q", body)
	}
	status, raw = c.raw("POST", "/auth/resend", nil)
	if status != 200 || strings.Contains(string(raw), "devCode") {
		t.Fatalf("resend leaked a code: %d %s", status, raw)
	}
	// dev-only routes don't exist in production
	if status, _ := c.raw("GET", "/dev/outbox", nil); status != 404 {
		t.Errorf("/dev/outbox in production: %d", status)
	}
	var cfg struct {
		DevMode bool `json:"devMode"`
	}
	c.ok("GET", "/config", nil, &cfg)
	if cfg.DevMode {
		t.Error("/config reports dev mode in production")
	}
}

func TestConfigRefusesUnsafeProduction(t *testing.T) {
	base := map[string]string{
		"APP_ENV":             "production",
		"DATABASE_URL":        "postgres://x",
		"APP_ORIGIN":          "https://wearwhat.com",
		"DATA_KEY":            base64.StdEncoding.EncodeToString(make([]byte, 32)),
		"PAYSTACK_SECRET_KEY": "sk_live_x",
		"SMTP_HOST":           "smtp.test",
	}
	set := func(t *testing.T, without string, extra map[string]string) {
		for k, v := range base {
			if k != without {
				t.Setenv(k, v)
			} else {
				t.Setenv(k, "")
			}
		}
		for k, v := range extra {
			t.Setenv(k, v)
		}
	}
	t.Run("complete settings load", func(t *testing.T) {
		set(t, "", nil)
		if _, err := config.Load(); err != nil {
			t.Fatal(err)
		}
	})
	for _, missing := range []string{"PAYSTACK_SECRET_KEY", "DATA_KEY", "SMTP_HOST", "APP_ORIGIN"} {
		t.Run("needs "+missing, func(t *testing.T) {
			set(t, missing, nil)
			if _, err := config.Load(); err == nil {
				t.Fatalf("production started without %s", missing)
			}
		})
	}
	t.Run("demo seed refused", func(t *testing.T) {
		set(t, "", map[string]string{"SEED_DEMO": "true"})
		if _, err := config.Load(); err == nil {
			t.Fatal("production started with SEED_DEMO")
		}
	})
	t.Run("an unset environment is treated as production", func(t *testing.T) {
		set(t, "PAYSTACK_SECRET_KEY", map[string]string{"APP_ENV": ""})
		if _, err := config.Load(); err == nil {
			t.Fatal("unset APP_ENV must not unlock test payments")
		}
	})
}

func TestPasswordReset(t *testing.T) {
	e := newEnv(t)
	c := e.shopper()
	other := e.anon()
	other.ok("POST", "/auth/login", m{"email": c.email, "password": c.password}, nil)

	// same answer whether or not the account exists
	stranger := e.anon()
	stranger.ok("POST", "/auth/forgot", m{"email": "nobody@test.dev"}, nil)
	stranger.ok("POST", "/auth/forgot", m{"email": c.email}, nil)

	token := extractToken(t, e.mailTo(c.email, "Reset"))
	stranger.status(400, "POST", "/auth/reset", m{"token": token, "password": "short"})
	stranger.status(400, "POST", "/auth/reset", m{"token": "bogus-token", "password": "a-new-password"})
	stranger.ok("POST", "/auth/reset", m{"token": token, "password": "a-new-password"}, nil)

	// single use
	stranger.status(400, "POST", "/auth/reset", m{"token": token, "password": "another-password"})
	// every existing session is signed out
	c.status(401, "GET", "/me", nil)
	other.status(401, "GET", "/me", nil)
	// old password dead, new one works
	e.anon().status(401, "POST", "/auth/login", m{"email": c.email, "password": c.password})
	e.anon().ok("POST", "/auth/login", m{"email": c.email, "password": "a-new-password"}, nil)
}

func TestHandleAvailability(t *testing.T) {
	e := newEnv(t)
	c := e.shopper()
	var res struct {
		Available bool `json:"available"`
	}
	e.anon().ok("GET", "/handles/"+c.handle+"/available", nil, &res)
	if res.Available {
		t.Error("a taken handle reads as available")
	}
	// someone's own handle isn't "taken" for them
	c.ok("GET", "/handles/"+c.handle+"/available", nil, &res)
	if !res.Available {
		t.Error("your own handle should read as available")
	}
	e.anon().ok("GET", "/handles/brand.new/available", nil, &res)
	if !res.Available {
		t.Error("a free handle reads as taken")
	}
	e.anon().ok("GET", "/handles/no/available", nil, &res)
	if res.Available {
		t.Error("an invalid handle can't be available")
	}
	other := e.shopper()
	if msg := other.status(409, "PATCH", "/me", m{"handle": c.handle}); !strings.Contains(msg, "taken") {
		t.Errorf("message: %q", msg)
	}
}

func TestCrossSiteWritesBlocked(t *testing.T) {
	e := newEnv(t)
	c := e.shopper()
	send := func(origin string) int {
		req, _ := http.NewRequest("POST", e.srv.URL+api+"/auth/logout", nil)
		req.Header.Set("Origin", origin)
		res, err := c.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if got := send("https://evil.example"); got != 403 {
		t.Fatalf("a cross-site POST got %d", got)
	}
	c.ok("GET", "/me", nil, nil) // still signed in: the request was refused
	if got := send("http://app.test"); got != 204 {
		t.Fatalf("the app's own origin got %d", got)
	}
}

/* ---------- Apple ---------- */

type appleFixture struct {
	key *rsa.PrivateKey
	url string
}

func newAppleFixture(t *testing.T) *appleFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(m{"keys": []m{{
			"kty": "RSA", "kid": "k1", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	t.Cleanup(srv.Close)
	return &appleFixture{key: key, url: srv.URL}
}

func (a *appleFixture) token(t *testing.T, aud, sub, email string, exp time.Time, key *rsa.PrivateKey) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": "https://appleid.apple.com", "aud": aud, "sub": sub, "email": email, "exp": exp.Unix(),
	})
	tok.Header["kid"] = "k1"
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAppleSignIn(t *testing.T) {
	apple := newAppleFixture(t)
	e := newEnv(t, func(o *options) { o.apple = "com.wearwhat.web"; o.appleURL = apple.url })
	future := time.Now().Add(time.Hour)

	t.Run("a valid token signs in and creates the account once", func(t *testing.T) {
		c := e.anon()
		var res struct {
			IsNew bool `json:"isNew"`
		}
		c.ok("POST", "/auth/apple", m{"identityToken": apple.token(t, "com.wearwhat.web", "sub1", "a@privaterelay.test", future, apple.key), "kind": "shopper"}, &res)
		if !res.IsNew {
			t.Error("first sign-in should be new")
		}
		c.ok("GET", "/me", nil, nil)
		again := e.anon()
		again.ok("POST", "/auth/apple", m{"identityToken": apple.token(t, "com.wearwhat.web", "sub1", "a@privaterelay.test", future, apple.key)}, &res)
		if res.IsNew {
			t.Error("second sign-in should find the same account")
		}
		if n := e.count(`SELECT count(*) FROM users WHERE apple_sub = 'sub1'`); n != 1 {
			t.Errorf("%d accounts", n)
		}
	})

	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rejected := map[string]string{
		"wrong audience": apple.token(t, "com.someone.else", "sub2", "", future, apple.key),
		"expired":        apple.token(t, "com.wearwhat.web", "sub3", "", time.Now().Add(-time.Hour), apple.key),
		"forged key":     apple.token(t, "com.wearwhat.web", "sub4", "", future, otherKey),
		"garbage":        "not-a-token",
	}
	for name, token := range rejected {
		t.Run("rejects "+name, func(t *testing.T) {
			c := e.anon()
			c.status(401, "POST", "/auth/apple", m{"identityToken": token})
			c.status(401, "GET", "/me", nil)
		})
	}
	t.Run("rejects an unsigned token", func(t *testing.T) {
		tok := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{"iss": "https://appleid.apple.com", "aud": "com.wearwhat.web", "sub": "sub5", "exp": future.Unix()})
		s, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
		if err != nil {
			t.Fatal(err)
		}
		e.anon().status(401, "POST", "/auth/apple", m{"identityToken": s})
	})
}

func TestAppleUnavailableWithoutConfig(t *testing.T) {
	e := newEnv(t)
	e.anon().status(400, "POST", "/auth/apple", m{"identityToken": "anything"})
	var cfg struct {
		AppleClientID string `json:"appleClientId"`
		PhoneSignup   bool   `json:"phoneSignup"`
	}
	e.anon().ok("GET", "/config", nil, &cfg)
	if cfg.AppleClientID != "" || cfg.PhoneSignup {
		t.Errorf("config should hide unconfigured options: %+v", cfg)
	}
}
