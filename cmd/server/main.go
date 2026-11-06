// Command server runs the wearwhat HTTP API.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"wearwhat-be/internal/app"
	"wearwhat-be/internal/config"
	"wearwhat-be/internal/db"
	"wearwhat-be/internal/seed"
	"wearwhat-be/internal/server"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
	_ = godotenv.Load() // .env is optional: real deployments set the environment directly
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx, cfg.DatabaseURL, "")
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}
	if cfg.SeedDemo {
		if err := seed.IfEmpty(ctx, pool, cfg.Location, cfg.DataKey); err != nil {
			return err
		}
	}

	mailer, sms, pay := server.DefaultDeps(cfg, log)
	deps := &app.Deps{DB: pool, Cfg: cfg, Log: log, Mailer: mailer, SMS: sms, Payments: pay, Now: time.Now}
	srv := server.New(deps)
	srv.RunJobs(ctx)

	httpSrv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           srv.Handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute, // large video uploads
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Info("wearwhat API listening", "addr", httpSrv.Addr, "env", cfg.Env, "testPayments", pay.Test(), "email", cfg.MailEnabled(), "cloudinary", cfg.CloudinaryEnabled(), "sms", cfg.SMSEnabled())
		errCh <- httpSrv.ListenAndServe()
	}()
	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}
