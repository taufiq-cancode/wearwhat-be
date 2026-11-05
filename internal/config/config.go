// Package config loads settings from the environment and refuses unsafe combinations.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Env          string
	Port         string
	Origins      []string
	DatabaseURL  string
	Location     *time.Location
	StaticDir    string
	UploadDir    string
	TrustProxy   bool
	SeedDemo     bool
	SupportEmail string
	DataKey      []byte

	SMTPHost string
	SMTPPort string
	SMTPUser string
	SMTPPass string
	SMTPFrom string

	TermiiKey    string
	TermiiSender string
	TermiiBase   string

	AppleClientID string
	AppleKeysURL  string

	PaystackSecret  string
	PaystackBase    string
	PaymentCurrency string
	FXRate          float64
}

// IsDev is true only when APP_ENV is exactly "development"; anything else is treated as production.
func (c *Config) IsDev() bool { return c.Env == "development" }

// Origin is the app's primary browser origin, used for links in emails and payment callbacks.
func (c *Config) Origin() string {
	if len(c.Origins) == 0 {
		return ""
	}
	return c.Origins[0]
}

func (c *Config) TestPayments() bool { return c.PaystackSecret == "" }
func (c *Config) SMSEnabled() bool   { return c.TermiiKey != "" }
func (c *Config) MailEnabled() bool  { return c.SMTPHost != "" }

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// Load reads the environment. The returned error explains which setting to fix.
func Load() (*Config, error) {
	c := &Config{
		Env:          env("APP_ENV", "production"),
		Port:         env("PORT", "8080"),
		DatabaseURL:  env("DATABASE_URL", ""),
		StaticDir:    env("STATIC_DIR", ""),
		UploadDir:    env("UPLOAD_DIR", "./data/uploads"),
		TrustProxy:   env("TRUST_PROXY", "false") == "true",
		SeedDemo:     env("SEED_DEMO", "false") == "true",
		SupportEmail: env("SUPPORT_EMAIL", "support@wearwhat.com"),

		SMTPHost: env("SMTP_HOST", ""),
		SMTPPort: env("SMTP_PORT", "587"),
		SMTPUser: env("SMTP_USER", ""),
		SMTPPass: env("SMTP_PASS", ""),
		SMTPFrom: env("SMTP_FROM", "wearwhat <hello@wearwhat.com>"),

		TermiiKey:    env("TERMII_API_KEY", ""),
		TermiiSender: env("TERMII_SENDER", "wearwhat"),
		TermiiBase:   strings.TrimRight(env("TERMII_BASE_URL", "https://api.ng.termii.com"), "/"),

		AppleClientID: env("APPLE_CLIENT_ID", ""),
		AppleKeysURL:  env("APPLE_KEYS_URL", "https://appleid.apple.com/auth/keys"),

		PaystackSecret:  env("PAYSTACK_SECRET_KEY", ""),
		PaystackBase:    strings.TrimRight(env("PAYSTACK_BASE_URL", "https://api.paystack.co"), "/"),
		PaymentCurrency: strings.ToUpper(env("PAYMENT_CURRENCY", "USD")),
	}

	for _, o := range strings.Split(env("APP_ORIGIN", ""), ",") {
		if o = strings.TrimRight(strings.TrimSpace(o), "/"); o != "" {
			c.Origins = append(c.Origins, o)
		}
	}

	loc, err := time.LoadLocation(env("APP_TZ", "Africa/Lagos"))
	if err != nil {
		// Windows machines without tzdata: Lagos is a fixed UTC+1 with no daylight saving.
		loc = time.FixedZone("WAT", 3600)
	}
	c.Location = loc

	fx, err := strconv.ParseFloat(env("FX_RATE", "1"), 64)
	if err != nil || fx <= 0 {
		return nil, errors.New("FX_RATE must be a positive number")
	}
	c.FXRate = fx

	if k := env("DATA_KEY", ""); k != "" {
		key, err := base64.StdEncoding.DecodeString(k)
		if err != nil || len(key) != 32 {
			return nil, errors.New("DATA_KEY must be base64 of exactly 32 bytes (openssl rand -base64 32)")
		}
		c.DataKey = key
	}

	return c, c.validate()
}

// validate enforces that development conveniences can never run in production.
func (c *Config) validate() error {
	if c.DatabaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	if c.IsDev() {
		if len(c.DataKey) == 0 {
			// stable and obviously not secret: development databases hold only demo data
			c.DataKey = []byte("wearwhat-development-only-key-32")
		}
		return nil
	}
	var missing []string
	if len(c.DataKey) == 0 {
		missing = append(missing, "DATA_KEY")
	}
	if len(c.Origins) == 0 {
		missing = append(missing, "APP_ORIGIN")
	}
	if c.PaystackSecret == "" {
		missing = append(missing, "PAYSTACK_SECRET_KEY (test payments are not allowed in production)")
	}
	if c.SMTPHost == "" {
		missing = append(missing, "SMTP_HOST (verification codes must be deliverable)")
	}
	if c.SeedDemo {
		return errors.New("SEED_DEMO cannot be used in production")
	}
	if len(missing) > 0 {
		return fmt.Errorf("production mode needs: %s", strings.Join(missing, ", "))
	}
	return nil
}
