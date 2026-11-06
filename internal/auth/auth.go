// Package auth handles sign-up, verification, sessions, password reset and Apple sign-in.
package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"wearwhat-be/internal/app"
	"wearwhat-be/internal/db"
	"wearwhat-be/internal/domain"
	"wearwhat-be/internal/httpx"
	"wearwhat-be/internal/secure"
)

const (
	cookieName     = "ww_session"
	sessionTTL     = 30 * 24 * time.Hour
	pendingTTL     = 30 * time.Minute
	codeTTL        = 15 * time.Minute
	maxCodeTries   = 5
	resetTTL       = time.Hour
	minPasswordLen = 8
)

type Service struct {
	*app.Deps
	apple *appleVerifier
}

func New(d *app.Deps) *Service {
	return &Service{Deps: d, apple: newAppleVerifier(d.Cfg.AppleKeysURL, d.Cfg.AppleClientID)}
}

func (s *Service) Routes(r *httpx.Router) {
	r.Public("POST /auth/signup", s.signup)
	r.Public("POST /auth/verify", s.verify)
	r.Public("POST /auth/resend", s.resend)
	r.Public("POST /auth/login", s.login)
	r.Public("POST /auth/logout", s.logout)
	r.Public("POST /auth/forgot", s.forgot)
	r.Public("POST /auth/reset", s.reset)
	r.Public("POST /auth/apple", s.appleSignIn)
	r.Public("GET /handles/{handle}/available", s.handleAvailable)
	r.Public("GET /config", s.publicConfig)
}

/* ---------- sessions ---------- */

// Authenticate implements httpx.Authenticator. Pending sessions count as signed out.
func (s *Service) Authenticate(r *http.Request) (*httpx.Principal, error) {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return nil, nil
	}
	hash := secure.Hash(c.Value)
	var p httpx.Principal
	var pending bool
	var expires time.Time
	err = s.DB.QueryRow(r.Context(), `SELECT u.id, u.handle, s.pending, s.expires_at,
			COALESCE(t.brand_id, ''), COALESCE(t.role, '')
		FROM sessions s JOIN users u ON u.id = s.user_id
		LEFT JOIN team_members t ON t.user_id = u.id AND t.status = 'active'
		WHERE s.token_hash = $1 AND s.expires_at > now()`, hash).
		Scan(&p.UserID, &p.Handle, &pending, &expires, &p.BrandID, &p.BrandRole)
	if db.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if pending {
		return nil, nil
	}
	// keep active people signed in: extend once half the session has elapsed
	if time.Until(expires) < sessionTTL/2 {
		if _, err := s.DB.Exec(r.Context(), `UPDATE sessions SET expires_at = $2 WHERE token_hash = $1`, hash, time.Now().Add(sessionTTL)); err != nil {
			s.Log.Warn("extend session", "err", err)
		}
	}
	return &p, nil
}

func (s *Service) startSession(ctx context.Context, q db.Q, w http.ResponseWriter, userID string, pending bool) error {
	token := secure.Token()
	ttl := sessionTTL
	if pending {
		ttl = pendingTTL
	}
	if _, err := q.Exec(ctx, `INSERT INTO sessions (token_hash, user_id, pending, expires_at) VALUES ($1,$2,$3,$4)`,
		secure.Hash(token), userID, pending, time.Now().Add(ttl)); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   !s.Cfg.IsDev(),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(ttl.Seconds()),
	})
	return nil
}

func (s *Service) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true, Secure: !s.Cfg.IsDev(), SameSite: http.SameSiteLaxMode, MaxAge: -1})
}

// pendingUser finds the account a pending session is verifying.
func (s *Service) pendingUser(r *http.Request) (string, error) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return "", httpx.BadRequest("Start again from the sign-up screen")
	}
	var id string
	err = s.DB.QueryRow(r.Context(), `SELECT user_id FROM sessions WHERE token_hash = $1 AND pending AND expires_at > now()`, secure.Hash(c.Value)).Scan(&id)
	if db.IsNotFound(err) {
		return "", httpx.BadRequest("Start again from the sign-up screen")
	}
	return id, err
}

func (s *Service) ip(r *http.Request) string { return httpx.ClientIP(r, s.Cfg.TrustedProxyHops) }

// perIP scales a per-address limit: in development every request comes from one machine.
func (s *Service) perIP(limit int) int {
	if s.Cfg.IsDev() {
		return limit * 50
	}
	return limit
}

/* ---------- sign-up ---------- */

type signupInput struct {
	Method   string `json:"method"`
	Email    string `json:"email"`
	Phone    string `json:"phone"`
	Password string `json:"password"`
	Kind     string `json:"kind"`
}

type codeResponse struct {
	OK      bool   `json:"ok"`
	DevCode string `json:"devCode,omitempty"`
}

func (s *Service) signup(w http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	if err := httpx.Limit(ctx, s.DB, "signup:"+s.ip(r), s.perIP(20), time.Hour, "Too many attempts — try again later"); err != nil {
		return nil, err
	}
	var in signupInput
	if err := httpx.Decode(r, &in); err != nil {
		return nil, err
	}
	kind := "shopper"
	if in.Kind == "brand" {
		kind = "brand"
	}
	var email, phone *string
	switch in.Method {
	case "Phone":
		if !s.Cfg.SMSEnabled() {
			return nil, httpx.BadRequest("Phone sign-up isn’t available right now — use email")
		}
		p := domain.NormalizePhone(in.Phone)
		if !domain.ValidPhone(p) {
			return nil, httpx.BadRequest("Enter your phone number")
		}
		phone = &p
	case "Email":
		e := strings.ToLower(strings.TrimSpace(in.Email))
		if !domain.ValidEmail(e) {
			return nil, httpx.BadRequest("Enter a valid email")
		}
		email = &e
	default:
		return nil, httpx.BadRequest("Choose how to sign up")
	}
	if len(in.Password) < minPasswordLen {
		return nil, httpx.BadRequest("Passwords need 8+ characters")
	}
	if len(in.Password) > 200 {
		return nil, httpx.BadRequest("That password is too long")
	}
	hash := secure.HashPassword(in.Password)

	var userID, code string
	err := db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		var verifiedAt *time.Time
		err := tx.QueryRow(ctx, `SELECT id, verified_at FROM users WHERE email = $1 OR phone = $2 FOR UPDATE`, email, phone).Scan(&userID, &verifiedAt)
		switch {
		case err == nil && verifiedAt != nil:
			return httpx.Conflict("An account with this " + contactWord(in.Method) + " already exists — log in instead")
		case err == nil:
			// an unfinished sign-up: whoever proves the address next owns the account
			if _, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2, signup_kind = $3 WHERE id = $1`, userID, hash, kind); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, userID); err != nil {
				return err
			}
		case db.IsNotFound(err):
			userID = secure.ID("usr")
			handle, name := provisional(email, phone)
			handle, err = uniqueHandle(ctx, tx, handle)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO users (id, handle, name, email, phone, password_hash, signup_kind)
				VALUES ($1,$2,$3,$4,$5,$6,$7)`, userID, handle, name, email, phone, hash, kind); err != nil {
				return err
			}
		default:
			return err
		}
		if code, err = s.issueCode(ctx, tx, userID); err != nil {
			return err
		}
		return s.startSession(ctx, tx, w, userID, true)
	})
	if err != nil {
		return nil, err
	}
	s.sendCode(ctx, email, phone, code)
	return s.codeResponse(code), nil
}

func contactWord(method string) string {
	if method == "Phone" {
		return "phone number"
	}
	return "email"
}

func provisional(email, phone *string) (handle, name string) {
	base := "user"
	if email != nil {
		base, _, _ = strings.Cut(*email, "@")
		name = base
	} else if phone != nil {
		name = "New member"
	}
	var b strings.Builder
	for _, r := range strings.ToLower(base) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' {
			b.WriteRune(r)
		}
	}
	h := b.String()
	if len(h) < 3 {
		h = "user"
	}
	if len(h) > 20 {
		h = h[:20]
	}
	return h, name
}

func uniqueHandle(ctx context.Context, q db.Q, base string) (string, error) {
	candidate := base
	for i := 0; i < 20; i++ {
		var taken bool
		if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE handle = $1)`, candidate).Scan(&taken); err != nil {
			return "", err
		}
		if !taken {
			return candidate, nil
		}
		code := secure.Code()
		candidate = base + "." + code[:4]
	}
	return "", fmt.Errorf("could not find a free handle for %q", base)
}

func (s *Service) issueCode(ctx context.Context, q db.Q, userID string) (string, error) {
	code := secure.Code()
	_, err := q.Exec(ctx, `INSERT INTO verification_codes (user_id, code_hash, attempts, expires_at)
		VALUES ($1,$2,0,$3) ON CONFLICT (user_id) DO UPDATE SET code_hash = $2, attempts = 0, expires_at = $3, created_at = now()`,
		userID, secure.Hash(userID+":"+code), time.Now().Add(codeTTL))
	return code, err
}

func (s *Service) sendCode(ctx context.Context, email, phone *string, code string) {
	body := fmt.Sprintf("Your wearwhat code is %s. It expires in 15 minutes.", code)
	if email != nil {
		s.SendMail(*email, "Your wearwhat code", body)
		return
	}
	if phone != nil {
		// a failed SMS shouldn't reveal anything; the person can ask for a resend
		if err := s.SMS.Send(ctx, *phone, body); err != nil {
			s.Log.Error("send sms", "err", err)
		}
	}
}

// codeResponse includes the code itself only in development, so the app works with no mail server.
func (s *Service) codeResponse(code string) codeResponse {
	out := codeResponse{OK: true}
	if s.Cfg.IsDev() {
		out.DevCode = code
	}
	return out
}

func (s *Service) verify(w http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	userID, err := s.pendingUser(r)
	if err != nil {
		return nil, err
	}
	var in struct {
		Code string `json:"code"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return nil, err
	}
	if len(in.Code) != 6 {
		return nil, httpx.BadRequest("Enter the 6-digit code")
	}
	var wrong error
	err = db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		var hash []byte
		var attempts int
		var expires time.Time
		err := tx.QueryRow(ctx, `SELECT code_hash, attempts, expires_at FROM verification_codes WHERE user_id = $1 FOR UPDATE`, userID).Scan(&hash, &attempts, &expires)
		if db.IsNotFound(err) {
			wrong = httpx.BadRequest("That code has expired — request a new one")
			return nil
		}
		if err != nil {
			return err
		}
		if time.Now().After(expires) {
			wrong = httpx.BadRequest("That code has expired — request a new one")
			return nil
		}
		if attempts >= maxCodeTries {
			wrong = httpx.Err(http.StatusTooManyRequests, "Too many tries — request a new code")
			return nil
		}
		if !hmacEqual(hash, secure.Hash(userID+":"+in.Code)) {
			// committed deliberately, so the attempt counts even though verification failed
			if _, err := tx.Exec(ctx, `UPDATE verification_codes SET attempts = attempts + 1 WHERE user_id = $1`, userID); err != nil {
				return err
			}
			wrong = httpx.BadRequest("That code isn’t right — check it and try again")
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET verified_at = now() WHERE id = $1`, userID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM verification_codes WHERE user_id = $1`, userID); err != nil {
			return err
		}
		// a fresh token replaces the pending one
		if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1 AND pending`, userID); err != nil {
			return err
		}
		return s.startSession(ctx, tx, w, userID, false)
	})
	if err != nil {
		return nil, err
	}
	if wrong != nil {
		return nil, wrong
	}
	return httpx.OK(), nil
}

func (s *Service) resend(_ http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	userID, err := s.pendingUser(r)
	if err != nil {
		return nil, err
	}
	if err := httpx.Limit(ctx, s.DB, "resend:"+userID, 5, time.Hour, "Too many codes requested — try again later"); err != nil {
		return nil, err
	}
	var email, phone *string
	if err := s.DB.QueryRow(ctx, `SELECT email, phone FROM users WHERE id = $1`, userID).Scan(&email, &phone); err != nil {
		return nil, err
	}
	code, err := s.issueCode(ctx, s.DB, userID)
	if err != nil {
		return nil, err
	}
	s.sendCode(ctx, email, phone, code)
	return s.codeResponse(code), nil
}

/* ---------- login ---------- */

func (s *Service) login(w http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	var in struct {
		Email    string `json:"email"`
		Phone    string `json:"phone"`
		Password string `json:"password"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return nil, err
	}
	identifier := strings.ToLower(strings.TrimSpace(in.Email))
	phone := domain.NormalizePhone(in.Phone)
	// the login field takes either: an entry with no @ is treated as a phone number
	if identifier != "" && !strings.Contains(identifier, "@") {
		phone = domain.NormalizePhone(identifier)
		identifier = ""
	}
	if (identifier == "" && phone == "") || in.Password == "" {
		return nil, httpx.BadRequest("Enter your email and password")
	}
	busy := "Too many attempts — wait a few minutes and try again"
	if err := httpx.Limit(ctx, s.DB, "login-ip:"+s.ip(r), s.perIP(60), 10*time.Minute, busy); err != nil {
		return nil, err
	}
	// per account, only wrong passwords count: signing in often must never lock anyone out
	accountKey := "login:" + identifier + phone
	locked, err := httpx.Exhausted(ctx, s.DB, accountKey, 10, 10*time.Minute)
	if err != nil {
		return nil, err
	}
	if locked {
		return nil, httpx.Err(http.StatusTooManyRequests, busy)
	}
	countFailure := func() error {
		_, err := httpx.Allow(ctx, s.DB, accountKey, 10, 10*time.Minute)
		return err
	}

	var userID, kind string
	var hash *string
	var verifiedAt *time.Time
	var email, userPhone *string
	err = s.DB.QueryRow(ctx, `SELECT id, password_hash, verified_at, signup_kind, email, phone FROM users
		WHERE ($1 <> '' AND email = $1) OR ($2 <> '' AND phone = $2)`, identifier, phone).
		Scan(&userID, &hash, &verifiedAt, &kind, &email, &userPhone)
	if err != nil && !db.IsNotFound(err) {
		return nil, err
	}
	badLogin := httpx.Err(http.StatusUnauthorized, "That email or password isn’t right")
	if db.IsNotFound(err) || hash == nil {
		secure.DummyPasswordCheck(in.Password)
		if err := countFailure(); err != nil {
			return nil, err
		}
		return nil, badLogin
	}
	if !secure.CheckPassword(*hash, in.Password) {
		if err := countFailure(); err != nil {
			return nil, err
		}
		return nil, badLogin
	}
	if err := httpx.Clear(ctx, s.DB, accountKey); err != nil {
		return nil, err
	}
	if verifiedAt == nil {
		// right password on an unfinished sign-up: send them back to the code step
		code, err := s.issueCode(ctx, s.DB, userID)
		if err != nil {
			return nil, err
		}
		if err := s.startSession(ctx, s.DB, w, userID, true); err != nil {
			return nil, err
		}
		s.sendCode(ctx, email, userPhone, code)
		extra := map[string]any{"needsVerification": true, "kind": kind}
		if s.Cfg.IsDev() {
			extra["devCode"] = code
		}
		return nil, &httpx.Error{Status: http.StatusForbidden, Msg: "Finish signing up — we sent you a new code", Extra: extra}
	}
	if err := s.startSession(ctx, s.DB, w, userID, false); err != nil {
		return nil, err
	}
	return httpx.OK(), nil
}

func (s *Service) logout(w http.ResponseWriter, r *http.Request) (any, error) {
	if c, err := r.Cookie(cookieName); err == nil {
		if _, err := s.DB.Exec(r.Context(), `DELETE FROM sessions WHERE token_hash = $1`, secure.Hash(c.Value)); err != nil {
			return nil, err
		}
	}
	s.clearCookie(w)
	return nil, nil
}

/* ---------- password reset ---------- */

// forgot always answers the same way, so it can't be used to discover who has an account.
func (s *Service) forgot(_ http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	var in struct {
		Email string `json:"email"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return nil, err
	}
	if err := httpx.Limit(ctx, s.DB, "forgot-ip:"+s.ip(r), s.perIP(20), time.Hour, "Too many requests — try again later"); err != nil {
		return nil, err
	}
	ident := strings.ToLower(strings.TrimSpace(in.Email))
	phone := ""
	if !strings.Contains(ident, "@") {
		phone = domain.NormalizePhone(ident)
		ident = ""
	}
	if ident == "" && phone == "" {
		return nil, httpx.BadRequest("Enter your email")
	}
	if ok, err := httpx.Allow(ctx, s.DB, "forgot:"+ident+phone, 5, time.Hour); err != nil {
		return nil, err
	} else if !ok {
		return httpx.OK(), nil
	}
	var userID string
	var email, userPhone *string
	err := s.DB.QueryRow(ctx, `SELECT id, email, phone FROM users WHERE ($1 <> '' AND email = $1) OR ($2 <> '' AND phone = $2)`, ident, phone).Scan(&userID, &email, &userPhone)
	if db.IsNotFound(err) {
		return httpx.OK(), nil
	}
	if err != nil {
		return nil, err
	}
	token := secure.Token()
	if _, err := s.DB.Exec(ctx, `INSERT INTO password_resets (token_hash, user_id, expires_at) VALUES ($1,$2,$3)`,
		secure.Hash(token), userID, time.Now().Add(resetTTL)); err != nil {
		return nil, err
	}
	link := s.Cfg.Origin() + "/reset?token=" + token
	body := "Reset your wearwhat password with this link (it expires in 1 hour):\n\n" + link + "\n\nIf this wasn’t you, you can ignore this message."
	if email != nil {
		s.SendMail(*email, "Reset your wearwhat password", body)
	} else if userPhone != nil {
		if err := s.SMS.Send(ctx, *userPhone, body); err != nil {
			s.Log.Error("send sms", "err", err)
		}
	}
	return httpx.OK(), nil
}

func (s *Service) reset(_ http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	var in struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return nil, err
	}
	if err := httpx.Limit(ctx, s.DB, "reset-ip:"+s.ip(r), s.perIP(30), time.Hour, "Too many attempts — try again later"); err != nil {
		return nil, err
	}
	if len(in.Password) < minPasswordLen {
		return nil, httpx.BadRequest("Passwords need 8+ characters")
	}
	if len(in.Password) > 200 {
		return nil, httpx.BadRequest("That password is too long")
	}
	invalid := httpx.BadRequest("That link has expired — request a new one")
	var failed error
	err := db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		var userID string
		// atomic claim: a token can only ever be spent once
		err := tx.QueryRow(ctx, `UPDATE password_resets SET used_at = now()
			WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now() RETURNING user_id`, secure.Hash(in.Token)).Scan(&userID)
		if db.IsNotFound(err) {
			failed = invalid
			return nil
		}
		if err != nil {
			return err
		}
		// proving control of the address also completes an unfinished sign-up
		if _, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2, verified_at = COALESCE(verified_at, now()) WHERE id = $1`,
			userID, secure.HashPassword(in.Password)); err != nil {
			return err
		}
		// everything signed in with the old password is logged out
		_, err = tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, userID)
		return err
	})
	if err != nil {
		return nil, err
	}
	if failed != nil {
		return nil, failed
	}
	return httpx.OK(), nil
}

/* ---------- handles and config ---------- */

func (s *Service) handleAvailable(_ http.ResponseWriter, r *http.Request) (any, error) {
	h := domain.NormalizeHandle(r.PathValue("handle"))
	kind := r.URL.Query().Get("kind")
	if !domain.ValidHandle(h) {
		return map[string]bool{"available": false}, nil
	}
	table := "users"
	if kind == "brand" {
		table = "brands"
	}
	var taken bool
	err := s.DB.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM `+table+` WHERE handle = $1 AND ($2 = '' OR id <> $2))`, h, currentOwnerID(r, table)).Scan(&taken)
	return map[string]bool{"available": !taken}, err
}

// currentOwnerID lets someone keep their own handle without it reading as taken.
func currentOwnerID(r *http.Request, table string) string {
	who := httpx.Who(r)
	if who == nil {
		return ""
	}
	if table == "brands" {
		return who.BrandID
	}
	return who.UserID
}

func (s *Service) publicConfig(_ http.ResponseWriter, _ *http.Request) (any, error) {
	return map[string]any{
		"testPayments":   s.Payments.Test(),
		"paymentMethods": s.Payments.Methods(),
		"phoneSignup":    s.Cfg.SMSEnabled(),
		"appleClientId":  s.Cfg.AppleClientID,
		"supportEmail":   s.Cfg.SupportEmail,
		"devMode":        s.Cfg.IsDev(),
	}, nil
}
