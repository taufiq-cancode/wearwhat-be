package studio

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"wearwhat-be/internal/db"
	"wearwhat-be/internal/domain"
	"wearwhat-be/internal/httpx"
	"wearwhat-be/internal/inbox"
	"wearwhat-be/internal/media"
	"wearwhat-be/internal/secure"
	"wearwhat-be/internal/views"
)

/* ---------- promotions ---------- */

func (s *Service) promotionList(ctx context.Context, brandID string) ([]domain.Promotion, error) {
	rows, err := s.DB.Query(ctx, `SELECT code, type, value, scope, length, uses, live AND (ends_at IS NULL OR ends_at > $2)
		FROM promotions WHERE brand_id = $1 ORDER BY created_at DESC, code`, brandID, s.Now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Promotion{}
	for rows.Next() {
		var p domain.Promotion
		if err := rows.Scan(&p.Code, &p.Type, &p.Value, &p.Scope, &p.Length, &p.Uses, &p.Live); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// promoScopes are the real things a code can apply to: everything, a category the brand
// sells, or one of its recent or upcoming drops.
func (s *Service) promoScopes(ctx context.Context, brandID string) ([]string, error) {
	out := []string{"Everything"}
	rows, err := s.DB.Query(ctx, `SELECT DISTINCT category FROM products WHERE brand_id = $1 ORDER BY category`, brandID)
	if err != nil {
		return nil, err
	}
	categories, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out = append(out, categories...)
	rows, err = s.DB.Query(ctx, `SELECT title FROM drops WHERE brand_id = $1 AND (released_at IS NULL OR released_at > $2::timestamptz - interval '30 days')
		ORDER BY releases_at DESC`, brandID, s.Now())
	if err != nil {
		return nil, err
	}
	drops, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	return append(out, drops...), nil
}

func (s *Service) promotions(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	list, err := s.promotionList(ctx, who.BrandID)
	if err != nil {
		return nil, err
	}
	next, err := s.nextDrop(ctx, s.DB, who.BrandID)
	if err != nil {
		return nil, err
	}
	scopes, err := s.promoScopes(ctx, who.BrandID)
	if err != nil {
		return nil, err
	}
	return domain.PromotionsOverview{NextDrop: next, Promotions: list, Scopes: scopes}, nil
}

func (s *Service) createPromotion(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	var in struct {
		Code   string `json:"code"`
		Type   string `json:"type"`
		Value  string `json:"value"`
		Scope  string `json:"scope"`
		Length string `json:"length"`
		Banner bool   `json:"banner"`
		Notify bool   `json:"notify"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return nil, err
	}
	code := strings.ToUpper(strings.Join(strings.Fields(in.Code), ""))
	if !domain.ValidPromo(code) {
		return nil, httpx.BadRequest("Codes use 3–24 letters and numbers")
	}
	switch in.Type {
	case "% off":
		if d := domain.PromoDiscount(in.Type, in.Value, []int{10000}); d[0] <= 0 || d[0] > 10000 {
			return nil, httpx.BadRequest("Add the discount amount")
		}
		if v := parseAmount(in.Value); v > 100 {
			return nil, httpx.BadRequest("A percentage can’t be over 100")
		}
	case "$ off":
		if parseAmount(in.Value) <= 0 {
			return nil, httpx.BadRequest("Add the discount amount")
		}
	case "Free ship":
		in.Value = ""
	default:
		return nil, httpx.BadRequest("Choose a promotion type")
	}
	scopes, err := s.promoScopes(ctx, who.BrandID)
	if err != nil {
		return nil, err
	}
	if in.Scope == "" {
		in.Scope = "Everything"
	}
	validScope := false
	for _, sc := range scopes {
		validScope = validScope || sc == in.Scope
	}
	if !validScope {
		return nil, httpx.BadRequest("Choose what it applies to")
	}
	ends := domain.PromoEnds(in.Length, s.Now())
	if ends == nil {
		return nil, httpx.BadRequest("Choose how long it runs")
	}
	err = db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO promotions (code, brand_id, type, value, scope, length, ends_at, banner, notify, live, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,true,$10)`, code, who.BrandID, in.Type, in.Value, in.Scope, in.Length, ends, in.Banner, in.Notify, s.Now())
		if db.IsUnique(err) {
			return httpx.Conflict(code + " already exists")
		}
		if err != nil {
			return err
		}
		if !in.Notify {
			return nil
		}
		var name, handle string
		var avatar *domain.Photo
		if err := tx.QueryRow(ctx, `SELECT name, handle, avatar FROM brands WHERE id = $1`, who.BrandID).Scan(&name, &handle, &avatar); err != nil {
			return err
		}
		return inbox.NotifyFollowers(ctx, tx, who.BrandID, inbox.Note{
			Who: name, Text: domain.PromoLabel(in.Type, in.Value) + " with code " + code + ".", Kind: "Drops", Avatar: avatar, Href: "/b/" + handle, Pref: "drops",
		})
	})
	if err != nil {
		return nil, err
	}
	return domain.Promotion{Code: code, Type: in.Type, Value: in.Value, Scope: in.Scope, Length: in.Length, Live: true}, nil
}

func parseAmount(s string) float64 {
	var v float64
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%f", &v); err != nil {
		return 0
	}
	return v
}

func (s *Service) patchPromotion(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	var in struct {
		Live bool `json:"live"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return nil, err
	}
	var p domain.Promotion
	var endsAt *time.Time
	var length string
	err := s.DB.QueryRow(ctx, `SELECT code, type, value, scope, length, uses, ends_at FROM promotions WHERE code = $1 AND brand_id = $2`,
		r.PathValue("code"), who.BrandID).Scan(&p.Code, &p.Type, &p.Value, &p.Scope, &length, &p.Uses, &endsAt)
	if db.IsNotFound(err) {
		return nil, httpx.NotFound("Promotion not found")
	}
	if err != nil {
		return nil, err
	}
	p.Length = length
	// switching an ended code back on restarts it for its original length
	if in.Live && endsAt != nil && !endsAt.After(s.Now()) {
		endsAt = domain.PromoEnds(length, s.Now())
	}
	if _, err := s.DB.Exec(ctx, `UPDATE promotions SET live = $2, ends_at = $3 WHERE code = $1`, p.Code, in.Live, endsAt); err != nil {
		return nil, err
	}
	p.Live = in.Live
	return p, nil
}

/* ---------- media library ---------- */

func (s *Service) listMedia(_ http.ResponseWriter, r *http.Request) (any, error) {
	rows, err := s.DB.Query(r.Context(), `SELECT id, media FROM brand_media WHERE brand_id = $1 ORDER BY created_at DESC, id`, httpx.Who(r).BrandID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.GridTile{}
	for rows.Next() {
		var t domain.GridTile
		if err := rows.Scan(&t.ID, &t.Image); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Service) addMedia(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	var in struct {
		URL string `json:"url"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return nil, err
	}
	photo, err := media.Photo(ctx, s.DB, who, in.URL)
	if err != nil {
		return nil, err
	}
	id := secure.ID("md")
	if _, err := s.DB.Exec(ctx, `INSERT INTO brand_media (id, brand_id, media, created_at) VALUES ($1,$2,$3,$4)`, id, who.BrandID, photo, s.Now()); err != nil {
		return nil, err
	}
	return domain.GridTile{ID: id, Image: photo}, nil
}

/* ---------- posts ---------- */

var postTypes = map[string]bool{"Post": true, "Reel": true, "Story": true}

func (s *Service) createPost(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	var in struct {
		Type       string   `json:"type"`
		MediaIDs   []string `json:"mediaIds"`
		ProductIDs []string `json:"productIds"`
		Caption    string   `json:"caption"`
		When       string   `json:"when"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return nil, err
	}
	if !postTypes[in.Type] {
		in.Type = "Post"
	}
	if len(in.MediaIDs) == 0 {
		return nil, httpx.BadRequest("Pick at least one photo")
	}
	if len(in.MediaIDs) > 6 {
		return nil, httpx.BadRequest("Use up to 6 photos")
	}
	caption := strings.TrimSpace(in.Caption)
	if len([]rune(caption)) > 2200 {
		return nil, httpx.BadRequest("That caption is too long")
	}
	photos, err := s.brandMediaFor(ctx, who.BrandID, in.MediaIDs)
	if err != nil {
		return nil, err
	}
	if in.Type == "Reel" && photos[0].Kind != "video" {
		return nil, httpx.BadRequest("A reel needs a video — upload one to your media first")
	}
	products, err := views.ProductsWhere(ctx, s.DB, `WHERE p.id = ANY($1) AND p.brand_id = $2`, in.ProductIDs, who.BrandID)
	if err != nil {
		return nil, err
	}
	if len(products) != len(in.ProductIDs) {
		return nil, httpx.BadRequest("You can only tag your own products")
	}

	now := s.Now()
	status := "published"
	var publishAt, publishedAt *time.Time
	withDrop := false
	switch in.When {
	case "", "Now":
		publishedAt = &now
	case "Tomorrow 09:00":
		status = "scheduled"
		local := now.In(domain.Location()).AddDate(0, 0, 1)
		at := time.Date(local.Year(), local.Month(), local.Day(), 9, 0, 0, 0, local.Location())
		publishAt = &at
	case "With next drop":
		next, err := s.nextDrop(ctx, s.DB, who.BrandID)
		if err != nil {
			return nil, err
		}
		if next == nil {
			return nil, httpx.BadRequest("Set a date for your next drop first")
		}
		at, err := time.Parse(time.RFC3339, next.ReleasesAt)
		if err != nil {
			return nil, err
		}
		status, withDrop, publishAt = "scheduled", true, &at
	default:
		return nil, httpx.BadRequest("Choose when to publish")
	}
	var expires *time.Time
	if in.Type == "Story" && publishedAt != nil {
		e := publishedAt.Add(24 * time.Hour)
		expires = &e
	}
	aspect := 390.0 / 480.0
	if photos[0].Aspect > 0 {
		aspect = clamp(photos[0].Aspect, 0.6, 1.91)
	}
	id := secure.ID("po")
	var location string
	if err := s.DB.QueryRow(ctx, `SELECT location FROM brands WHERE id = $1`, who.BrandID).Scan(&location); err != nil {
		return nil, err
	}
	err = db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO posts (id, brand_id, type, status, publish_at, with_next_drop, expires_at, media, aspect, subtitle, caption, created_at, published_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
			id, who.BrandID, in.Type, status, publishAt, withDrop, expires, photos, aspect, location, caption, now, publishedAt); err != nil {
			return err
		}
		for i, p := range products {
			y := 12.0 + float64(i)*11
			if y > 80 {
				y = 80
			}
			label := fmt.Sprintf("%s — $%d", p.Summary.Name, p.Summary.Price/100)
			if _, err := tx.Exec(ctx, `INSERT INTO post_tags (post_id, position, product_id, label, x, y) VALUES ($1,$2,$3,$4,6,$5)`, id, i, p.Summary.ID, label, y); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO post_shop (post_id, position, product_id) VALUES ($1,$2,$3)`, id, i, p.Summary.ID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "id": id, "status": status}, nil
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// brandMediaFor resolves library ids in the order given, rejecting anything not in this brand's library.
func (s *Service) brandMediaFor(ctx context.Context, brandID string, ids []string) ([]domain.Photo, error) {
	rows, err := s.DB.Query(ctx, `SELECT id, media FROM brand_media WHERE brand_id = $1 AND id = ANY($2)`, brandID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	found := map[string]domain.Photo{}
	for rows.Next() {
		var id string
		var p domain.Photo
		if err := rows.Scan(&id, &p); err != nil {
			return nil, err
		}
		found[id] = p
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]domain.Photo, 0, len(ids))
	for _, id := range ids {
		p, ok := found[id]
		if !ok {
			return nil, httpx.BadRequest("That photo isn’t in your media library")
		}
		out = append(out, p)
	}
	return out, nil
}

// PublishDuePosts is run by the background job.
func (s *Service) PublishDuePosts(ctx context.Context) error {
	now := s.Now()
	_, err := s.DB.Exec(ctx, `UPDATE posts SET status = 'published', published_at = $1,
		expires_at = CASE WHEN type = 'Story' THEN $1::timestamptz + interval '24 hours' ELSE NULL END
		WHERE status = 'scheduled' AND NOT with_next_drop AND publish_at <= $1`, now)
	return err
}
