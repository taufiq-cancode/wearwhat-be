// Package studio is the brand side: onboarding, settings and team.
// Every query is scoped to the caller's own brand.
package studio

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"wearwhat-be/internal/app"
	"wearwhat-be/internal/commerce"
	"wearwhat-be/internal/db"
	"wearwhat-be/internal/domain"
	"wearwhat-be/internal/httpx"
	"wearwhat-be/internal/inbox"
	"wearwhat-be/internal/media"
	"wearwhat-be/internal/secure"
)

type Service struct {
	*app.Deps
	Commerce *commerce.Service
}

func (s *Service) Routes(r *httpx.Router) {
	r.User("POST /studio/onboarding", s.onboard)

	r.Brand("GET /studio/metrics", httpx.CapAny, s.metrics)

	r.Brand("GET /studio/orders", httpx.CapOrders, s.listOrders)
	r.Brand("GET /studio/orders/{id}", httpx.CapOrders, s.getOrder)
	r.Brand("PATCH /studio/orders/{id}", httpx.CapOrders, s.patchOrder)

	r.Brand("GET /studio/catalog", httpx.CapContent, s.catalog)
	r.Brand("PATCH /studio/products/{id}/stock", httpx.CapContent, s.setStock)
	r.Brand("GET /studio/products/{id}", httpx.CapContent, s.getProduct)
	r.Brand("POST /studio/products", httpx.CapContent, s.createProduct)
	r.Brand("PATCH /studio/products/{id}", httpx.CapContent, s.updateProduct)

	r.Brand("GET /studio/promotions", httpx.CapContent, s.promotions)
	r.Brand("POST /studio/promotions", httpx.CapContent, s.createPromotion)
	r.Brand("PATCH /studio/promotions/{code}", httpx.CapContent, s.patchPromotion)
	r.Brand("GET /studio/drops/next", httpx.CapContent, s.getNextDrop)
	r.Brand("PUT /studio/drops/next", httpx.CapContent, s.putNextDrop)

	r.Brand("GET /studio/media", httpx.CapContent, s.listMedia)
	r.Brand("POST /studio/media", httpx.CapContent, s.addMedia)
	r.Brand("POST /studio/posts", httpx.CapContent, s.createPost)

	r.Brand("GET /studio/settings", httpx.CapAny, s.getSettings)
	r.Brand("PUT /studio/settings/store", httpx.CapSettings, s.putStore)
	r.Brand("PUT /studio/settings/payouts", httpx.CapSettings, s.putPayouts)
	r.Brand("PUT /studio/settings/shipping", httpx.CapSettings, s.putShipping)
	r.Brand("POST /studio/team/invites", httpx.CapSettings, s.invite)
	r.Brand("DELETE /studio/team/{id}", httpx.CapSettings, s.removeMember)

	r.Public("GET /team/invites/{token}", s.inviteInfo)
	r.User("POST /team/invites/accept", s.acceptInvite)
}

/* ---------- stored settings ---------- */

// payoutsRow is how payout details are stored: the account number stays encrypted.
type payoutsRow struct {
	Provider   string `json:"provider"`
	Bank       string `json:"bank"`
	AccountEnc string `json:"accountEnc"`
	Schedule   string `json:"schedule"`
	Currency   string `json:"currency"`
	LegalName  string `json:"legalName"`
}

// businessRow is the verification file. The registration number (which may be a NIN or BVN) is encrypted.
type businessRow struct {
	Type            string `json:"type"`
	LegalName       string `json:"legalName"`
	RegistrationEnc string `json:"registrationEnc"`
	DocumentID      string `json:"documentId"`
}

var (
	accountRe  = regexp.MustCompile(`^\d{10}$`)
	providers  = map[string]bool{"Paystack": true, "Flutterwave": true, "Stripe": true}
	schedules  = map[string]bool{"Daily": true, "Weekly": true, "Monthly": true}
	currencies = map[string]bool{"NGN": true, "GHS": true, "USD": true}
)

func validatePayouts(provider, bank, account, currency string) error {
	switch {
	case !providers[provider]:
		return httpx.BadRequest("Choose a payout provider")
	case strings.TrimSpace(bank) == "":
		return httpx.BadRequest("Add your bank")
	case !accountRe.MatchString(account):
		return httpx.BadRequest("Enter a 10-digit account number")
	case !currencies[currency]:
		return httpx.BadRequest("Choose a payout currency")
	}
	return nil
}

func validateShipping(in domain.ShippingSettings) error {
	if !in.Zones.Lagos && !in.Zones.Nigeria && !in.Zones.Africa && !in.Zones.International {
		return httpx.BadRequest("Pick at least one delivery zone")
	}
	return nil
}

func brandMeta(categories []string, location string) string {
	parts := []string{}
	if len(categories) > 0 {
		parts = append(parts, strings.Join(categories, " & "))
	}
	if location != "" {
		parts = append(parts, location)
	}
	return strings.Join(parts, " · ")
}

func validateStoreBasics(name, handle, bio string) error {
	switch {
	case strings.TrimSpace(name) == "" || len([]rune(name)) > 60:
		return httpx.BadRequest("Your store needs a name")
	case !domain.ValidHandle(handle):
		return httpx.BadRequest("Handles use 3–30 letters, numbers, dots or underscores")
	case len([]rune(bio)) > 300:
		return httpx.BadRequest("Keep the bio under 300 characters")
	}
	return nil
}

/* ---------- onboarding ---------- */

type onboardingInput struct {
	Store struct {
		Name       string   `json:"name"`
		Handle     string   `json:"handle"`
		Categories []string `json:"categories"`
		Location   string   `json:"location"`
		Bio        string   `json:"bio"`
		LogoURL    *string  `json:"logoUrl"`
	} `json:"store"`
	Business struct {
		Type         string `json:"type"`
		LegalName    string `json:"legalName"`
		Registration string `json:"registration"`
		DocumentID   string `json:"documentId"`
	} `json:"business"`
	Payouts struct {
		Provider string `json:"provider"`
		Bank     string `json:"bank"`
		Account  string `json:"account"`
		Currency string `json:"currency"`
	} `json:"payouts"`
	Shipping     domain.ShippingSettings `json:"shipping"`
	FirstProduct *domain.ProductInput    `json:"firstProduct"`
}

func (s *Service) onboard(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	if who.BrandID != "" {
		return nil, httpx.Conflict("You already run a brand")
	}
	var in onboardingInput
	if err := httpx.Decode(r, &in); err != nil {
		return nil, err
	}
	handle := domain.NormalizeHandle(in.Store.Handle)
	if err := validateStoreBasics(in.Store.Name, handle, in.Store.Bio); err != nil {
		return nil, err
	}
	if err := validatePayouts(in.Payouts.Provider, in.Payouts.Bank, in.Payouts.Account, in.Payouts.Currency); err != nil {
		return nil, err
	}
	if err := validateShipping(in.Shipping); err != nil {
		return nil, err
	}
	if in.Business.DocumentID == "" {
		return nil, httpx.BadRequest("Upload a document")
	}
	if err := media.OwnDocument(ctx, s.DB, who.UserID, in.Business.DocumentID); err != nil {
		return nil, err
	}
	registration, err := secure.Encrypt(s.Cfg.DataKey, strings.TrimSpace(in.Business.Registration))
	if err != nil {
		return nil, err
	}
	account, err := secure.Encrypt(s.Cfg.DataKey, in.Payouts.Account)
	if err != nil {
		return nil, err
	}
	logo, err := media.OptionalPhoto(ctx, s.DB, who, in.Store.LogoURL)
	if err != nil {
		return nil, err
	}
	categories := in.Store.Categories
	if categories == nil {
		categories = []string{}
	}
	status := "in_review"
	var verifiedAt *time.Time
	if s.Cfg.IsDev() {
		// no review queue in development, so new brands are immediately visible
		status = "verified"
		now := s.Now()
		verifiedAt = &now
	}
	brandID := secure.ID("br")
	name := strings.TrimSpace(in.Store.Name)
	meta := brandMeta(categories, strings.TrimSpace(in.Store.Location))
	err = db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		if in.Shipping.Processing == "" {
			in.Shipping.Processing = "48h"
		}
		if in.Shipping.Returns == "" {
			in.Shipping.Returns = "14 days"
		}
		_, err := tx.Exec(ctx, `INSERT INTO brands (id, owner_id, handle, name, initials, status, verified_at, avatar, meta, description, bio, location, categories, payouts, shipping, business)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$9,$10,$11,$12,$13,$14,$15)`,
			brandID, who.UserID, handle, name, domain.Initials(name), status, verifiedAt, logo, meta, strings.TrimSpace(in.Store.Bio),
			strings.TrimSpace(in.Store.Location), categories,
			payoutsRow{Provider: in.Payouts.Provider, Bank: strings.TrimSpace(in.Payouts.Bank), AccountEnc: account, Schedule: "Weekly", Currency: in.Payouts.Currency, LegalName: strings.TrimSpace(in.Business.LegalName)},
			in.Shipping,
			businessRow{Type: in.Business.Type, LegalName: strings.TrimSpace(in.Business.LegalName), RegistrationEnc: registration, DocumentID: in.Business.DocumentID})
		if db.IsUnique(err) {
			return httpx.Conflict("@" + handle + " is taken")
		}
		if err != nil {
			return err
		}
		var email *string
		var userName string
		if err := tx.QueryRow(ctx, `SELECT email, name FROM users WHERE id = $1`, who.UserID).Scan(&email, &userName); err != nil {
			return err
		}
		emailStr := ""
		if email != nil {
			emailStr = *email
		}
		if _, err := tx.Exec(ctx, `INSERT INTO team_members (id, brand_id, user_id, email, name, role, status) VALUES ($1,$2,$3,$4,$5,'OWNER','active')`,
			secure.ID("tm"), brandID, who.UserID, emailStr, userName); err != nil {
			return err
		}
		if in.FirstProduct != nil {
			owner := &httpx.Principal{UserID: who.UserID, BrandID: brandID, BrandRole: "OWNER"}
			if _, err := s.insertProduct(ctx, tx, owner, *in.FirstProduct); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return httpx.OK(), nil
}

/* ---------- settings ---------- */

func (s *Service) settings(ctx context.Context, who *httpx.Principal) (domain.StudioSettings, error) {
	var out domain.StudioSettings
	var status string
	var verifiedAt *time.Time
	var avatar *domain.Photo
	var payouts payoutsRow
	err := s.DB.QueryRow(ctx, `SELECT name, handle, categories, location, bio, avatar, show_worn_by, accept_messages, status, verified_at, payouts, shipping
		FROM brands WHERE id = $1`, who.BrandID).Scan(
		&out.Store.Name, &out.Store.Handle, &out.Store.Categories, &out.Store.Location, &out.Store.Bio, &avatar,
		&out.Store.ShowWornBy, &out.Store.AcceptMessages, &status, &verifiedAt, &payouts, &out.Shipping)
	if err != nil {
		return out, err
	}
	if out.Store.Categories == nil {
		out.Store.Categories = []string{}
	}
	if avatar != nil {
		out.Store.LogoURL = &avatar.URL
	}
	out.Verified = status == "verified"
	if verifiedAt != nil {
		out.VerifiedAt = domain.LongDate(*verifiedAt)
	}
	out.Payouts = domain.PayoutSettings{Provider: payouts.Provider, Bank: payouts.Bank, Schedule: payouts.Schedule, Currency: payouts.Currency}
	// the account number is only decrypted for people allowed to manage payouts
	if who.Can(httpx.CapSettings) {
		account, err := secure.Decrypt(s.Cfg.DataKey, payouts.AccountEnc)
		if err != nil {
			return out, err
		}
		out.Payouts.Account = account
		if len(account) == 10 && payouts.LegalName != "" {
			name := strings.ToUpper(payouts.LegalName)
			out.Payouts.AccountName = &name
		}
	}
	rows, err := s.DB.Query(ctx, `SELECT t.id, COALESCE(NULLIF(u.name, ''), NULLIF(t.name, ''), t.email), t.email, t.role, t.status
		FROM team_members t LEFT JOIN users u ON u.id = t.user_id WHERE t.brand_id = $1
		ORDER BY t.role = 'OWNER' DESC, t.status = 'active' DESC, t.created_at`, who.BrandID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	out.Team = []domain.TeamMember{}
	for rows.Next() {
		var m domain.TeamMember
		var email, role, state string
		if err := rows.Scan(&m.ID, &m.Name, &email, &role, &state); err != nil {
			return out, err
		}
		switch {
		case role == "OWNER":
			m.Role, m.Detail = "OWNER", strings.TrimSuffix("Owner · "+email, " · ")
		case state == "invited":
			m.Role, m.Detail = "STAFF", "Invited · "+role
		default:
			m.Role, m.Detail = "STAFF", roleDetail(role)
		}
		out.Team = append(out.Team, m)
	}
	return out, rows.Err()
}

func roleDetail(role string) string {
	switch role {
	case "Orders":
		return "Orders & fulfilment"
	case "Content":
		return "Content & posts"
	}
	return "Admin"
}

func (s *Service) getSettings(_ http.ResponseWriter, r *http.Request) (any, error) {
	return s.settings(r.Context(), httpx.Who(r))
}

func (s *Service) putStore(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	var in domain.StoreSettings
	if err := httpx.Decode(r, &in); err != nil {
		return nil, err
	}
	handle := domain.NormalizeHandle(in.Handle)
	if err := validateStoreBasics(in.Name, handle, in.Bio); err != nil {
		return nil, err
	}
	logo, err := media.OptionalPhoto(ctx, s.DB, who, in.LogoURL)
	if err != nil {
		return nil, err
	}
	if in.Categories == nil {
		in.Categories = []string{}
	}
	name := strings.TrimSpace(in.Name)
	location := strings.TrimSpace(in.Location)
	meta := brandMeta(in.Categories, location)
	_, err = s.DB.Exec(ctx, `UPDATE brands SET name = $2, handle = $3, initials = $4, categories = $5, location = $6, bio = $7, avatar = $8,
		show_worn_by = $9, accept_messages = $10, meta = $11, description = $11 WHERE id = $1`,
		who.BrandID, name, handle, domain.Initials(name), in.Categories, location, strings.TrimSpace(in.Bio), logo, in.ShowWornBy, in.AcceptMessages, meta)
	if db.IsUnique(err) {
		return nil, httpx.Conflict("@" + handle + " is taken")
	}
	if err != nil {
		return nil, err
	}
	return s.settings(ctx, who)
}

func (s *Service) putPayouts(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	var in domain.PayoutSettings
	if err := httpx.Decode(r, &in); err != nil {
		return nil, err
	}
	if err := validatePayouts(in.Provider, in.Bank, in.Account, in.Currency); err != nil {
		return nil, err
	}
	if !schedules[in.Schedule] {
		return nil, httpx.BadRequest("Choose a payout schedule")
	}
	enc, err := secure.Encrypt(s.Cfg.DataKey, in.Account)
	if err != nil {
		return nil, err
	}
	if _, err := s.DB.Exec(ctx, `UPDATE brands SET payouts = payouts || $2 WHERE id = $1`, who.BrandID, map[string]string{
		"provider": in.Provider, "bank": strings.TrimSpace(in.Bank), "accountEnc": enc, "schedule": in.Schedule, "currency": in.Currency,
	}); err != nil {
		return nil, err
	}
	return s.settings(ctx, who)
}

func (s *Service) putShipping(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	var in domain.ShippingSettings
	if err := httpx.Decode(r, &in); err != nil {
		return nil, err
	}
	if err := validateShipping(in); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	if _, err := s.DB.Exec(ctx, `UPDATE brands SET shipping = $2 WHERE id = $1`, who.BrandID, raw); err != nil {
		return nil, err
	}
	return s.settings(ctx, who)
}

/* ---------- team ---------- */

var inviteRoles = map[string]bool{"Orders": true, "Content": true, "Admin": true}

const inviteTTL = 14 * 24 * time.Hour

func (s *Service) invite(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	var in struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return nil, err
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if !domain.ValidEmail(email) {
		return nil, httpx.BadRequest("Enter a valid email")
	}
	if !inviteRoles[in.Role] {
		return nil, httpx.BadRequest("Choose a role")
	}
	if err := httpx.Limit(ctx, s.DB, "invite:"+who.BrandID, 30, 24*time.Hour, "Too many invites today — try again tomorrow"); err != nil {
		return nil, err
	}
	var active bool
	if err := s.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM team_members WHERE brand_id = $1 AND email = $2 AND status = 'active')`, who.BrandID, email).Scan(&active); err != nil {
		return nil, err
	}
	if active {
		return nil, httpx.Conflict(email + " is already on your team")
	}
	token := secure.Token()
	_, err := s.DB.Exec(ctx, `INSERT INTO team_members (id, brand_id, email, role, status, invite_token_hash, invite_expires_at)
		VALUES ($1,$2,$3,$4,'invited',$5,$6)
		ON CONFLICT (brand_id, email) DO UPDATE SET role = $4, invite_token_hash = $5, invite_expires_at = $6`,
		secure.ID("tm"), who.BrandID, email, in.Role, secure.Hash(token), s.Now().Add(inviteTTL))
	if err != nil {
		return nil, err
	}
	var brandName string
	if err := s.DB.QueryRow(ctx, `SELECT name FROM brands WHERE id = $1`, who.BrandID).Scan(&brandName); err != nil {
		return nil, err
	}
	s.SendMail(email, "Join "+brandName+" on wearwhat",
		"You’ve been invited to help run "+brandName+" on wearwhat ("+in.Role+").\n\nAccept the invite: "+s.Cfg.Origin()+"/invite/"+token+"\n\nThis link expires in 14 days.")
	return s.settings(ctx, who)
}

func (s *Service) removeMember(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	tag, err := s.DB.Exec(ctx, `DELETE FROM team_members WHERE id = $1 AND brand_id = $2 AND role <> 'OWNER'`, r.PathValue("id"), who.BrandID)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, httpx.NotFound("Team member not found")
	}
	return s.settings(ctx, who)
}

func (s *Service) inviteInfo(_ http.ResponseWriter, r *http.Request) (any, error) {
	var brandName, role, email string
	err := s.DB.QueryRow(r.Context(), `SELECT b.name, t.role, t.email FROM team_members t JOIN brands b ON b.id = t.brand_id
		WHERE t.invite_token_hash = $1 AND t.status = 'invited' AND t.invite_expires_at > $2`, secure.Hash(r.PathValue("token")), s.Now()).Scan(&brandName, &role, &email)
	if db.IsNotFound(err) {
		return nil, httpx.NotFound("This invite has expired or been used")
	}
	if err != nil {
		return nil, err
	}
	return map[string]string{"brandName": brandName, "role": role, "email": email}, nil
}

func (s *Service) acceptInvite(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	var in struct {
		Token string `json:"token"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return nil, err
	}
	if who.BrandID != "" {
		return nil, httpx.Conflict("You’re already on a brand’s team")
	}
	var userName string
	if err := s.DB.QueryRow(ctx, `SELECT name FROM users WHERE id = $1`, who.UserID).Scan(&userName); err != nil {
		return nil, err
	}
	var brandID string
	// the token is claimed atomically and cleared, so it can't be used twice
	err := s.DB.QueryRow(ctx, `UPDATE team_members SET user_id = $2, name = $3, status = 'active', invite_token_hash = NULL, invite_expires_at = NULL
		WHERE invite_token_hash = $1 AND status = 'invited' AND invite_expires_at > $4 RETURNING brand_id`,
		secure.Hash(in.Token), who.UserID, userName, s.Now()).Scan(&brandID)
	if db.IsNotFound(err) {
		return nil, httpx.NotFound("This invite has expired or been used")
	}
	if db.IsUnique(err) {
		return nil, httpx.Conflict("You’re already on a brand’s team")
	}
	if err != nil {
		return nil, err
	}
	return httpx.OK(), nil
}

/* ---------- notification action ---------- */

// AmplifyAction shows a shopper's wear on the brand's page and to its followers.
func (s *Service) AmplifyAction(ctx context.Context, who *httpx.Principal, a *domain.NotificationAction, done bool) (any, error) {
	if who.BrandID == "" {
		return nil, httpx.Forbidden("Set up your brand first")
	}
	var reach int
	var brandName string
	var avatar *domain.Photo
	err := db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT followers, name, avatar FROM brands WHERE id = $1`, who.BrandID).Scan(&reach, &brandName, &avatar); err != nil {
			return err
		}
		var userID *string
		var media []domain.Photo
		err := tx.QueryRow(ctx, `SELECT p.user_id, p.media FROM posts p WHERE p.id = $1`, a.Target).Scan(&userID, &media)
		if db.IsNotFound(err) {
			return httpx.NotFound("That wear has been removed")
		}
		if err != nil {
			return err
		}
		if !done {
			_, err := tx.Exec(ctx, `DELETE FROM post_amplifications WHERE post_id = $1 AND brand_id = $2`, a.Target, who.BrandID)
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO post_amplifications (post_id, brand_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, a.Target, who.BrandID)
		if err != nil || tag.RowsAffected() == 0 || userID == nil {
			return err
		}
		var thumb *domain.Photo
		if len(media) > 0 {
			thumb = &media[0]
		}
		return inbox.Notify(ctx, tx, inbox.Note{UserID: *userID, Who: brandName, Text: "featured your wear on their page.", Kind: "Social", Avatar: avatar, Thumb: thumb, Href: "/post/" + a.Target})
	})
	if err != nil {
		return nil, err
	}
	return map[string]int{"reach": reach}, nil
}
