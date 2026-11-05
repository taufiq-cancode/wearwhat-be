// Package commerce handles the bag, promotions, checkout, payment and orders.
package commerce

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"wearwhat-be/internal/app"
	"wearwhat-be/internal/db"
	"wearwhat-be/internal/domain"
	"wearwhat-be/internal/httpx"
	"wearwhat-be/internal/secure"
	"wearwhat-be/internal/views"
)

const maxLineQty = 20

type Service struct{ *app.Deps }

func (s *Service) Routes(r *httpx.Router) {
	r.User("GET /cart", s.getCart)
	r.User("POST /cart/items", s.addItem)
	r.User("PATCH /cart/items/{id}", s.setQty)
	r.User("DELETE /cart/items/{id}", s.removeItem)
	r.User("PUT /cart/promo", s.applyPromo)
	r.User("DELETE /cart/promo", s.removePromo)

	r.User("POST /checkout", s.checkout)
	r.User("GET /checkouts/{number}", s.getCheckout)
	r.Mux.HandleFunc("POST "+r.Base+"/webhooks/paystack", s.paystackWebhook(r))

	r.User("GET /me/orders", s.myOrders)
	r.User("GET /orders/{id}", s.getOrder)
	r.User("POST /orders/{id}/return", s.requestReturn)
	r.User("POST /products/{id}/reviews", s.addReview)
}

/* ---------- bag ---------- */

type line struct {
	ID         string
	ProductID  string
	Size       string
	Color      string
	Qty        int
	SourcePost *string
	Product    views.ProductRow
	DropTitle  string
}

func (l line) subtotal() int { return l.Qty * l.Product.Summary.Price }

func (s *Service) loadLines(ctx context.Context, q db.Q, userID string, lock bool) ([]line, error) {
	sql := `SELECT id, product_id, size, color, qty, source_post_id FROM cart_lines WHERE user_id = $1 ORDER BY created_at, id`
	if lock {
		sql += ` FOR UPDATE`
	}
	rows, err := q.Query(ctx, sql, userID)
	if err != nil {
		return nil, err
	}
	lines := []line{}
	ids := []string{}
	for rows.Next() {
		var l line
		if err := rows.Scan(&l.ID, &l.ProductID, &l.Size, &l.Color, &l.Qty, &l.SourcePost); err != nil {
			rows.Close()
			return nil, err
		}
		lines = append(lines, l)
		ids = append(ids, l.ProductID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return lines, nil
	}
	products, err := views.ProductsWhere(ctx, q, `WHERE p.id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]views.ProductRow, len(products))
	dropIDs := []string{}
	for _, p := range products {
		byID[p.Summary.ID] = p
		if p.DropID != nil {
			dropIDs = append(dropIDs, *p.DropID)
		}
	}
	titles := map[string]string{}
	if len(dropIDs) > 0 {
		dr, err := q.Query(ctx, `SELECT id, title FROM drops WHERE id = ANY($1)`, dropIDs)
		if err != nil {
			return nil, err
		}
		for dr.Next() {
			var id, title string
			if err := dr.Scan(&id, &title); err != nil {
				dr.Close()
				return nil, err
			}
			titles[id] = title
		}
		dr.Close()
		if err := dr.Err(); err != nil {
			return nil, err
		}
	}
	for i := range lines {
		lines[i].Product = byID[lines[i].ProductID]
		if d := lines[i].Product.DropID; d != nil {
			lines[i].DropTitle = titles[*d]
		}
	}
	return lines, nil
}

// cart builds the bag response. A promo that no longer applies is removed quietly.
func (s *Service) cart(ctx context.Context, userID string) (domain.Cart, error) {
	lines, err := s.loadLines(ctx, s.DB, userID, false)
	if err != nil {
		return domain.Cart{}, err
	}
	out := domain.Cart{Lines: make([]domain.CartLine, 0, len(lines))}
	for _, l := range lines {
		out.Lines = append(out.Lines, domain.CartLine{ID: l.ID, Product: l.Product.Summary, Size: l.Size, Color: l.Color, Qty: l.Qty})
	}
	var code string
	err = s.DB.QueryRow(ctx, `SELECT code FROM cart_promos WHERE user_id = $1`, userID).Scan(&code)
	if db.IsNotFound(err) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	p, err := loadPromo(ctx, s.DB, code, false)
	if err == nil {
		applied := p.apply(lines)
		if p.usable(s.Now(), userID) && applied.eligible {
			out.Promo = &domain.CartPromo{Code: p.Code, Label: domain.PromoLabel(p.Type, p.Value), Discount: applied.total(), FreeShipping: applied.freeShipping}
			return out, nil
		}
	} else if !db.IsNotFound(err) {
		return out, err
	}
	_, err = s.DB.Exec(ctx, `DELETE FROM cart_promos WHERE user_id = $1`, userID)
	return out, err
}

func (s *Service) getCart(_ http.ResponseWriter, r *http.Request) (any, error) {
	return s.cart(r.Context(), httpx.Who(r).UserID)
}

// checkStock validates a size and quantity against what's actually available.
func checkStock(ctx context.Context, q db.Q, productID, size string, qty int) error {
	var stock int
	err := q.QueryRow(ctx, `SELECT stock FROM product_sizes WHERE product_id = $1 AND label = $2`, productID, size).Scan(&stock)
	if db.IsNotFound(err) {
		return httpx.BadRequest("Pick a size")
	}
	if err != nil {
		return err
	}
	switch {
	case stock == 0:
		return httpx.Conflict(size + " is sold out")
	case qty > stock:
		return httpx.Conflict(fmt.Sprintf("Only %d left in %s", stock, size))
	}
	return nil
}

func (s *Service) addItem(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	var in struct {
		ProductID string  `json:"productId"`
		Size      string  `json:"size"`
		Color     string  `json:"color"`
		Qty       int     `json:"qty"`
		PostID    *string `json:"postId"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return nil, err
	}
	if in.Qty == 0 {
		in.Qty = 1
	}
	if in.Qty < 1 || in.Qty > maxLineQty {
		return nil, httpx.BadRequest(fmt.Sprintf("You can add up to %d at a time", maxLineQty))
	}
	rows, err := views.ProductsWhere(ctx, s.DB, `WHERE p.id = $1 AND `+views.ProductVisible, in.ProductID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, httpx.NotFound("This piece isn’t available any more")
	}
	p := rows[0]
	if in.Size == "" {
		in.Size = p.Summary.DefaultSize
	}
	if in.Color == "" {
		in.Color = p.Summary.DefaultColor
	}
	if !hasColor(p.Colors, in.Color) {
		return nil, httpx.BadRequest("Pick a colour")
	}
	if in.PostID != nil {
		var exists bool
		if err := s.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM posts WHERE id = $1)`, *in.PostID).Scan(&exists); err != nil {
			return nil, err
		}
		if !exists {
			in.PostID = nil
		}
	}
	err = db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		var current int
		err := tx.QueryRow(ctx, `SELECT qty FROM cart_lines WHERE user_id = $1 AND product_id = $2 AND size = $3 AND color = $4 FOR UPDATE`,
			who.UserID, in.ProductID, in.Size, in.Color).Scan(&current)
		if err != nil && !db.IsNotFound(err) {
			return err
		}
		total := current + in.Qty
		if total > maxLineQty {
			return httpx.BadRequest(fmt.Sprintf("You can have up to %d of a piece in your bag", maxLineQty))
		}
		if err := checkStock(ctx, tx, in.ProductID, in.Size, total); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO cart_lines (id, user_id, product_id, size, color, qty, source_post_id) VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (user_id, product_id, size, color) DO UPDATE SET qty = cart_lines.qty + EXCLUDED.qty,
				source_post_id = COALESCE(EXCLUDED.source_post_id, cart_lines.source_post_id)`,
			secure.ID("l"), who.UserID, in.ProductID, in.Size, in.Color, in.Qty, in.PostID); err != nil {
			return err
		}
		tried, err := triedOn(ctx, tx, who.UserID, in.ProductID, s.Now())
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO analytics_events (type, brand_id, product_id, user_id, from_try_on, at) VALUES ('add_to_bag',$1,$2,$3,$4,$5)`,
			p.Summary.Brand.ID, in.ProductID, who.UserID, tried, s.Now())
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.cart(ctx, who.UserID)
}

func hasColor(colors []domain.ColorOption, name string) bool {
	if len(colors) == 0 {
		return true
	}
	for _, c := range colors {
		if c.Name == name {
			return true
		}
	}
	return false
}

// triedOn reports a try-on of this piece in the past week, which attributes a purchase to try-on.
func triedOn(ctx context.Context, q db.Q, userID, productID string, now time.Time) (bool, error) {
	var tried bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM analytics_events WHERE user_id = $1 AND type = 'try_on' AND product_id = $2
		AND at > $3::timestamptz - interval '7 days')`, userID, productID, now).Scan(&tried)
	return tried, err
}

func (s *Service) setQty(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	var in struct {
		Qty int `json:"qty"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return nil, err
	}
	if in.Qty < 1 {
		return nil, httpx.BadRequest("Quantity must be at least 1")
	}
	if in.Qty > maxLineQty {
		return nil, httpx.BadRequest(fmt.Sprintf("You can have up to %d of a piece in your bag", maxLineQty))
	}
	return nil, db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		var productID, size string
		err := tx.QueryRow(ctx, `SELECT product_id, size FROM cart_lines WHERE id = $1 AND user_id = $2 FOR UPDATE`, r.PathValue("id"), who.UserID).Scan(&productID, &size)
		if db.IsNotFound(err) {
			return httpx.NotFound("Not in your bag")
		}
		if err != nil {
			return err
		}
		if err := checkStock(ctx, tx, productID, size, in.Qty); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE cart_lines SET qty = $2 WHERE id = $1`, r.PathValue("id"), in.Qty)
		return err
	})
}

func (s *Service) removeItem(_ http.ResponseWriter, r *http.Request) (any, error) {
	_, err := s.DB.Exec(r.Context(), `DELETE FROM cart_lines WHERE id = $1 AND user_id = $2`, r.PathValue("id"), httpx.Who(r).UserID)
	return nil, err
}

/* ---------- promotions ---------- */

type promo struct {
	Code, Type, Value, Scope string
	BrandID, UserID          *string
	EndsAt                   *time.Time
	Uses                     int
	MaxUses                  *int
	Live                     bool
}

func loadPromo(ctx context.Context, q db.Q, code string, lock bool) (promo, error) {
	var p promo
	sql := `SELECT code, type, value, scope, brand_id, user_id, ends_at, uses, max_uses, live FROM promotions WHERE code = $1`
	if lock {
		sql += ` FOR UPDATE`
	}
	err := q.QueryRow(ctx, sql, code).Scan(&p.Code, &p.Type, &p.Value, &p.Scope, &p.BrandID, &p.UserID, &p.EndsAt, &p.Uses, &p.MaxUses, &p.Live)
	return p, err
}

// usable checks whether the code can be redeemed right now by this person.
func (p promo) usable(now time.Time, userID string) bool {
	switch {
	case !p.Live:
		return false
	case p.EndsAt != nil && !now.Before(*p.EndsAt):
		return false
	case p.MaxUses != nil && p.Uses >= *p.MaxUses:
		return false
	case p.UserID != nil && *p.UserID != userID:
		return false
	}
	return true
}

type appliedPromo struct {
	perLine      []int
	freeShipping bool
	eligible     bool
}

func (a appliedPromo) total() int {
	n := 0
	for _, v := range a.perLine {
		n += v
	}
	return n
}

// matches reports whether a line falls inside the promo's brand and scope.
func (p promo) matches(l line) bool {
	if p.BrandID != nil && *p.BrandID != l.Product.Summary.Brand.ID {
		return false
	}
	return p.Scope == "" || p.Scope == "Everything" || p.Scope == l.Product.Summary.Category || (l.DropTitle != "" && p.Scope == l.DropTitle)
}

// apply computes the discount for each line. Lines outside the promo get nothing.
func (p promo) apply(lines []line) appliedPromo {
	out := appliedPromo{perLine: make([]int, len(lines))}
	eligible := make([]int, len(lines))
	for i, l := range lines {
		if p.matches(l) {
			eligible[i] = l.subtotal()
			out.eligible = true
		}
	}
	if !out.eligible {
		return out
	}
	if p.Type == "Free ship" {
		out.freeShipping = true
		return out
	}
	out.perLine = domain.PromoDiscount(p.Type, p.Value, eligible)
	return out
}

func (s *Service) applyPromo(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	var in struct {
		Code string `json:"code"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return nil, err
	}
	code := strings.ToUpper(strings.TrimSpace(in.Code))
	if code == "" {
		return nil, httpx.BadRequest("Enter a promo code")
	}
	// slows guessing: codes aren't secret, but single-use rewards shouldn't be fished for
	if err := httpx.Limit(ctx, s.DB, "promo:"+who.UserID, 30, 10*time.Minute, "Too many tries — wait a few minutes"); err != nil {
		return nil, err
	}
	p, err := loadPromo(ctx, s.DB, code, false)
	invalid := httpx.BadRequest("That code isn’t valid")
	if db.IsNotFound(err) {
		return nil, invalid
	}
	if err != nil {
		return nil, err
	}
	if !p.usable(s.Now(), who.UserID) {
		return nil, invalid
	}
	lines, err := s.loadLines(ctx, s.DB, who.UserID, false)
	if err != nil {
		return nil, err
	}
	if !p.apply(lines).eligible {
		return nil, httpx.BadRequest("That code doesn’t apply to anything in your bag")
	}
	if _, err := s.DB.Exec(ctx, `INSERT INTO cart_promos (user_id, code) VALUES ($1,$2) ON CONFLICT (user_id) DO UPDATE SET code = $2`, who.UserID, code); err != nil {
		return nil, err
	}
	return s.cart(ctx, who.UserID)
}

func (s *Service) removePromo(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	if _, err := s.DB.Exec(r.Context(), `DELETE FROM cart_promos WHERE user_id = $1`, who.UserID); err != nil {
		return nil, err
	}
	return s.cart(r.Context(), who.UserID)
}
