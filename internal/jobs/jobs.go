// Package jobs runs the recurring background work. Every step is safe to run from several servers.
package jobs

import (
	"context"
	"fmt"
	"strings"
	"time"

	"wearwhat-be/internal/app"
	"wearwhat-be/internal/commerce"
	"wearwhat-be/internal/studio"
	"wearwhat-be/internal/views"
)

const interval = 30 * time.Second

type Runner struct {
	*app.Deps
	Commerce *commerce.Service
	Studio   *studio.Service
}

// Run ticks until ctx is cancelled. A failing step is logged and retried on the next tick.
func (j *Runner) Run(ctx context.Context) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	j.Tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			j.Tick(ctx)
		}
	}
}

// Tick runs each step once.
func (j *Runner) Tick(ctx context.Context) {
	steps := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"publish posts", j.Studio.PublishDuePosts},
		{"release drops", j.Studio.ReleaseDueDrops},
		{"expire checkouts", j.Commerce.ExpireCheckouts},
		{"weekly digest", j.SendDigests},
		{"cleanup", j.cleanup},
	}
	for _, s := range steps {
		if err := s.fn(ctx); err != nil && ctx.Err() == nil {
			j.Log.Error("background job failed", "job", s.name, "err", err)
		}
	}
}

func (j *Runner) cleanup(ctx context.Context) error {
	for _, q := range []string{
		`DELETE FROM sessions WHERE expires_at < now()`,
		`DELETE FROM verification_codes WHERE expires_at < now() - interval '1 day'`,
		`DELETE FROM password_resets WHERE expires_at < now() - interval '1 day'`,
		`DELETE FROM rate_limits WHERE window_start < now() - interval '1 day'`,
	} {
		if _, err := j.DB.Exec(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// SendDigests emails people who opted in with new pieces from the brands they follow, once a week.
func (j *Runner) SendDigests(ctx context.Context) error {
	now := j.Now()
	rows, err := j.DB.Query(ctx, `SELECT id, email, name FROM users
		WHERE COALESCE((prefs ->> 'digest')::boolean, false) AND email IS NOT NULL AND verified_at IS NOT NULL
		AND (last_digest_at IS NULL OR last_digest_at < $1::timestamptz - interval '7 days') LIMIT 50`, now)
	if err != nil {
		return err
	}
	type person struct{ id, email, name string }
	var due []person
	for rows.Next() {
		var p person
		if err := rows.Scan(&p.id, &p.email, &p.name); err != nil {
			rows.Close()
			return err
		}
		due = append(due, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range due {
		products, err := views.ProductsWhere(ctx, j.DB, `WHERE `+views.ProductVisible+` AND p.created_at > $2::timestamptz - interval '7 days'
			AND p.brand_id IN (SELECT brand_id FROM follows_brand WHERE user_id = $1) ORDER BY p.created_at DESC LIMIT 5`, p.id, now)
		if err != nil {
			return err
		}
		if len(products) > 0 {
			lines := make([]string, 0, len(products))
			for _, x := range products {
				lines = append(lines, fmt.Sprintf("%s — %s ($%.2f)\n%s/p/%s", x.Summary.Brand.Name, x.Summary.Name, float64(x.Summary.Price)/100, j.Cfg.Origin(), x.Summary.ID))
			}
			j.SendMail(p.email, "New this week from brands you follow",
				"Hi "+p.name+", here’s what’s new:\n\n"+strings.Join(lines, "\n\n")+"\n\nTurn this email off any time in Menu → Email digest.")
		}
		if _, err := j.DB.Exec(ctx, `UPDATE users SET last_digest_at = $2 WHERE id = $1`, p.id, now); err != nil {
			return err
		}
	}
	return nil
}
