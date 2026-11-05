// Package views turns database rows into the API's response shapes for use across features.
package views

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"wearwhat-be/internal/db"
	"wearwhat-be/internal/domain"
)

// ProductVisible is the SQL rule for what shoppers can see (aliases: p = products, b = brands):
// not hidden, from a verified brand, and not waiting on an unreleased drop.
const ProductVisible = `(NOT p.hidden AND b.status = 'verified' AND (p.drop_id IS NULL OR EXISTS (
	SELECT 1 FROM drops d WHERE d.id = p.drop_id AND d.released_at IS NOT NULL)))`

// PaidOrder is the SQL rule for orders that really happened (alias: o = orders).
const PaidOrder = `o.status NOT IN ('PENDING', 'CANCELLED')`

// NotBlocked is the SQL rule hiding content between two people who have blocked each other.
// other is the SQL expression for the other person's id; $1 must be the viewer's id.
func NotBlocked(other string) string {
	return fmt.Sprintf(`NOT EXISTS (SELECT 1 FROM blocks bl WHERE (bl.blocker_id = $1 AND bl.blocked_id = %[1]s) OR (bl.blocker_id = %[1]s AND bl.blocked_id = $1))`, other)
}

const productCols = `p.id, p.name, p.price, p.images, p.placeholder, p.category, p.flat_lay, p.colors,
	p.description, p.color, p.hidden, p.drop_id, b.id, b.handle, b.name,
	COALESCE((SELECT json_agg(json_build_object('label', s.label, 'stock', s.stock) ORDER BY s.position)
		FROM product_sizes s WHERE s.product_id = p.id), '[]'::json)`

const productFrom = ` FROM products p JOIN brands b ON b.id = p.brand_id `

// ProductRow is a product with everything features need beyond the summary.
type ProductRow struct {
	Summary     domain.ProductSummary
	Images      []domain.Photo
	FlatLay     *domain.Photo
	Colors      []domain.ColorOption
	Sizes       []domain.SizeStock
	Description string
	Color       string
	Hidden      bool
	DropID      *string
}

// Stock is the total across sizes.
func (p ProductRow) Stock() int {
	n := 0
	for _, s := range p.Sizes {
		n += s.Stock
	}
	return n
}

func scanProduct(row pgx.Row, extra ...any) (ProductRow, error) {
	var p ProductRow
	var s = &p.Summary
	dest := append([]any{
		&s.ID, &s.Name, &s.Price, &p.Images, &s.Placeholder, &s.Category, &p.FlatLay, &p.Colors,
		&p.Description, &p.Color, &p.Hidden, &p.DropID, &s.Brand.ID, &s.Brand.Handle, &s.Brand.Name, &p.Sizes,
	}, extra...)
	if err := row.Scan(dest...); err != nil {
		return p, err
	}
	s.Currency = "USD"
	if len(p.Images) > 0 {
		img := p.Images[0]
		s.Image = &img
	}
	s.TryOnReady = p.FlatLay != nil
	s.DefaultSize = domain.DefaultSize(p.Sizes)
	if len(p.Colors) > 0 {
		s.DefaultColor = p.Colors[0].Name
	}
	return p, nil
}

// ProductsWhere loads products matching a SQL condition, in the order it specifies.
func ProductsWhere(ctx context.Context, q db.Q, cond string, args ...any) ([]ProductRow, error) {
	rows, err := q.Query(ctx, `SELECT `+productCols+productFrom+cond, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProductRow{}
	for rows.Next() {
		p, err := scanProduct(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ProductByID loads one product regardless of visibility.
func ProductByID(ctx context.Context, q db.Q, id string) (ProductRow, error) {
	return scanProduct(q.QueryRow(ctx, `SELECT `+productCols+productFrom+`WHERE p.id = $1`, id))
}

// Summaries batch-loads product summaries by id.
func Summaries(ctx context.Context, q db.Q, ids []string) (map[string]domain.ProductSummary, error) {
	out := make(map[string]domain.ProductSummary, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := ProductsWhere(ctx, q, `WHERE p.id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	for _, p := range rows {
		out[p.Summary.ID] = p.Summary
	}
	return out, nil
}

// SummaryPtr loads a single summary, or nil when id is nil or missing.
func SummaryPtr(ctx context.Context, q db.Q, id *string) (*domain.ProductSummary, error) {
	if id == nil {
		return nil, nil
	}
	p, err := ProductByID(ctx, q, *id)
	if db.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p.Summary, nil
}

// BrandShipping is the settings a shopper-facing screen needs.
func BrandShipping(ctx context.Context, q db.Q, brandID string) (domain.ShippingSettings, error) {
	var s domain.ShippingSettings
	err := q.QueryRow(ctx, `SELECT shipping FROM brands WHERE id = $1`, brandID).Scan(&s)
	return s, err
}
