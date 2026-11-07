// Package httpx is the thin HTTP layer: routing with auth levels, JSON, errors and middleware.
package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"runtime/debug"
	"slices"
	"strings"
	"time"
)

/* ---------- errors ---------- */

// Error is shown to the user as-is; anything else becomes a generic 500.
type Error struct {
	Status int
	Msg    string
	Extra  map[string]any
}

func (e *Error) Error() string { return e.Msg }

func Err(status int, msg string) *Error { return &Error{Status: status, Msg: msg} }

func BadRequest(msg string) *Error { return Err(http.StatusBadRequest, msg) }
func NotFound(msg string) *Error   { return Err(http.StatusNotFound, msg) }
func Conflict(msg string) *Error   { return Err(http.StatusConflict, msg) }
func Forbidden(msg string) *Error  { return Err(http.StatusForbidden, msg) }

/* ---------- identity ---------- */

// Principal is the signed-in person and the brand they act for, if any.
type Principal struct {
	UserID    string
	Handle    string
	BrandID   string
	BrandRole string // OWNER | Admin | Orders | Content
	// Admin runs the wearwhat admin panel. It has nothing to do with the brand team role "Admin".
	Admin bool
}

// capability groups a team role can be limited to.
const (
	CapAny      = ""         // any team member
	CapOrders   = "orders"   // orders, returns, customer messages
	CapContent  = "content"  // catalog, posts, media, promotions
	CapSettings = "settings" // store, payouts, shipping, team
)

// Can reports whether the principal's team role includes a capability.
func (p *Principal) Can(capability string) bool {
	if p == nil || p.BrandID == "" {
		return false
	}
	switch p.BrandRole {
	case "OWNER", "Admin":
		return true
	case "Orders":
		return capability == CapAny || capability == CapOrders
	case "Content":
		return capability == CapAny || capability == CapContent
	}
	return false
}

type ctxKey struct{}

// Who returns the authenticated principal, or nil on a public route with no session.
func Who(r *http.Request) *Principal {
	p, _ := r.Context().Value(ctxKey{}).(*Principal)
	return p
}

// WithPrincipal stores the principal on a context.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// Authenticator turns a request's session cookie into a principal (nil when signed out).
type Authenticator interface {
	Authenticate(r *http.Request) (*Principal, error)
}

/* ---------- routing ---------- */

// Handler returns the JSON response body; nil means 204 No Content.
type Handler func(w http.ResponseWriter, r *http.Request) (any, error)

// Router registers API routes with their required auth level.
type Router struct {
	Mux  *http.ServeMux
	Auth Authenticator
	Log  *slog.Logger
	Base string // e.g. /api/v1
}

type level int

const (
	levelPublic level = iota
	levelUser
	levelBrand
	levelAdmin
)

// Public routes are open, but still resolve the principal when a session exists.
func (rt *Router) Public(pattern string, h Handler) { rt.add(pattern, h, levelPublic, "") }

// User routes need a signed-in account (401 otherwise).
func (rt *Router) User(pattern string, h Handler) { rt.add(pattern, h, levelUser, "") }

// Brand routes need a signed-in account on a brand team with the capability (403 otherwise).
func (rt *Router) Brand(pattern, capability string, h Handler) {
	rt.add(pattern, h, levelBrand, capability)
}

// Admin routes need a signed-in admin. Anyone else gets 404, so the panel isn't advertised.
func (rt *Router) Admin(pattern string, h Handler) { rt.add(pattern, h, levelAdmin, "") }

// File is a handler result sent as raw bytes rather than JSON. It's never cached, because
// files sent this way are private.
type File struct {
	ContentType string
	Name        string
	Data        []byte
}

func (rt *Router) add(pattern string, h Handler, lvl level, capability string) {
	method, path, ok := strings.Cut(pattern, " ")
	if !ok {
		panic("route needs a method: " + pattern)
	}
	rt.Mux.HandleFunc(method+" "+rt.Base+path, func(w http.ResponseWriter, r *http.Request) {
		p, err := rt.Auth.Authenticate(r)
		if err != nil {
			rt.fail(w, r, err)
			return
		}
		if lvl != levelPublic && p == nil {
			WriteJSON(w, http.StatusUnauthorized, map[string]any{"error": "Sign in to continue"})
			return
		}
		if lvl == levelAdmin && !p.Admin {
			WriteJSON(w, http.StatusNotFound, map[string]any{"error": "Not found"})
			return
		}
		if lvl == levelBrand {
			if p.BrandID == "" {
				WriteJSON(w, http.StatusForbidden, map[string]any{"error": "Set up your brand first"})
				return
			}
			if !p.Can(capability) {
				WriteJSON(w, http.StatusForbidden, map[string]any{"error": "Your team role doesn’t include this"})
				return
			}
		}
		r = r.WithContext(WithPrincipal(r.Context(), p))
		out, err := h(w, r)
		if err != nil {
			rt.fail(w, r, err)
			return
		}
		if isNil(out) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if f, ok := out.(*File); ok {
			writeFile(w, f)
			return
		}
		WriteJSON(w, http.StatusOK, out)
	})
}

func isNil(v any) bool { return v == nil }

func (rt *Router) fail(w http.ResponseWriter, r *http.Request, err error) {
	var e *Error
	if errors.As(err, &e) {
		body := map[string]any{"error": e.Msg}
		for k, v := range e.Extra {
			body[k] = v
		}
		WriteJSON(w, e.Status, body)
		return
	}
	rt.Log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	WriteJSON(w, http.StatusInternalServerError, map[string]any{"error": "Something went wrong — try again"})
}

// WriteJSON writes a JSON response.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Default().Debug("write response", "err", err)
	}
}

func writeFile(w http.ResponseWriter, f *File) {
	h := w.Header()
	h.Set("Content-Type", f.ContentType)
	h.Set("Content-Length", fmt.Sprint(len(f.Data)))
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": f.Name}))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(f.Data); err != nil {
		slog.Default().Debug("write file", "err", err)
	}
}

// Decode parses a JSON body into v. An empty body is an error.
func Decode(r *http.Request, v any) error {
	body := http.MaxBytesReader(nil, r.Body, 1<<20)
	if err := json.NewDecoder(body).Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return BadRequest("Missing request body")
		}
		return BadRequest("That request couldn’t be read")
	}
	return nil
}

// OK is the standard `{ ok: true }` response.
func OK() map[string]bool { return map[string]bool{"ok": true} }

/* ---------- middleware ---------- */

// Recover turns a panic into a 500 instead of dropping the connection.
func Recover(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				log.Error("panic", "path", r.URL.Path, "panic", fmt.Sprint(v), "stack", string(debug.Stack()))
				WriteJSON(w, http.StatusInternalServerError, map[string]any{"error": "Something went wrong — try again"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// Logging logs each request. Query strings are left out because reset tokens can appear in them.
// The address is the one rate limits see, which is how to check TRUSTED_PROXY_HOPS after a deploy.
func Logging(log *slog.Logger, proxyHops int, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		log.Info("http", "method", r.Method, "path", r.URL.Path, "status", sw.status, "ms", time.Since(start).Milliseconds(), "ip", ClientIP(r, proxyHops))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the real connection, to extend upload deadlines.
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// SecurityHeaders sets safe defaults on every response.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}

// Origins blocks cross-site writes and answers CORS for the configured app origins.
// exempt paths (provider webhooks) carry no cookies and authenticate by signature instead.
func Origins(allowed []string, exempt []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		known := origin != "" && slices.Contains(allowed, origin)
		if known {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Add("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			if known {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Accept")
				w.Header().Set("Access-Control-Max-Age", "600")
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		safe := r.Method == http.MethodGet || r.Method == http.MethodHead
		if !safe && origin != "" && !known && !slices.Contains(exempt, r.URL.Path) && !sameHost(origin, r.Host) {
			WriteJSON(w, http.StatusForbidden, map[string]any{"error": "Request blocked: unexpected origin"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func sameHost(origin, host string) bool {
	_, rest, ok := strings.Cut(origin, "://")
	return ok && rest == host
}

// ClientIP is the caller's address. Proxies add to the end of X-Forwarded-For and a client can
// put anything at the start, so it's counted from the right: proxyHops is how many proxies we
// run behind, and 0 ignores the header. A list too short, or an entry that isn't an address,
// falls back to the connection's address.
func ClientIP(r *http.Request, proxyHops int) string {
	if proxyHops > 0 {
		entries := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
		if i := len(entries) - proxyHops; i >= 0 {
			if ip := strings.TrimSpace(entries[i]); net.ParseIP(ip) != nil {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
