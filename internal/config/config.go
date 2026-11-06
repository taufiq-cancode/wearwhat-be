// Package config loads settings from the environment and refuses unsafe combinations.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Env         string
	Port        string
	Origins     []string
	DatabaseURL string
	Location    *time.Location
	StaticDir   string
	UploadDir   string
	// TrustedProxyHops is how many proxies sit in front of the server. 0 ignores X-Forwarded-For.
	TrustedProxyHops int
	SeedDemo         bool
	SupportEmail     string
	DataKey          []byte

	ResendKey  string
	ResendBase string
	MailFrom   string

	CloudinaryCloud  string
	CloudinaryKey    string
	CloudinarySecret string
	CloudinaryBase   string

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
func (c *Config) MailEnabled() bool  { return c.ResendKey != "" }

// CloudinaryEnabled is true when uploads go to Cloudinary rather than local disk.
func (c *Config) CloudinaryEnabled() bool { return c.CloudinaryCloud != "" }

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
		SeedDemo:     env("SEED_DEMO", "false") == "true",
		SupportEmail: env("SUPPORT_EMAIL", "support@wearwhat.site"),

		ResendKey:  env("RESEND_API_KEY", ""),
		ResendBase: strings.TrimRight(env("RESEND_BASE_URL", "https://api.resend.com"), "/"),
		MailFrom:   env("MAIL_FROM", ""),

		CloudinaryBase: strings.TrimRight(env("CLOUDINARY_BASE_URL", "https://api.cloudinary.com"), "/"),

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
	if err := checkCurrency(c.PaymentCurrency, c.FXRate); err != nil {
		return nil, err
	}

	hops, err := strconv.Atoi(env("TRUSTED_PROXY_HOPS", "0"))
	if err != nil || hops < 0 {
		return nil, errors.New("TRUSTED_PROXY_HOPS must be 0 or more")
	}
	c.TrustedProxyHops = hops

	if k := env("DATA_KEY", ""); k != "" {
		key, err := decodeKey(k)
		if err != nil {
			return nil, err
		}
		c.DataKey = key
	}

	if err := c.parseCloudinary(env("CLOUDINARY_URL", "")); err != nil {
		return nil, err
	}

	return c, c.validate()
}

// checkCurrency stops a charge going out at the wrong size. Prices are stored in USD, so a
// $120 order in NGN at the default rate of 1 would otherwise charge ₦120.
func checkCurrency(currency string, rate float64) error {
	if currency == "USD" && rate != 1 {
		return errors.New("FX_RATE must be 1 when PAYMENT_CURRENCY is USD: prices are already in USD")
	}
	if currency != "USD" && rate == 1 {
		return fmt.Errorf("PAYMENT_CURRENCY=%s needs FX_RATE: the number of %s per 1 USD", currency, currency)
	}
	return nil
}

// decodeKey accepts standard or URL-safe base64, so a generated value can't fail on encoding alone.
func decodeKey(k string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(k)
	if err != nil {
		key, err = base64.URLEncoding.DecodeString(k)
	}
	if err != nil || len(key) != 32 {
		return nil, errors.New("DATA_KEY must be base64 of exactly 32 bytes (openssl rand -base64 32)")
	}
	return key, nil
}

// parseCloudinary reads cloudinary://API_KEY:API_SECRET@CLOUD_NAME. Its error never repeats the
// value, because that would print the secret in the logs.
func (c *Config) parseCloudinary(raw string) error {
	if raw == "" {
		return nil
	}
	bad := errors.New("CLOUDINARY_URL must look like cloudinary://API_KEY:API_SECRET@CLOUD_NAME")
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "cloudinary" || u.Hostname() == "" || u.User == nil {
		return bad
	}
	secret, ok := u.User.Password()
	if !ok || secret == "" || u.User.Username() == "" {
		return bad
	}
	c.CloudinaryCloud = u.Hostname()
	c.CloudinaryKey = u.User.Username()
	c.CloudinarySecret = secret
	return nil
}

// validate enforces that development conveniences can never run in production.
func (c *Config) validate() error {
	if c.DatabaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	if c.MailFrom != "" {
		if _, err := mail.ParseAddress(c.MailFrom); err != nil {
			return errors.New("MAIL_FROM must look like: wearwhat <hello@yourdomain.com>")
		}
	} else if c.ResendKey != "" {
		return errors.New("RESEND_API_KEY needs MAIL_FROM: an address on a domain verified in Resend")
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
	if c.ResendKey == "" {
		missing = append(missing, "RESEND_API_KEY (verification codes must be deliverable)")
	}
	if c.MailFrom == "" {
		missing = append(missing, "MAIL_FROM")
	}
	if !c.CloudinaryEnabled() {
		missing = append(missing, "CLOUDINARY_URL (a host's local disk can be wiped on every deploy)")
	}
	if c.SeedDemo {
		return errors.New("SEED_DEMO cannot be used in production")
	}
	if len(missing) > 0 {
		return fmt.Errorf("production mode needs: %s", strings.Join(missing, ", "))
	}
	return nil
}
