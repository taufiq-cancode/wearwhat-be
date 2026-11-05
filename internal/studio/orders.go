package studio

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"wearwhat-be/internal/commerce"
	"wearwhat-be/internal/db"
	"wearwhat-be/internal/domain"
	"wearwhat-be/internal/httpx"
	"wearwhat-be/internal/views"
)

func itoa(n int) string { return strconv.Itoa(n) }

// orderView shows the piece at the price this order paid, not today's price.
func (s *Service) orderView(o commerce.OrderRow, product domain.ProductSummary, customer domain.Customer) domain.StudioOrder {
	product.Price = o.UnitPrice
	return domain.StudioOrder{
		ID:        domain.StudioOrderID(o.Seq),
		Number:    "#" + domain.StudioOrderID(o.Seq),
		Customer:  customer,
		Product:   product,
		Size:      o.Size,
		Color:     o.Color,
		Qty:       o.Qty,
		Status:    o.Status,
		PlacedAgo: domain.Ago(o.PlacedAt, s.Now()),
		FromTryOn: o.FromTryOn,
	}
}

func (s *Service) studioOrder(ctx context.Context, q db.Q, o commerce.OrderRow) (domain.StudioOrder, error) {
	var c domain.Customer
	var handle string
	if err := q.QueryRow(ctx, `SELECT id, name, handle FROM users WHERE id = $1`, o.UserID).Scan(&c.ID, &c.Name, &handle); err != nil {
		return domain.StudioOrder{}, err
	}
	c.Handle = "@" + handle
	p, err := views.ProductByID(ctx, q, o.ProductID)
	if err != nil {
		return domain.StudioOrder{}, err
	}
	return s.orderView(o, p.Summary, c), nil
}

func (s *Service) listOrders(_ http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	rows, err := s.DB.Query(ctx, `SELECT `+commerce.OrderCols+` FROM orders o WHERE o.brand_id = $1 AND `+views.PaidOrder+`
		ORDER BY o.placed_at DESC, o.seq DESC LIMIT 200`, httpx.Who(r).BrandID)
	if err != nil {
		return nil, err
	}
	list := []commerce.OrderRow{}
	productIDs := []string{}
	userIDs := []string{}
	for rows.Next() {
		o, err := commerce.ScanOrder(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, o)
		productIDs = append(productIDs, o.ProductID)
		userIDs = append(userIDs, o.UserID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	summaries, err := views.Summaries(ctx, s.DB, productIDs)
	if err != nil {
		return nil, err
	}
	customers := map[string]domain.Customer{}
	urows, err := s.DB.Query(ctx, `SELECT id, name, handle FROM users WHERE id = ANY($1)`, userIDs)
	if err != nil {
		return nil, err
	}
	for urows.Next() {
		var c domain.Customer
		var handle string
		if err := urows.Scan(&c.ID, &c.Name, &handle); err != nil {
			urows.Close()
			return nil, err
		}
		c.Handle = "@" + handle
		customers[c.ID] = c
	}
	urows.Close()
	if err := urows.Err(); err != nil {
		return nil, err
	}
	out := make([]domain.StudioOrder, 0, len(list))
	for _, o := range list {
		out = append(out, s.orderView(o, summaries[o.ProductID], customers[o.UserID]))
	}
	return out, nil
}

// brandOrder loads an order only if it belongs to the caller's brand.
func (s *Service) brandOrder(ctx context.Context, who *httpx.Principal, id string) (commerce.OrderRow, error) {
	seq, ok := domain.ParseOrderID(id)
	if !ok {
		return commerce.OrderRow{}, httpx.NotFound("Order not found")
	}
	o, err := commerce.ScanOrder(s.DB.QueryRow(ctx, `SELECT `+commerce.OrderCols+` FROM orders o WHERE o.seq = $1 AND o.brand_id = $2 AND `+views.PaidOrder, seq, who.BrandID))
	if db.IsNotFound(err) {
		return o, httpx.NotFound("Order not found")
	}
	return o, err
}

func (s *Service) getOrder(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	o, err := s.brandOrder(ctx, who, r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	base, err := s.studioOrder(ctx, s.DB, o)
	if err != nil {
		return nil, err
	}
	out := domain.StudioOrderDetail{
		StudioOrder:   base,
		Subtotal:      o.Subtotal,
		Discount:      o.Discount,
		Shipping:      o.Shipping,
		Fee:           o.Fee,
		Payout:        o.Payout,
		ShipTo:        o.ShipTo,
		ShippingSpeed: o.Speed,
		Carrier:       o.Carrier,
		TrackingCode:  o.Tracking,
		ReturnNote:    o.ReturnNote,
	}
	if o.ReturnAt != nil {
		out.ReturnAge = domain.Ago(*o.ReturnAt, s.Now())
	}
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM orders o WHERE o.user_id = $1 AND o.brand_id = $2 AND `+views.PaidOrder, o.UserID, who.BrandID).Scan(&out.CustomerOrders); err != nil {
		return nil, err
	}
	var ship domain.ShippingSettings
	if err := s.DB.QueryRow(ctx, `SELECT name, shipping FROM brands WHERE id = $1`, who.BrandID).Scan(&out.BrandName, &ship); err != nil {
		return nil, err
	}
	out.ShipsFrom = ship.ShipsFrom
	if out.Timeline, err = s.timeline(ctx, o); err != nil {
		return nil, err
	}
	return out, nil
}

// timeline shows what has really happened to the order, and what's still to come.
func (s *Service) timeline(ctx context.Context, o commerce.OrderRow) ([]domain.TimelineStep, error) {
	rows, err := s.DB.Query(ctx, `SELECT stage, at FROM order_events WHERE order_seq = $1 ORDER BY at, id`, o.Seq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	at := map[string]time.Time{}
	for rows.Next() {
		var stage string
		var t time.Time
		if err := rows.Scan(&stage, &t); err != nil {
			return nil, err
		}
		at[stage] = t
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	now := s.Now()
	step := func(title, key, fallback string) domain.TimelineStep {
		t, ok := at[key]
		st := domain.TimelineStep{Title: title, Detail: fallback, Done: ok}
		if ok && fallback == "" {
			st.Detail = domain.Ago(t, now)
		}
		return st
	}
	paidDetail := ""
	if method, err := s.paymentLabel(ctx, o); err != nil {
		return nil, err
	} else if method != "" {
		paidDetail = method
	}
	placed := domain.TimelineStep{Title: "Order placed", Detail: domain.Ago(o.PlacedAt, now), Done: true}
	if placed.Detail != "now" {
		placed.Detail += " ago"
	}
	steps := []domain.TimelineStep{
		placed,
		step("Payment captured", "paid", paidDetail),
		step("Shipped", "shipped", o.Carrier),
	}
	if _, ok := at["out_for_delivery"]; ok || o.Status == domain.StatusShipped {
		steps = append(steps, step("Out for delivery", "out_for_delivery", ""))
	}
	steps = append(steps, step("Delivered", "delivered", ""))
	if _, ok := at["return_requested"]; ok {
		steps = append(steps, step("Return requested", "return_requested", ""))
	}
	if _, ok := at["return_approved"]; ok {
		steps = append(steps, step("Return approved", "return_approved", ""))
	}
	if _, ok := at["return_declined"]; ok {
		steps = append(steps, step("Return declined", "return_declined", ""))
	}
	return steps, nil
}

func (s *Service) paymentLabel(ctx context.Context, o commerce.OrderRow) (string, error) {
	var provider, method *string
	err := s.DB.QueryRow(ctx, `SELECT c.provider, c.payment_method FROM orders o JOIN checkouts c ON c.number = o.checkout_number WHERE o.seq = $1`, o.Seq).Scan(&provider, &method)
	if db.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	label := map[string]string{"card": "card", "apple_pay": "Apple Pay", "bank_transfer": "bank transfer"}[*method]
	if *provider == "test" {
		return "Test payment · " + label, nil
	}
	return "Paystack · " + label, nil
}

func (s *Service) patchOrder(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	var in struct {
		Status       string `json:"status"`
		Carrier      string `json:"carrier"`
		TrackingCode string `json:"trackingCode"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return nil, err
	}
	seq, ok := domain.ParseOrderID(r.PathValue("id"))
	if !ok {
		return nil, httpx.NotFound("Order not found")
	}
	o, err := s.Commerce.SetStatus(ctx, who.BrandID, seq, in.Status, commerce.StudioOrderCarrier{Carrier: in.Carrier, Tracking: in.TrackingCode})
	if err != nil {
		return nil, err
	}
	return s.studioOrder(ctx, s.DB, o)
}
