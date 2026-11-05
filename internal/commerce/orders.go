package commerce

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
	"wearwhat-be/internal/secure"
	"wearwhat-be/internal/views"
)

// OrderRow is one stored order, the single record both the shopper and the brand see.
type OrderRow struct {
	Seq                          int64
	UserID, BrandID, ProductID   string
	Size, Color                  string
	Qty, UnitPrice               int
	Subtotal, Discount, Shipping int
	Fee, Payout                  int
	Status, Carrier, Tracking    string
	ReturnNote                   string
	FromTryOn                    bool
	ShipTo                       domain.Address
	Speed                        string
	PlacedAt                     time.Time
	ShippedAt, DeliveredAt       *time.Time
	ReturnAt                     *time.Time
}

const OrderCols = `o.seq, o.user_id, o.brand_id, o.product_id, o.size, o.color, o.qty, o.unit_price, o.subtotal, o.discount, o.shipping,
	o.fee, o.payout, o.status, o.carrier, o.tracking_code, o.return_note, o.from_try_on, o.ship_to, o.shipping_speed,
	o.placed_at, o.shipped_at, o.delivered_at, o.return_requested_at`

// ScanOrder reads a row selected with OrderCols.
func ScanOrder(row pgx.Row) (OrderRow, error) {
	var o OrderRow
	err := row.Scan(&o.Seq, &o.UserID, &o.BrandID, &o.ProductID, &o.Size, &o.Color, &o.Qty, &o.UnitPrice, &o.Subtotal, &o.Discount, &o.Shipping,
		&o.Fee, &o.Payout, &o.Status, &o.Carrier, &o.Tracking, &o.ReturnNote, &o.FromTryOn, &o.ShipTo, &o.Speed,
		&o.PlacedAt, &o.ShippedAt, &o.DeliveredAt, &o.ReturnAt)
	return o, err
}

// productAtPurchase shows the piece at the price this order actually paid.
func productAtPurchase(sum domain.ProductSummary, o OrderRow) domain.ProductSummary {
	sum.Price = o.UnitPrice
	return sum
}

/* ---------- the shopper's view ---------- */

func (s *Service) shopperOrder(ctx context.Context, o OrderRow, sum domain.ProductSummary, ship domain.ShippingSettings) (domain.ShopperOrder, error) {
	out := domain.ShopperOrder{
		ID:      domain.ShopperOrderID(o.Seq),
		Product: productAtPurchase(sum, o),
		Size:    o.Size,
		Color:   o.Color,
		Qty:     o.Qty,
		Status:  domain.ShopperStatus(o.Status),
		When:    s.whenLabel(o, ship),
	}
	out.ReturnStatus = domain.ReturnStatus(o.Status)
	if o.Status == domain.StatusDelivered && o.DeliveredAt != nil {
		days := domain.ReturnWindowDays(ship.Returns)
		out.CanReturn = days > 0 && s.Now().Before(o.DeliveredAt.Add(time.Duration(days)*24*time.Hour))
	}
	if o.DeliveredAt != nil {
		var reviewed bool
		if err := s.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM reviews WHERE order_seq = $1)`, o.Seq).Scan(&reviewed); err != nil {
			return out, err
		}
		out.CanReview = !reviewed
	}
	return out, nil
}

func (s *Service) whenLabel(o OrderRow, ship domain.ShippingSettings) string {
	switch o.Status {
	case domain.StatusToFulfil:
		if ship.Processing != "" {
			return "ships in " + ship.Processing
		}
		return "preparing"
	case domain.StatusShipped:
		return "arrives " + arrivalDay(o)
	case domain.StatusOutForDelivery:
		return "arriving today"
	}
	if o.DeliveredAt != nil {
		return domain.ShortDate(*o.DeliveredAt)
	}
	return domain.ShortDate(o.PlacedAt)
}

// arrivalDay estimates delivery from the promised speed: next day express, 3 days standard.
func arrivalDay(o OrderRow) string {
	from := o.PlacedAt
	if o.ShippedAt != nil {
		from = *o.ShippedAt
	}
	days := 3
	if o.Speed == "express" {
		days = 1
	}
	return domain.Weekday(from.AddDate(0, 0, days))
}

func (s *Service) myOrders(_ http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	rows, err := s.DB.Query(ctx, `SELECT `+OrderCols+` FROM orders o WHERE o.user_id = $1 AND `+views.PaidOrder+` ORDER BY o.placed_at DESC, o.seq DESC LIMIT 200`, httpx.Who(r).UserID)
	if err != nil {
		return nil, err
	}
	list := []OrderRow{}
	ids := []string{}
	for rows.Next() {
		o, err := ScanOrder(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, o)
		ids = append(ids, o.ProductID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	summaries, err := views.Summaries(ctx, s.DB, ids)
	if err != nil {
		return nil, err
	}
	shipping := map[string]domain.ShippingSettings{}
	out := make([]domain.ShopperOrder, 0, len(list))
	for _, o := range list {
		ship, ok := shipping[o.BrandID]
		if !ok {
			if ship, err = views.BrandShipping(ctx, s.DB, o.BrandID); err != nil {
				return nil, err
			}
			shipping[o.BrandID] = ship
		}
		item, err := s.shopperOrder(ctx, o, summaries[o.ProductID], ship)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}

// ownOrder finds an order only for the person who placed it; anyone else sees a 404.
func (s *Service) ownOrder(ctx context.Context, q db.Q, userID, id string) (OrderRow, error) {
	seq, ok := domain.ParseOrderID(id)
	if !ok {
		return OrderRow{}, httpx.NotFound("Order not found")
	}
	o, err := ScanOrder(q.QueryRow(ctx, `SELECT `+OrderCols+` FROM orders o WHERE o.seq = $1 AND o.user_id = $2 AND `+views.PaidOrder, seq, userID))
	if db.IsNotFound(err) {
		return o, httpx.NotFound("Order not found")
	}
	return o, err
}

func (s *Service) getOrder(_ http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	o, err := s.ownOrder(ctx, s.DB, httpx.Who(r).UserID, r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	p, err := views.ProductByID(ctx, s.DB, o.ProductID)
	if err != nil {
		return nil, err
	}
	ship, err := views.BrandShipping(ctx, s.DB, o.BrandID)
	if err != nil {
		return nil, err
	}
	base, err := s.shopperOrder(ctx, o, p.Summary, ship)
	if err != nil {
		return nil, err
	}
	events, err := s.trackingEvents(ctx, o, p.Summary.Brand.Name, ship)
	if err != nil {
		return nil, err
	}
	stage := domain.ShopperStage(o.Status)
	head, sub := headline(o, p.Summary.Brand.Name, ship)
	out := domain.ShopperOrderDetail{
		ShopperOrder: base,
		Stage:        stage,
		Headline:     head,
		Subline:      sub,
		Carrier:      "Courier assigned at dispatch",
		Events:       events,
		ShipTo:       o.ShipTo,
		Total:        o.Subtotal - o.Discount + o.Shipping,
	}
	if o.Carrier != "" {
		out.Carrier = o.Carrier
	}
	if o.Tracking != "" {
		code := o.Tracking
		out.TrackingCode = &code
	}
	return out, nil
}

func headline(o OrderRow, brand string, ship domain.ShippingSettings) (string, string) {
	switch o.Status {
	case domain.StatusToFulfil:
		sub := brand + " is preparing it"
		if ship.Processing != "" {
			sub = brand + " ships within " + ship.Processing
		}
		return "Preparing your order", sub
	case domain.StatusShipped:
		return "Arriving " + arrivalDay(o), "In transit"
	case domain.StatusOutForDelivery:
		return "Arriving today", "Out for delivery"
	case domain.StatusReturnReq:
		return "Return requested", "Waiting on " + brand
	case domain.StatusReturnOK:
		return "Return approved", brand + " will be in touch with the next steps"
	case domain.StatusReturnNo:
		return "Return declined", "Message " + brand + " if you have questions"
	}
	when := ""
	if o.DeliveredAt != nil {
		when = domain.ShortDate(*o.DeliveredAt)
	}
	return "Delivered", when
}

// trackingEvents builds the timeline from what really happened, newest first, with the
// stages still to come shown as pending.
func (s *Service) trackingEvents(ctx context.Context, o OrderRow, brand string, ship domain.ShippingSettings) ([]domain.TrackingEvent, error) {
	rows, err := s.DB.Query(ctx, `SELECT stage, detail, at FROM order_events WHERE order_seq = $1 ORDER BY at, id`, o.Seq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type ev struct {
		detail string
		at     time.Time
	}
	happened := map[string]ev{}
	for rows.Next() {
		var stage string
		var e ev
		if err := rows.Scan(&stage, &e.detail, &e.at); err != nil {
			return nil, err
		}
		happened[stage] = e
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	from := brand
	if ship.ShipsFrom != "" {
		from = brand + " · " + ship.ShipsFrom
	}
	carrier := o.Carrier
	stages := []struct{ key, title, detail string }{
		{"paid", "Order confirmed", "Payment received · " + brand + " notified"},
		{"shipped", "Packed & handed to courier", from},
		{"out_for_delivery", "Out for delivery", carrier},
		{"delivered", "Delivered", "Delivered to " + o.ShipTo.City},
	}
	switch o.Status {
	case domain.StatusReturnReq, domain.StatusReturnOK, domain.StatusReturnNo:
		stages = append(stages, struct{ key, title, detail string }{"return_requested", "Return requested", o.ReturnNote})
	}
	if o.Status == domain.StatusReturnOK {
		stages = append(stages, struct{ key, title, detail string }{"return_approved", "Return approved", brand + " will be in touch"})
	}
	if o.Status == domain.StatusReturnNo {
		stages = append(stages, struct{ key, title, detail string }{"return_declined", "Return declined", ""})
	}

	out := make([]domain.TrackingEvent, 0, len(stages))
	current := true
	for i := len(stages) - 1; i >= 0; i-- {
		st := stages[i]
		e, done := happened[st.key]
		// an order that skipped "out for delivery" shouldn't show it as forever pending
		if !done && st.key == "out_for_delivery" && o.DeliveredAt != nil {
			continue
		}
		// a step still to come has no detail yet: "Delivered to Lagos" would read as already done
		item := domain.TrackingEvent{Title: st.title, Time: "Pending", Done: done}
		if done {
			item.Time = domain.DayTime(e.at)
			item.Detail = st.detail
			if e.detail != "" {
				item.Detail = e.detail
			}
			item.Current = current
			current = false
		}
		out = append(out, item)
	}
	return out, nil
}

/* ---------- returns & reviews ---------- */

func (s *Service) requestReturn(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	var in struct {
		Note string `json:"note"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return nil, err
	}
	note := strings.TrimSpace(in.Note)
	if note == "" {
		return nil, httpx.BadRequest("Say why you’re returning it")
	}
	if len([]rune(note)) > 500 {
		return nil, httpx.BadRequest("Keep the reason under 500 characters")
	}
	err := db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		o, err := s.ownOrder(ctx, tx, who.UserID, r.PathValue("id"))
		if err != nil {
			return err
		}
		if o.Status != domain.StatusDelivered || o.DeliveredAt == nil {
			return httpx.Conflict("This order can’t be returned right now")
		}
		ship, err := views.BrandShipping(ctx, tx, o.BrandID)
		if err != nil {
			return err
		}
		days := domain.ReturnWindowDays(ship.Returns)
		if days == 0 {
			return httpx.Conflict("This brand doesn’t accept returns")
		}
		now := s.Now()
		if now.After(o.DeliveredAt.Add(time.Duration(days) * 24 * time.Hour)) {
			return httpx.Conflict(fmt.Sprintf("The %d-day return window has closed", days))
		}
		if _, err := tx.Exec(ctx, `UPDATE orders SET status = $2, return_note = $3, return_requested_at = $4 WHERE seq = $1`, o.Seq, domain.StatusReturnReq, note, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO order_events (order_seq, stage, detail, at) VALUES ($1,'return_requested',$2,$3)`, o.Seq, note, now); err != nil {
			return err
		}
		var name string
		if err := tx.QueryRow(ctx, `SELECT name FROM products WHERE id = $1`, o.ProductID).Scan(&name); err != nil {
			return err
		}
		return inbox.Notify(ctx, tx, inbox.Note{
			BrandID: o.BrandID, Who: "Return", Text: fmt.Sprintf("#%d · %s — “%s”.", o.Seq, name, shorten(note, 60)), Kind: "Orders",
			Href: fmt.Sprintf("/studio/orders/%d", o.Seq), Pref: "brandReturns",
		})
	})
	return nil, err
}

func shorten(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func (s *Service) addReview(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	var in struct {
		OrderID string `json:"orderId"`
		Rating  int    `json:"rating"`
		Text    string `json:"text"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return nil, err
	}
	if in.Rating < 1 || in.Rating > 5 {
		return nil, httpx.BadRequest("Give it 1 to 5 stars")
	}
	text := strings.TrimSpace(in.Text)
	if len([]rune(text)) > 1000 {
		return nil, httpx.BadRequest("Keep your review under 1000 characters")
	}
	o, err := s.ownOrder(ctx, s.DB, who.UserID, in.OrderID)
	if err != nil {
		return nil, err
	}
	if o.ProductID != r.PathValue("id") {
		return nil, httpx.BadRequest("That order isn’t for this piece")
	}
	if o.DeliveredAt == nil {
		return nil, httpx.Conflict("You can review a piece once it’s delivered")
	}
	review := domain.Review{ID: secure.ID("rv"), Author: who.Handle, Rating: in.Rating, Text: text}
	review.Meta = "Size " + o.Size + " · " + strings.Repeat("★", in.Rating)
	_, err = s.DB.Exec(ctx, `INSERT INTO reviews (id, product_id, user_id, order_seq, rating, text, size, created_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		review.ID, o.ProductID, who.UserID, o.Seq, in.Rating, text, o.Size, s.Now())
	if db.IsUnique(err) {
		return nil, httpx.Conflict("You’ve already reviewed this order")
	}
	if err != nil {
		return nil, err
	}
	return review, nil
}

/* ---------- status changes, called from the studio ---------- */

// StudioOrderCarrier is the optional shipping detail given when an order ships.
type StudioOrderCarrier struct {
	Carrier  string
	Tracking string
}

// SetStatus moves a brand's order along its allowed flow and tells the buyer.
func (s *Service) SetStatus(ctx context.Context, brandID string, seq int64, to string, ship StudioOrderCarrier) (OrderRow, error) {
	var o OrderRow
	var buyerEmail *string
	var productName string
	var brandName string
	var ordersPref bool
	err := db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		var err error
		o, err = ScanOrder(tx.QueryRow(ctx, `SELECT `+OrderCols+` FROM orders o WHERE o.seq = $1 AND o.brand_id = $2 AND `+views.PaidOrder+` FOR UPDATE`, seq, brandID))
		if db.IsNotFound(err) {
			return httpx.NotFound("Order not found")
		}
		if err != nil {
			return err
		}
		if !domain.CanTransition(o.Status, to) {
			return httpx.Conflict(fmt.Sprintf("An order that’s %s can’t move to %s", strings.ToLower(o.Status), strings.ToLower(to)))
		}
		now := s.Now()
		stage := ""
		detail := ""
		switch to {
		case domain.StatusShipped:
			carrier := strings.TrimSpace(ship.Carrier)
			tracking := strings.TrimSpace(ship.Tracking)
			if len(carrier) > 80 || len(tracking) > 80 {
				return httpx.BadRequest("Carrier and tracking are too long")
			}
			if _, err := tx.Exec(ctx, `UPDATE orders SET status = $2, shipped_at = $3, carrier = $4, tracking_code = $5 WHERE seq = $1`, seq, to, now, carrier, tracking); err != nil {
				return err
			}
			o.Carrier, o.Tracking = carrier, tracking
			stage = "shipped"
		case domain.StatusOutForDelivery:
			if _, err := tx.Exec(ctx, `UPDATE orders SET status = $2 WHERE seq = $1`, seq, to); err != nil {
				return err
			}
			stage = "out_for_delivery"
		case domain.StatusDelivered:
			if _, err := tx.Exec(ctx, `UPDATE orders SET status = $2, delivered_at = $3 WHERE seq = $1`, seq, to, now); err != nil {
				return err
			}
			o.DeliveredAt = &now
			stage = "delivered"
		case domain.StatusReturnOK:
			// the buyer is owed their money back; moving it stays a deliberate manual step
			if _, err := tx.Exec(ctx, `UPDATE orders SET status = $2, refund_status = 'due' WHERE seq = $1`, seq, to); err != nil {
				return err
			}
			stage = "return_approved"
		case domain.StatusReturnNo:
			if _, err := tx.Exec(ctx, `UPDATE orders SET status = $2 WHERE seq = $1`, seq, to); err != nil {
				return err
			}
			stage = "return_declined"
		}
		o.Status = to
		if _, err := tx.Exec(ctx, `INSERT INTO order_events (order_seq, stage, detail, at) VALUES ($1,$2,$3,$4)`, seq, stage, detail, now); err != nil {
			return err
		}

		var images []domain.Photo
		if err := tx.QueryRow(ctx, `SELECT p.name, p.images, b.name FROM products p JOIN brands b ON b.id = p.brand_id WHERE p.id = $1`, o.ProductID).Scan(&productName, &images, &brandName); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT email, COALESCE((prefs ->> 'orders')::boolean, true) FROM users WHERE id = $1`, o.UserID).Scan(&buyerEmail, &ordersPref); err != nil {
			return err
		}
		var thumb, avatar *domain.Photo
		if len(images) > 0 {
			thumb = &images[0]
		}
		if err := tx.QueryRow(ctx, `SELECT avatar FROM brands WHERE id = $1`, brandID).Scan(&avatar); err != nil {
			return err
		}
		id := domain.ShopperOrderID(seq)
		return inbox.Notify(ctx, tx, inbox.Note{
			UserID: o.UserID, Who: brandName, Text: statusLine(id, productName, to), Kind: "Orders", Avatar: avatar, Thumb: thumb, Pref: "orders",
			Action: &domain.NotificationAction{Type: "track", Label: "TRACK", Target: id},
		})
	})
	if err != nil {
		return o, err
	}
	if buyerEmail != nil && ordersPref {
		s.SendMail(*buyerEmail, "Your "+brandName+" order: "+domain.ShopperStatus(to), statusLine(domain.ShopperOrderID(seq), productName, to)+
			trackingLine(o)+"\n\nTrack it any time from Orders in the app.")
	}
	return o, nil
}

func trackingLine(o OrderRow) string {
	if o.Status == domain.StatusShipped && o.Carrier != "" {
		line := "\n\nCarrier: " + o.Carrier
		if o.Tracking != "" {
			line += "\nTracking: " + o.Tracking
		}
		return line
	}
	return ""
}

func statusLine(id, product, status string) string {
	switch status {
	case domain.StatusShipped:
		return fmt.Sprintf("Order #%s (%s) has shipped.", id, product)
	case domain.StatusOutForDelivery:
		return fmt.Sprintf("Order #%s (%s) is out for delivery.", id, product)
	case domain.StatusDelivered:
		return fmt.Sprintf("Order #%s (%s) was delivered. Post a wear to earn 10%% off your next order.", id, product)
	case domain.StatusReturnOK:
		return fmt.Sprintf("Your return for order #%s (%s) was approved.", id, product)
	case domain.StatusReturnNo:
		return fmt.Sprintf("Your return for order #%s (%s) was declined.", id, product)
	}
	return fmt.Sprintf("Order #%s (%s) was updated.", id, product)
}
