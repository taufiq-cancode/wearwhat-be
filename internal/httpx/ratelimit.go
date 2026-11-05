package httpx

import (
	"context"
	"net/http"
	"time"

	"wearwhat-be/internal/db"
)

// Allow counts a hit against key in a fixed time window stored in Postgres, so limits
// hold across restarts and across several server instances. It returns false past the limit.
func Allow(ctx context.Context, q db.Q, key string, limit int, window time.Duration) (bool, error) {
	var hits int
	err := q.QueryRow(ctx, `
		INSERT INTO rate_limits (key, window_start, hits) VALUES ($1, now(), 1)
		ON CONFLICT (key) DO UPDATE SET
			hits = CASE WHEN rate_limits.window_start < now() - make_interval(secs => $2) THEN 1 ELSE rate_limits.hits + 1 END,
			window_start = CASE WHEN rate_limits.window_start < now() - make_interval(secs => $2) THEN now() ELSE rate_limits.window_start END
		RETURNING hits`, key, window.Seconds()).Scan(&hits)
	if err != nil {
		return false, err
	}
	return hits <= limit, nil
}

// Exhausted reports whether key has already reached its limit, without counting a hit.
// Use it with Allow when only some outcomes should count, such as failed logins.
func Exhausted(ctx context.Context, q db.Q, key string, limit int, window time.Duration) (bool, error) {
	var hits int
	err := q.QueryRow(ctx, `SELECT hits FROM rate_limits WHERE key = $1 AND window_start >= now() - make_interval(secs => $2)`, key, window.Seconds()).Scan(&hits)
	if db.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return hits >= limit, nil
}

// Clear forgets a key's hits, for example after a successful login.
func Clear(ctx context.Context, q db.Q, key string) error {
	_, err := q.Exec(ctx, `DELETE FROM rate_limits WHERE key = $1`, key)
	return err
}

// Limit is Allow as a request guard, returning a 429 the user can act on.
func Limit(ctx context.Context, q db.Q, key string, limit int, window time.Duration, msg string) error {
	ok, err := Allow(ctx, q, key, limit, window)
	if err != nil {
		return err
	}
	if !ok {
		return Err(http.StatusTooManyRequests, msg)
	}
	return nil
}
