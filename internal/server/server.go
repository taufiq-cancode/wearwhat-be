// Package server wires every feature together into one http.Handler.
package server

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"wearwhat-be/internal/account"
	"wearwhat-be/internal/app"
	"wearwhat-be/internal/auth"
	"wearwhat-be/internal/catalog"
	"wearwhat-be/internal/commerce"
	"wearwhat-be/internal/config"
	"wearwhat-be/internal/domain"
	"wearwhat-be/internal/httpx"
	"wearwhat-be/internal/inbox"
	"wearwhat-be/internal/jobs"
	"wearwhat-be/internal/media"
	"wearwhat-be/internal/notify"
	"wearwhat-be/internal/payments"
	"wearwhat-be/internal/social"
	"wearwhat-be/internal/studio"
)

const apiBase = "/api/v1"

// Server exposes the handler and background runner.
type Server struct {
	Handler http.Handler
	Jobs    *jobs.Runner
}

// DefaultDeps chooses real providers when keys are set, development fallbacks otherwise.
func DefaultDeps(cfg *config.Config, log *slog.Logger) (mailer notify.Mailer, sms notify.SMS, pay payments.Provider) {
	if cfg.MailEnabled() {
		mailer = &notify.SMTP{Host: cfg.SMTPHost, Port: cfg.SMTPPort, User: cfg.SMTPUser, Pass: cfg.SMTPPass, From: cfg.SMTPFrom}
	} else {
		mailer = &notify.Console{Log: log}
	}
	if cfg.SMSEnabled() {
		sms = &notify.Termii{Key: cfg.TermiiKey, Sender: cfg.TermiiSender, Base: cfg.TermiiBase}
	} else {
		sms = notify.NoSMS{}
	}
	if cfg.PaystackSecret != "" {
		pay = &payments.Paystack{Secret: cfg.PaystackSecret, Base: cfg.PaystackBase}
	} else {
		pay = payments.Testing{}
	}
	return mailer, sms, pay
}

// New registers every route.
func New(d *app.Deps) *Server {
	domain.SetLocation(d.Cfg.Location)
	mux := http.NewServeMux()
	authSvc := auth.New(d)
	router := &httpx.Router{Mux: mux, Auth: authSvc, Log: d.Log, Base: apiBase}

	accountSvc := &account.Service{Deps: d}
	commerceSvc := &commerce.Service{Deps: d}
	studioSvc := &studio.Service{Deps: d, Commerce: commerceSvc}
	inboxSvc := &inbox.Service{Deps: d, Actions: map[string]inbox.ActionHandler{
		"follow":  accountSvc.FollowAction,
		"approve": accountSvc.ApproveAction,
		"amplify": studioSvc.AmplifyAction,
	}}

	authSvc.Routes(router)
	accountSvc.Routes(router)
	(&catalog.Service{Deps: d}).Routes(router)
	(&social.Service{Deps: d}).Routes(router)
	commerceSvc.Routes(router)
	inboxSvc.Routes(router)
	studioSvc.Routes(router)
	(&media.Service{Deps: d, Store: media.LocalDisk{Dir: d.Cfg.UploadDir}}).Routes(router)

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := d.DB.Ping(r.Context()); err != nil {
			httpx.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "database unavailable"})
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// a development-only peek at outgoing messages, for testing flows with no mail server
	if console, ok := d.Mailer.(*notify.Console); ok && d.Cfg.IsDev() {
		mux.HandleFunc("GET "+apiBase+"/dev/outbox", func(w http.ResponseWriter, r *http.Request) {
			httpx.WriteJSON(w, http.StatusOK, console.Outbox(r.URL.Query().Get("to")))
		})
	}

	// anything else under the API base is a clean JSON 404, not the app's index page
	mux.HandleFunc(apiBase+"/", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusNotFound, map[string]string{"error": "Not found"})
	})
	if d.Cfg.StaticDir != "" {
		mux.Handle("/", spa(d.Cfg.StaticDir))
	}

	var h http.Handler = mux
	h = httpx.Origins(d.Cfg.Origins, []string{apiBase + "/webhooks/paystack"}, h)
	h = httpx.SecurityHeaders(h)
	h = httpx.Logging(d.Log, h)
	h = httpx.Recover(d.Log, h)

	return &Server{
		Handler: h,
		Jobs:    &jobs.Runner{Deps: d, Commerce: commerceSvc, Studio: studioSvc},
	}
}

// spa serves built frontend files, falling back to index.html so client-side routes load.
func spa(dir string) http.Handler {
	files := http.FileServer(http.Dir(dir))
	index := filepath.Join(dir, "index.html")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := filepath.Join(dir, filepath.Clean("/"+r.URL.Path))
		if info, err := os.Stat(clean); err == nil && !info.IsDir() {
			if strings.HasPrefix(r.URL.Path, "/assets/") {
				w.Header().Set("Cache-Control", "public, max-age=86400")
			}
			files.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, index)
	})
}

// RunJobs starts background work and stops it when ctx ends.
func (s *Server) RunJobs(ctx context.Context) { go s.Jobs.Run(ctx) }
