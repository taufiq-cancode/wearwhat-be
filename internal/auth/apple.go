package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"

	"wearwhat-be/internal/db"
	"wearwhat-be/internal/domain"
	"wearwhat-be/internal/httpx"
	"wearwhat-be/internal/secure"
)

func hmacEqual(a, b []byte) bool { return hmac.Equal(a, b) }

// appleVerifier checks identity tokens against Apple's published signing keys.
type appleVerifier struct {
	keysURL  string
	clientID string
	client   *http.Client

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

func newAppleVerifier(keysURL, clientID string) *appleVerifier {
	return &appleVerifier{keysURL: keysURL, clientID: clientID, client: &http.Client{Timeout: 10 * time.Second}}
}

type appleClaims struct {
	Email string `json:"email"`
	Nonce string `json:"nonce"`
	jwt.RegisteredClaims
}

func (a *appleVerifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if k := a.keys[kid]; k != nil && time.Since(a.fetched) < time.Hour {
		return k, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.keysURL, nil)
	if err != nil {
		return nil, err
	}
	res, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var set struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(res.Body).Decode(&set); err != nil {
		return nil, err
	}
	keys := make(map[string]*rsa.PublicKey, len(set.Keys))
	for _, k := range set.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	a.keys, a.fetched = keys, time.Now()
	if k := keys[kid]; k != nil {
		return k, nil
	}
	return nil, fmt.Errorf("apple key %q not found", kid)
}

// verify validates signature, issuer, audience, expiry and nonce, and returns the claims.
func (a *appleVerifier) verify(ctx context.Context, token, nonce string) (*appleClaims, error) {
	claims := &appleClaims{}
	_, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return a.key(ctx, kid)
	},
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer("https://appleid.apple.com"),
		jwt.WithAudience(a.clientID),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, err
	}
	if claims.Subject == "" {
		return nil, errors.New("apple token has no subject")
	}
	if nonce != "" && claims.Nonce != nonce {
		return nil, errors.New("apple token nonce mismatch")
	}
	return claims, nil
}

// appleSignIn signs in with a verified Apple identity token, creating the account if needed.
func (s *Service) appleSignIn(w http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	if s.Cfg.AppleClientID == "" {
		return nil, httpx.BadRequest("Sign in with Apple isn’t available right now")
	}
	if err := httpx.Limit(ctx, s.DB, "apple:"+s.ip(r), s.perIP(30), 10*time.Minute, "Too many attempts — try again later"); err != nil {
		return nil, err
	}
	var in struct {
		IdentityToken string `json:"identityToken"`
		Nonce         string `json:"nonce"`
		Kind          string `json:"kind"`
		Name          string `json:"name"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return nil, err
	}
	claims, err := s.apple.verify(ctx, in.IdentityToken, in.Nonce)
	if err != nil {
		s.Log.Warn("apple sign-in rejected", "err", err)
		return nil, httpx.Err(http.StatusUnauthorized, "Apple sign-in couldn’t be verified — try again")
	}
	kind := "shopper"
	if in.Kind == "brand" {
		kind = "brand"
	}
	email := strings.ToLower(strings.TrimSpace(claims.Email))
	isNew := false
	err = db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		var userID string
		err := tx.QueryRow(ctx, `SELECT id FROM users WHERE apple_sub = $1`, claims.Subject).Scan(&userID)
		if db.IsNotFound(err) && email != "" {
			// only an already-verified account can be linked by address; Apple has verified this one too
			err = tx.QueryRow(ctx, `UPDATE users SET apple_sub = $2 WHERE email = $1 AND verified_at IS NOT NULL RETURNING id`, email, claims.Subject).Scan(&userID)
		}
		if db.IsNotFound(err) {
			isNew = true
			userID = secure.ID("usr")
			var emailArg *string
			if email != "" {
				// an abandoned, unverified sign-up must not block the real owner of this address
				if _, err := tx.Exec(ctx, `DELETE FROM users WHERE email = $1 AND verified_at IS NULL`, email); err != nil {
					return err
				}
				emailArg = &email
			}
			base, name := provisional(emailArg, nil)
			if strings.TrimSpace(in.Name) != "" {
				name = strings.TrimSpace(in.Name)
			}
			if name == "" {
				name = "New member"
			}
			handle, err := uniqueHandle(ctx, tx, base)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `INSERT INTO users (id, handle, name, email, apple_sub, signup_kind, verified_at)
				VALUES ($1,$2,$3,$4,$5,$6,now())`, userID, domain.NormalizeHandle(handle), name, emailArg, claims.Subject, kind)
			if err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		return s.startSession(ctx, tx, w, userID, false)
	})
	if err != nil {
		return nil, err
	}
	return map[string]bool{"ok": true, "isNew": isNew}, nil
}
