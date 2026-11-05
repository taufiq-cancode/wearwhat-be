// Package payments talks to the payment provider. Card details never touch this server:
// customers pay on the provider's hosted page, and we keep only a reusable authorisation.
package payments

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Card is what a provider reports about a card it can charge again.
type Card struct {
	AuthCode    string
	Last4       string
	Brand       string
	Exp         string
	Fingerprint string
	Reusable    bool
}

type InitRequest struct {
	Reference   string
	Email       string
	Amount      int64 // minor units
	Currency    string
	Method      string // apple_pay | card | bank_transfer
	CallbackURL string
	SavedAuth   string // charge this saved card instead of redirecting
}

// Result is the state of a charge. Paid is only true when the provider confirms it.
type Result struct {
	Paid        bool
	Failed      bool
	RedirectURL string
	Amount      int64
	Currency    string
	Card        *Card
}

// Provider is implemented by each payment service.
type Provider interface {
	Name() string
	Test() bool
	// Methods lists what checkout can offer.
	Methods() []string
	Init(ctx context.Context, req InitRequest) (Result, error)
	Verify(ctx context.Context, reference string) (Result, error)
	// ParseWebhook authenticates a webhook body and returns the paid reference, if any.
	ParseWebhook(body []byte, signature string) (reference string, err error)
}

var ErrBadSignature = errors.New("bad webhook signature")

/* ---------- test provider (development only) ---------- */

// Testing confirms every charge immediately. No money moves, and the app labels it as test mode.
type Testing struct{}

func (Testing) Name() string      { return "test" }
func (Testing) Test() bool        { return true }
func (Testing) Methods() []string { return []string{"apple_pay", "card", "bank_transfer"} }

func (Testing) Init(_ context.Context, req InitRequest) (Result, error) {
	r := Result{Paid: true, Amount: req.Amount, Currency: req.Currency}
	if req.Method == "card" && req.SavedAuth == "" {
		r.Card = &Card{AuthCode: "test_auth", Last4: "4242", Brand: "Test card", Exp: "12/30", Fingerprint: "test-4242", Reusable: true}
	}
	return r, nil
}

func (Testing) Verify(context.Context, string) (Result, error) {
	return Result{}, errors.New("test provider has no pending charges")
}

func (Testing) ParseWebhook([]byte, string) (string, error) { return "", ErrBadSignature }

/* ---------- Paystack ---------- */

// Paystack implements the hosted-checkout flow, saved cards and signed webhooks.
type Paystack struct {
	Secret string
	Base   string
	Client *http.Client
}

func (p *Paystack) Name() string      { return "paystack" }
func (p *Paystack) Test() bool        { return false }
func (p *Paystack) Methods() []string { return []string{"apple_pay", "card", "bank_transfer"} }

func (p *Paystack) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.Base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.Secret)
	req.Header.Set("Content-Type", "application/json")
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return err
	}
	if res.StatusCode >= 300 {
		return fmt.Errorf("paystack %s: status %d", path, res.StatusCode)
	}
	return json.Unmarshal(raw, out)
}

type paystackTx struct {
	Status        string `json:"status"`
	Amount        int64  `json:"amount"`
	Currency      string `json:"currency"`
	Authorization struct {
		AuthorizationCode string `json:"authorization_code"`
		Last4             string `json:"last4"`
		ExpMonth          string `json:"exp_month"`
		ExpYear           string `json:"exp_year"`
		Brand             string `json:"brand"`
		Signature         string `json:"signature"`
		Reusable          bool   `json:"reusable"`
		Channel           string `json:"channel"`
	} `json:"authorization"`
}

func (t paystackTx) result() Result {
	// "abandoned" only means the customer hasn't finished yet, so it isn't a failure;
	// checkouts left that way are released when they expire.
	r := Result{Paid: t.Status == "success", Failed: t.Status == "failed", Amount: t.Amount, Currency: t.Currency}
	a := t.Authorization
	if a.Reusable && a.AuthorizationCode != "" && a.Channel == "card" {
		year := a.ExpYear
		if len(year) == 4 {
			year = year[2:]
		}
		r.Card = &Card{AuthCode: a.AuthorizationCode, Last4: a.Last4, Brand: a.Brand, Exp: a.ExpMonth + "/" + year, Fingerprint: a.Signature, Reusable: true}
	}
	return r
}

// Init starts a hosted checkout, or charges a saved card straight away.
func (p *Paystack) Init(ctx context.Context, req InitRequest) (Result, error) {
	if req.SavedAuth != "" {
		var out struct {
			Data paystackTx `json:"data"`
		}
		err := p.do(ctx, http.MethodPost, "/transaction/charge_authorization", map[string]any{
			"authorization_code": req.SavedAuth,
			"email":              req.Email,
			"amount":             req.Amount,
			"currency":           req.Currency,
			"reference":          req.Reference,
		}, &out)
		if err != nil {
			return Result{}, err
		}
		return out.Data.result(), nil
	}
	body := map[string]any{
		"email":        req.Email,
		"amount":       req.Amount,
		"currency":     req.Currency,
		"reference":    req.Reference,
		"callback_url": req.CallbackURL,
	}
	if ch := channel(req.Method); ch != "" {
		body["channels"] = []string{ch}
	}
	var out struct {
		Data struct {
			AuthorizationURL string `json:"authorization_url"`
		} `json:"data"`
	}
	if err := p.do(ctx, http.MethodPost, "/transaction/initialize", body, &out); err != nil {
		return Result{}, err
	}
	if out.Data.AuthorizationURL == "" {
		return Result{}, errors.New("paystack: no authorization url")
	}
	return Result{RedirectURL: out.Data.AuthorizationURL}, nil
}

func channel(method string) string {
	switch method {
	case "card":
		return "card"
	case "bank_transfer":
		return "bank_transfer"
	case "apple_pay":
		return "apple_pay"
	}
	return ""
}

// Verify asks Paystack for the charge's real state; a callback URL alone proves nothing.
func (p *Paystack) Verify(ctx context.Context, reference string) (Result, error) {
	var out struct {
		Data paystackTx `json:"data"`
	}
	if err := p.do(ctx, http.MethodGet, "/transaction/verify/"+url.PathEscape(reference), nil, &out); err != nil {
		return Result{}, err
	}
	return out.Data.result(), nil
}

// ParseWebhook checks the HMAC-SHA512 signature and extracts the reference of a successful charge.
func (p *Paystack) ParseWebhook(body []byte, signature string) (string, error) {
	mac := hmac.New(sha512.New, []byte(p.Secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(signature)) {
		return "", ErrBadSignature
	}
	var ev struct {
		Event string `json:"event"`
		Data  struct {
			Reference string `json:"reference"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		return "", err
	}
	if ev.Event != "charge.success" {
		return "", nil
	}
	return ev.Data.Reference, nil
}
