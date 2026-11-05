// Package app is the shared dependency container handed to each feature package.
package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"wearwhat-be/internal/config"
	"wearwhat-be/internal/notify"
	"wearwhat-be/internal/payments"
)

type Deps struct {
	DB       *pgxpool.Pool
	Cfg      *config.Config
	Log      *slog.Logger
	Mailer   notify.Mailer
	SMS      notify.SMS
	Payments payments.Provider
	// Now is overridable so tests can move the clock.
	Now func() time.Time
}

// SendMail delivers an email in the background so a slow mail server can't stall a request.
// Failures are logged; every message is also reflected in the app itself.
func (d *Deps) SendMail(to, subject, body string) {
	if to == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := d.Mailer.Send(ctx, to, subject, body); err != nil {
			d.Log.Error("send email", "subject", subject, "err", err)
		}
	}()
}
