// Package notify sends email and SMS. Each has a real provider and a console fallback for development.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
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

/* ---------- SMTP ---------- */

type SMTP struct {
	Host, Port, User, Pass, From string
}

// Send implements Mailer over SMTP with STARTTLS where the server offers it.
func (s *SMTP) Send(_ context.Context, to, subject, body string) error {
	from, err := mail.ParseAddress(s.From)
	if err != nil {
		return fmt.Errorf("SMTP_FROM: %w", err)
	}
	if strings.ContainsAny(to, "\r\n") || strings.ContainsAny(subject, "\r\n") {
		return fmt.Errorf("refusing header injection in email to %q", to)
	}
	var msg bytes.Buffer
	fmt.Fprintf(&msg, "From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s\r\n",
		s.From, to, mime.QEncoding.Encode("utf-8", subject), body)
	var auth smtp.Auth
	if s.User != "" {
		auth = smtp.PlainAuth("", s.User, s.Pass, s.Host)
	}
	return smtp.SendMail(net.JoinHostPort(s.Host, s.Port), auth, from.Address, []string{to}, msg.Bytes())
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
