// Package notify sends email and SMS. Each has a real provider and a console fallback for development.
package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Message is an outgoing email or SMS, kept simple and plain-text.
type Message struct {
	To      string    `json:"to"`
	Subject string    `json:"subject"`
	Body    string    `json:"body"`
	At      time.Time `json:"at"`
}

type Mailer interface {
	Send(ctx context.Context, to, subject, body string) error
}

type SMS interface {
	Send(ctx context.Context, to, body string) error
}

/* ---------- console (development) ---------- */

// Console prints messages to the log and keeps recent ones for GET /dev/outbox.
type Console struct {
	Log *slog.Logger
	mu  sync.Mutex
	box []Message
}

func (c *Console) record(m Message) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.box = append(c.box, m)
	if len(c.box) > 200 {
		c.box = c.box[len(c.box)-200:]
	}
}

// Send implements Mailer.
func (c *Console) Send(_ context.Context, to, subject, body string) error {
	c.record(Message{To: to, Subject: subject, Body: body, At: time.Now()})
	c.Log.Info("email (console)", "to", to, "subject", subject, "body", body)
	return nil
}

// Outbox returns recent messages, newest first, optionally only those to one address.
func (c *Console) Outbox(to string) []Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []Message{}
	for i := len(c.box) - 1; i >= 0; i-- {
		if to == "" || strings.EqualFold(c.box[i].To, to) {
			out = append(out, c.box[i])
		}
	}
	return out
}

/* ---------- Resend ---------- */

const (
	resendAttempts = 3
	resendMaxWait  = 5 * time.Second
)

// Resend sends email through Resend's HTTPS API.
type Resend struct {
	Key, From, Base string
	Client          *http.Client
	// Backoff is the wait before the first retry, doubling after that. Tests shorten it.
	Backoff time.Duration
}

// Send implements Mailer. Rate limits and server errors are retried. The same idempotency key
// goes with every attempt, so a retry can't deliver the message twice.
func (s *Resend) Send(ctx context.Context, to, subject, body string) error {
	payload, err := json.Marshal(map[string]any{"from": s.From, "to": []string{to}, "subject": subject, "text": body})
	if err != nil {
		return err
	}
	key, err := idempotencyKey()
	if err != nil {
		return err
	}
	wait := s.Backoff
	if wait == 0 {
		wait = time.Second
	}
	for attempt := 1; ; attempt++ {
		retry, after, err := s.post(ctx, payload, key)
		if err == nil || !retry || attempt == resendAttempts {
			return err
		}
		pause := wait
		if after > 0 {
			pause = after
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (gave up while retrying: %v)", err, ctx.Err())
		case <-time.After(min(pause, resendMaxWait)):
		}
		wait *= 2
	}
}

// post makes one attempt. It reports whether a failure is worth retrying, and how long the
// server asked us to wait. Errors leave out the recipient, because they end up in the logs.
func (s *Resend) post(ctx context.Context, payload []byte, key string) (retry bool, after time.Duration, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Base+"/emails", bytes.NewReader(payload))
	if err != nil {
		return false, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+s.Key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	req.Header.Set("User-Agent", "wearwhat-be")
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return false, 0, err
		}
		return true, 0, fmt.Errorf("resend: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode < 300 {
		_, _ = io.Copy(io.Discard, res.Body)
		return false, 0, nil
	}
	var problem struct {
		Name    string `json:"name"`
		Message string `json:"message"`
	}
	_ = json.NewDecoder(io.LimitReader(res.Body, 1<<16)).Decode(&problem)
	err = fmt.Errorf("resend: status %d: %s: %s", res.StatusCode, problem.Name, problem.Message)
	if res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500 {
		seconds, _ := strconv.Atoi(res.Header.Get("Retry-After"))
		return true, time.Duration(seconds) * time.Second, err
	}
	return false, 0, err
}

func idempotencyKey() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

/* ---------- Termii SMS ---------- */

type Termii struct {
	Key, Sender, Base string
	Client            *http.Client
}

// Send implements SMS through Termii's messaging API.
func (t *Termii) Send(ctx context.Context, to, body string) error {
	payload, _ := json.Marshal(map[string]string{
		"api_key": t.Key,
		"to":      strings.TrimPrefix(to, "+"),
		"from":    t.Sender,
		"sms":     body,
		"type":    "plain",
		"channel": "dnd",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.Base+"/api/sms/send", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := t.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return fmt.Errorf("termii: status %d", res.StatusCode)
	}
	return nil
}

// NoSMS is used when no SMS provider is configured; phone sign-up is hidden in that case.
type NoSMS struct{}

func (NoSMS) Send(context.Context, string, string) error {
	return fmt.Errorf("sms is not configured")
}
