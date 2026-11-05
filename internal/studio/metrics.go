package studio

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"wearwhat-be/internal/domain"
	"wearwhat-be/internal/httpx"
	"wearwhat-be/internal/views"
)

// counted orders exclude ones whose money went back to the buyer.
const countedOrder = views.PaidOrder + ` AND o.status <> 'RETURN APPROVED'`

type periodTotals struct {
	gmv, orders, views, tries, triedOrders int
}

func (s *Service) totals(ctx context.Context, brandID string, from, to time.Time) (periodTotals, error) {
	var t periodTotals
	err := s.DB.QueryRow(ctx, `SELECT COALESCE(sum(o.subtotal - o.brand_discount), 0), count(*), count(*) FILTER (WHERE o.from_try_on)
		FROM orders o WHERE o.brand_id = $1 AND `+countedOrder+` AND o.placed_at >= $2 AND o.placed_at < $3`, brandID, from, to).
		Scan(&t.gmv, &t.orders, &t.triedOrders)
	if err != nil {
		return t, err
	}
	err = s.DB.QueryRow(ctx, `SELECT count(*) FILTER (WHERE type = 'product_view'), count(*) FILTER (WHERE type = 'try_on')
		FROM analytics_events WHERE brand_id = $1 AND at >= $2 AND at < $3`, brandID, from, to).Scan(&t.views, &t.tries)
	return t, err
}

func (t periodTotals) conversion() (float64, bool) {
	if t.views == 0 {
		return 0, false
	}
	return float64(t.orders) / float64(t.views) * 100, true
}

// tryOnLift compares conversion for people who tried a piece on against those who didn't.
func (t periodTotals) tryOnLift() (float64, bool) {
	viewsWithout := t.views - t.tries
	ordersWithout := t.orders - t.triedOrders
	if t.tries == 0 || viewsWithout <= 0 || ordersWithout <= 0 {
		return 0, false
	}
	with := float64(t.triedOrders) / float64(t.tries)
	without := float64(ordersWithout) / float64(viewsWithout)
	return (with/without - 1) * 100, true
}

func changeLabel(cur, prev int) string {
	if prev == 0 {
		if cur == 0 {
			return "No change"
		}
		return "New this period"
	}
	return fmt.Sprintf("%+.1f%% vs prev", (float64(cur)-float64(prev))/float64(prev)*100)
}

func (s *Service) metrics(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	period := r.URL.Query().Get("period")
	days := map[string]int{"7D": 7, "30D": 30, "90D": 90}[period]
	if days == 0 {
		period, days = "30D", 30
	}
	now := s.Now()
	from := now.AddDate(0, 0, -days)
	prevFrom := from.AddDate(0, 0, -days)

	cur, err := s.totals(ctx, who.BrandID, from, now)
	if err != nil {
		return nil, err
	}
	prev, err := s.totals(ctx, who.BrandID, prevFrom, from)
	if err != nil {
		return nil, err
	}

	out := domain.StudioMetrics{
		Period: period,
		Range:  domain.DateRange(from, now),
		GMV:    domain.Metric{Value: cur.gmv, Delta: changeLabel(cur.gmv, prev.gmv)},
		Orders: domain.Metric{Value: cur.orders, Delta: changeLabel(cur.orders, prev.orders)},
	}

	if c, ok := cur.conversion(); ok {
		delta := "No earlier data"
		if p, ok := prev.conversion(); ok {
			delta = fmt.Sprintf("%+.1f pts", c-p)
		}
		out.Conversion = domain.Metric{Value: fmt.Sprintf("%.1f%%", c), Delta: delta}
	} else {
		out.Conversion = domain.Metric{Value: "—", Delta: "No views yet"}
	}
	if l, ok := cur.tryOnLift(); ok {
		out.TryOnLift = domain.Metric{Value: fmt.Sprintf("%+.0f%%", l), Delta: "vs non-try-on"}
	} else {
		out.TryOnLift = domain.Metric{Value: "—", Delta: "Not enough data"}
	}

	if out.Bars, err = s.bars(ctx, who.BrandID, from, days); err != nil {
		return nil, err
	}

	var added int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM analytics_events WHERE brand_id = $1 AND type = 'add_to_bag' AND from_try_on AND at >= $2`, who.BrandID, from).Scan(&added); err != nil {
		return nil, err
	}
	out.Funnel = []domain.FunnelStep{
		{Label: "Try-on sessions", Value: cur.tries, Pct: pct(cur.tries, cur.tries)},
		{Label: "Added to bag", Value: added, Pct: pct(added, cur.tries)},
		{Label: "Purchased", Value: cur.triedOrders, Pct: pct(cur.triedOrders, cur.tries)},
	}

	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM products WHERE brand_id = $1 AND flat_lay IS NULL`, who.BrandID).Scan(&out.MissingAssets); err != nil {
		return nil, err
	}
	if out.TopPosts, err = s.topPosts(ctx, who.BrandID, from); err != nil {
		return nil, err
	}
	return out, nil
}

func pct(part, whole int) float64 {
	if whole == 0 {
		return 0
	}
	return math.Min(100, math.Round(float64(part)/float64(whole)*1000)/10)
}

// bars buckets sales into 14 columns across the period, scaled to the tallest.
func (s *Service) bars(ctx context.Context, brandID string, from time.Time, days int) ([]int, error) {
	const buckets = 14
	span := time.Duration(days) * 24 * time.Hour / buckets
	rows, err := s.DB.Query(ctx, `SELECT o.placed_at, o.subtotal - o.brand_discount FROM orders o
		WHERE o.brand_id = $1 AND `+countedOrder+` AND o.placed_at >= $2`, brandID, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sums := make([]int, buckets)
	for rows.Next() {
		var at time.Time
		var v int
		if err := rows.Scan(&at, &v); err != nil {
			return nil, err
		}
		if i := int(at.Sub(from) / span); i >= 0 && i < buckets {
			sums[i] += v
		} else if i >= buckets {
			sums[buckets-1] += v
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	top := 0
	for _, v := range sums {
		if v > top {
			top = v
		}
	}
	out := make([]int, buckets)
	for i, v := range sums {
		switch {
		case v == 0 || top == 0:
			out[i] = 2 // a sliver, so an empty day still reads as a day
		default:
			out[i] = max(8, int(math.Round(float64(v)/float64(top)*100)))
		}
	}
	return out, nil
}

// topPosts ranks posts by the sales they actually led to.
func (s *Service) topPosts(ctx context.Context, brandID string, from time.Time) ([]domain.TopPost, error) {
	rows, err := s.DB.Query(ctx, `SELECT p.id, p.caption, p.media -> 0, u.handle, sum(o.subtotal - o.brand_discount), count(*),
			(SELECT count(*) FROM analytics_events a WHERE a.post_id = p.id AND a.type = 'post_view' AND a.at >= $2)
		FROM orders o JOIN posts p ON p.id = o.source_post_id LEFT JOIN users u ON u.id = p.user_id
		WHERE o.brand_id = $1 AND `+countedOrder+` AND o.placed_at >= $2
		GROUP BY p.id, u.handle ORDER BY sum(o.subtotal - o.brand_discount) DESC LIMIT 3`, brandID, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.TopPost{}
	for rows.Next() {
		var t domain.TopPost
		var caption string
		var handle *string
		var orders, views int
		if err := rows.Scan(&t.ID, &caption, &t.Image, &handle, &t.Value, &orders, &views); err != nil {
			return nil, err
		}
		t.Title = postTitle(caption, handle)
		parts := []string{}
		if views > 0 {
			parts = append(parts, compact(views)+" views")
		}
		parts = append(parts, fmt.Sprintf("%d orders", orders))
		if handle != nil {
			parts = append(parts, "UGC")
		}
		t.Meta = strings.Join(parts, " · ")
		out = append(out, t)
	}
	return out, rows.Err()
}

func postTitle(caption string, handle *string) string {
	if handle != nil {
		return *handle + " — wear"
	}
	r := []rune(strings.TrimSpace(caption))
	if len(r) == 0 {
		return "Post"
	}
	if len(r) > 36 {
		return string(r[:36]) + "…"
	}
	return string(r)
}

func compact(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprint(n)
}
