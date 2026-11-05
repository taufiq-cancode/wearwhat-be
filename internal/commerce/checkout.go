package commerce

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"wearwhat-be/internal/db"
	"wearwhat-be/internal/domain"
	"wearwhat-be/internal/httpx"
	"wearwhat-be/internal/inbox"
	"wearwhat-be/internal/payments"
	"wearwhat-be/internal/secure"
	"wearwhat-be/internal/views"
)

// checkoutTTL is how long stock stays reserved while a customer pays.
const checkoutTTL = 30 * time.Minute

type checkoutInput struct {
	Shipping        string `json:"shipping"`
	Payment         string `json:"payment"`
	AddressID       string `json:"addressId"`
	PaymentMethodID string `json:"paymentMethodId"`
}

func (s *Service) checkout(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	var in checkoutInput
	if err := httpx.Decode(r, &in); err != nil {
		return nil, err
	}
	if in.Shipping != "express" {
		in.Shipping = "standard"
	}
	allowed := false
	for _, m := range s.Payments.Methods() {
		allowed = allowed || m == in.Payment
	}
	if !allowed {
		return nil, httpx.BadRequest("Choose how to pay")
	}
	if err := httpx.Limit(ctx, s.DB, "checkout:"+who.UserID, 10, time.Minute, "Too many checkout attempts — wait a moment"); err != nil {
		return nil, err
	}

	var email string
	var number string
	var chargeAmount int64
	var chargeCurrency string
	var savedAuth string
	var total int
	var orderIDs []string

	err := db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		lines, err := s.loadLines(ctx, tx, who.UserID, true)
		if err != nil {
			return err
		}
		if len(lines) == 0 {
			return httpx.BadRequest("Your bag is empty")
		}
		var userEmail, userPhone *string
		if err := tx.QueryRow(ctx, `SELECT email, phone FROM users WHERE id = $1`, who.UserID).Scan(&userEmail, &userPhone); err != nil {
			return err
		}
		email = receiptAddress(userEmail, userPhone)

		shipTo, err := s.resolveAddress(ctx, tx, who.UserID, in.AddressID)
		if err != nil {
			return err
		}
		if in.PaymentMethodID != "" {
			var enc string
			err := tx.QueryRow(ctx, `SELECT auth_enc FROM payment_methods WHERE id = $1 AND user_id = $2 AND provider = $3`,
				in.PaymentMethodID, who.UserID, s.Payments.Name()).Scan(&enc)
			if db.IsNotFound(err) {
				return httpx.BadRequest("That payment method isn’t available any more")
			}
			if err != nil {
				return err
			}
			if savedAuth, err = secure.Decrypt(s.Cfg.DataKey, enc); err != nil {
				return fmt.Errorf("decrypt saved card: %w", err)
			}
			in.Payment = "card"
		}

		// availability is checked again now: anything hidden since it was added can't be bought
		for _, l := range lines {
			if l.Product.Summary.ID == "" {
				return httpx.Conflict("Something in your bag isn’t available any more")
			}
			var visible bool
			if err := tx.QueryRow(ctx, `SELECT `+views.ProductVisible+` FROM products p JOIN brands b ON b.id = p.brand_id WHERE p.id = $1`, l.ProductID).Scan(&visible); err != nil {
				return err
			}
			if !visible {
				return httpx.Conflict(l.Product.Summary.Name + " isn’t available any more")
			}
		}

		// reserving stock is one conditional update per line, so two buyers can never both
		// take the last unit: the second update matches no row and the whole checkout rolls back
		for _, l := range lines {
			tag, err := tx.Exec(ctx, `UPDATE product_sizes SET stock = stock - $3 WHERE product_id = $1 AND label = $2 AND stock >= $3`, l.ProductID, l.Size, l.Qty)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return httpx.Conflict(fmt.Sprintf("%s in %s just sold out", l.Product.Summary.Name, l.Size))
			}
		}

		// the server's own totals: whatever the client displayed is irrelevant
		applied, promoCode, promoBrandFunded, err := s.redeemPromo(ctx, tx, who.UserID, lines)
		if err != nil {
			return err
		}
		subtotals := make([]int, len(lines))
		subtotal := 0
		for i, l := range lines {
			subtotals[i] = l.subtotal()
			subtotal += subtotals[i]
		}
		shipping := domain.ShippingCost(subtotal, in.Shipping, applied.freeShipping)
		discount := applied.total()
		total = subtotal - discount + shipping
		perShipping := domain.Allocate(shipping, subtotals)

		chargeCurrency = s.Cfg.PaymentCurrency
		chargeAmount = int64(math.Round(float64(total) * s.Cfg.FXRate))
		now := s.Now()

		var seq int64
		if err := tx.QueryRow(ctx, `SELECT nextval('checkout_seq')`).Scan(&seq); err != nil {
			return err
		}
		number = fmt.Sprintf("WC-%d", seq)
		if _, err := tx.Exec(ctx, `INSERT INTO checkouts (number, user_id, subtotal, discount, shipping, total, charge_amount, charge_currency, status,
				shipping_speed, payment_method, provider, provider_ref, promo_code, ship_to, email, created_at, expires_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'pending',$9,$10,$11,$1,$12,$13,$14,$15,$16)`,
			number, who.UserID, subtotal, discount, shipping, total, chargeAmount, chargeCurrency,
			in.Shipping, in.Payment, s.Payments.Name(), promoCode, shipTo, email, now, now.Add(checkoutTTL)); err != nil {
			return err
		}

		orderIDs = make([]string, 0, len(lines))
		for i, l := range lines {
			money := domain.LineMoney{Subtotal: subtotals[i], Discount: applied.perLine[i], Shipping: perShipping[i]}
			if promoBrandFunded {
				money.BrandDiscount = applied.perLine[i]
			}
			money.Settle()
			tried, err := triedOn(ctx, tx, who.UserID, l.ProductID, now)
			if err != nil {
				return err
			}
			var orderSeq int64
			if err := tx.QueryRow(ctx, `INSERT INTO orders (checkout_number, user_id, brand_id, product_id, size, color, qty, unit_price, subtotal, discount, brand_discount,
					shipping, fee, payout, status, from_try_on, source_post_id, ship_to, shipping_speed, placed_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20) RETURNING seq`,
				number, who.UserID, l.Product.Summary.Brand.ID, l.ProductID, l.Size, l.Color, l.Qty, l.Product.Summary.Price, money.Subtotal, money.Discount, money.BrandDiscount,
				money.Shipping, money.Fee, money.Payout, domain.StatusPending, tried, l.SourcePost, shipTo, in.Shipping, now).Scan(&orderSeq); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO order_events (order_seq, stage, at) VALUES ($1,'placed',$2)`, orderSeq, now); err != nil {
				return err
			}
			orderIDs = append(orderIDs, domain.ShopperOrderID(orderSeq))
		}
		if _, err := tx.Exec(ctx, `DELETE FROM cart_lines WHERE user_id = $1`, who.UserID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM cart_promos WHERE user_id = $1`, who.UserID)
		return err
	})
	if err != nil {
		return nil, err
	}

	// the provider call happens after commit: a slow payment API must not hold database locks
	result, err := s.Payments.Init(ctx, payments.InitRequest{
		Reference:   number,
		Email:       email,
		Amount:      chargeAmount,
		Currency:    chargeCurrency,
		Method:      in.Payment,
		CallbackURL: s.Cfg.Origin() + "/orders/" + number + "/confirmed",
		SavedAuth:   savedAuth,
	})
	if err != nil {
		s.Log.Error("start payment", "number", number, "err", err)
		if cerr := s.cancelCheckout(ctx, number, "failed"); cerr != nil {
			s.Log.Error("cancel checkout", "number", number, "err", cerr)
		}
		return nil, httpx.Err(http.StatusBadGateway, "We couldn’t start the payment and you haven’t been charged. Your bag is restored — try again.")
	}
	out := domain.Checkout{Number: number, Total: total, Email: visibleEmail(email), OrderIDs: orderIDs, Status: "pending", TestMode: s.Payments.Test()}
	switch {
	case result.Paid:
		if err := s.MarkPaid(ctx, number, result); err != nil {
			return nil, err
		}
		out.Status = "paid"
	case result.Failed:
		if err := s.cancelCheckout(ctx, number, "failed"); err != nil {
			return nil, err
		}
		return nil, httpx.Err(http.StatusPaymentRequired, "That payment was declined. Your bag is restored — try another way to pay.")
	case result.RedirectURL != "":
		if _, err := s.DB.Exec(ctx, `UPDATE checkouts SET redirect_url = $2 WHERE number = $1`, number, result.RedirectURL); err != nil {
			return nil, err
		}
		out.RedirectURL = result.RedirectURL
	}
	return out, nil
}

// receiptAddress: Paystack requires an email, so phone-only accounts get an address nobody reads.
func receiptAddress(email, phone *string) string {
	if email != nil && *email != "" {
		return *email
	}
	if phone != nil {
		return strings.TrimPrefix(*phone, "+") + "@phone.wearwhat.invalid"
	}
	return "customer@phone.wearwhat.invalid"
}

func visibleEmail(address string) string {
	if strings.HasSuffix(address, ".invalid") {
		return "your account"
	}
	return address
}

func (s *Service) resolveAddress(ctx context.Context, q db.Q, userID, addressID string) (domain.Address, error) {
	var a domain.Address
	var err error
	if addressID != "" {
		err = q.QueryRow(ctx, `SELECT name, line1, city, phone FROM addresses WHERE id = $1 AND user_id = $2`, addressID, userID).Scan(&a.Name, &a.Line1, &a.City, &a.Phone)
	} else {
		err = q.QueryRow(ctx, `SELECT name, line1, city, phone FROM addresses WHERE user_id = $1 AND is_default`, userID).Scan(&a.Name, &a.Line1, &a.City, &a.Phone)
	}
	if db.IsNotFound(err) {
		return a, httpx.BadRequest("Add a delivery address first")
	}
	return a, err
}

// redeemPromo re-validates the bag's promo under a row lock and counts the redemption, so a
// single-use code can't be spent twice by two checkouts running at once.
func (s *Service) redeemPromo(ctx context.Context, tx pgx.Tx, userID string, lines []line) (appliedPromo, *string, bool, error) {
	none := appliedPromo{perLine: make([]int, len(lines))}
	var code string
	err := tx.QueryRow(ctx, `SELECT code FROM cart_promos WHERE user_id = $1`, userID).Scan(&code)
	if db.IsNotFound(err) {
		return none, nil, false, nil
	}
	if err != nil {
		return none, nil, false, err
	}
	p, err := loadPromo(ctx, tx, code, true)
	if db.IsNotFound(err) {
		return none, nil, false, nil
	}
	if err != nil {
		return none, nil, false, err
	}
	applied := p.apply(lines)
	if !p.usable(s.Now(), userID) || !applied.eligible {
		return none, nil, false, httpx.Conflict("Promo " + code + " can’t be used any more — remove it from your bag")
	}
	if _, err := tx.Exec(ctx, `UPDATE promotions SET uses = uses + 1 WHERE code = $1`, code); err != nil {
		return none, nil, false, err
	}
	return applied, &code, p.BrandID != nil, nil
}

// errAmountMismatch means the provider reports a charge that isn't what we asked for.
var errAmountMismatch = errors.New("charge doesn't match the checkout")

// MarkPaid confirms a checkout exactly once, however many times payment news arrives.
// A charge for the wrong amount never fulfils the order: it's flagged for a person and the
// stock and bag are released, since retrying can't change what was charged.
func (s *Service) MarkPaid(ctx context.Context, number string, res payments.Result) error {
	err := s.markPaid(ctx, number, res)
	if errors.Is(err, errAmountMismatch) {
		s.Log.Error("payment amount mismatch: needs manual review", "number", number, "err", err)
		return s.cancelCheckout(ctx, number, "amount_mismatch")
	}
	return err
}

func (s *Service) markPaid(ctx context.Context, number string, res payments.Result) error {
	type receipt struct {
		email, name string
		total       int
		lines       []string
	}
	var mail receipt
	var sendMail bool
	err := db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		var status, currency, userID, email string
		var amount int64
		var total int
		err := tx.QueryRow(ctx, `SELECT status, charge_currency, charge_amount, user_id, email, total FROM checkouts WHERE number = $1 FOR UPDATE`, number).
			Scan(&status, &currency, &amount, &userID, &email, &total)
		if db.IsNotFound(err) {
			return nil // a reference that isn't ours
		}
		if err != nil {
			return err
		}
		if status == "paid" {
			return nil
		}
		// never trust that a charge matches what we asked for
		if res.Amount != amount || !strings.EqualFold(res.Currency, currency) {
			return fmt.Errorf("%w: paid %d %s but expected %d %s", errAmountMismatch, res.Amount, res.Currency, amount, currency)
		}
		if status != "pending" {
			// the stock was already released, so this needs a person to refund or re-fulfil
			s.Log.Error("payment arrived after checkout closed", "number", number, "status", status)
			_, err := tx.Exec(ctx, `UPDATE checkouts SET status = 'paid_after_expiry', paid_at = $2 WHERE number = $1`, number, s.Now())
			return err
		}
		now := s.Now()
		if _, err := tx.Exec(ctx, `UPDATE checkouts SET status = 'paid', paid_at = $2 WHERE number = $1`, number, now); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `UPDATE orders SET status = $2, placed_at = $3 WHERE checkout_number = $1 AND status = $4
			RETURNING seq, brand_id, product_id, size, qty`, number, domain.StatusToFulfil, now, domain.StatusPending)
		if err != nil {
			return err
		}
		type paidLine struct {
			seq              int64
			brand, prod, sz string
			qty              int
		}
		var paid []paidLine
		for rows.Next() {
			var l paidLine
			if err := rows.Scan(&l.seq, &l.brand, &l.prod, &l.sz, &l.qty); err != nil {
				rows.Close()
				return err
			}
			paid = append(paid, l)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		var customer string
		if err := tx.QueryRow(ctx, `SELECT name FROM users WHERE id = $1`, userID).Scan(&customer); err != nil {
			return err
		}
		mail = receipt{email: email, name: customer, total: total}
		for _, l := range paid {
			if _, err := tx.Exec(ctx, `INSERT INTO order_events (order_seq, stage, at) VALUES ($1,'paid',$2)`, l.seq, now); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO closet_items (user_id, product_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, userID, l.prod); err != nil {
				return err
			}
			var name string
			var images []domain.Photo
			if err := tx.QueryRow(ctx, `SELECT name, images FROM products WHERE id = $1`, l.prod).Scan(&name, &images); err != nil {
				return err
			}
			var thumb *domain.Photo
			if len(images) > 0 {
				thumb = &images[0]
			}
			mail.lines = append(mail.lines, fmt.Sprintf("%s · %s · qty %d", name, l.sz, l.qty))
			if err := inbox.Notify(ctx, tx, inbox.Note{
				BrandID: l.brand, Who: fmt.Sprintf("Order #%d", l.seq), Text: fmt.Sprintf("%s bought %s · %s.", customer, name, l.sz),
				Kind: "Orders", Thumb: thumb, Href: fmt.Sprintf("/studio/orders/%d", l.seq), Pref: "brandOrders",
			}); err != nil {
				return err
			}
		}
		sendMail = true
		if res.Card != nil && res.Card.Reusable {
			return s.saveCard(ctx, tx, userID, res.Card)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if sendMail && !strings.HasSuffix(mail.email, ".invalid") {
		body := fmt.Sprintf("Thanks %s — your order %s is confirmed.\n\n%s\n\nTotal: $%.2f\n\nTrack it any time from Orders in the app.",
			mail.name, number, strings.Join(mail.lines, "\n"), float64(mail.total)/100)
		s.SendMail(mail.email, "Order "+number+" confirmed", body)
	}
	return nil
}

func (s *Service) saveCard(ctx context.Context, q db.Q, userID string, c *payments.Card) error {
	enc, err := secure.Encrypt(s.Cfg.DataKey, c.AuthCode)
	if err != nil {
		return err
	}
	brand := c.Brand
	if brand != "" {
		brand = strings.ToUpper(brand[:1]) + brand[1:]
	}
	fingerprint := c.Fingerprint
	if fingerprint == "" {
		fingerprint = c.Last4 + c.Exp
	}
	var hasDefault bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM payment_methods WHERE user_id = $1 AND is_default)`, userID).Scan(&hasDefault); err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO payment_methods (id, user_id, provider, label, detail, auth_enc, fingerprint, is_default)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (user_id, provider, fingerprint) DO UPDATE SET auth_enc = EXCLUDED.auth_enc, detail = EXCLUDED.detail`,
		secure.ID("pm"), userID, s.Payments.Name(), fmt.Sprintf("%s •••• %s", brand, c.Last4), "exp "+c.Exp, enc, fingerprint, !hasDefault)
	return err
}

// cancelCheckout releases a checkout that was never paid: stock returns to the shelf and the
// pieces return to the bag, so nobody has to rebuild it.
func (s *Service) cancelCheckout(ctx context.Context, number, status string) error {
	return db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		var current, userID string
		var promo *string
		err := tx.QueryRow(ctx, `SELECT status, user_id, promo_code FROM checkouts WHERE number = $1 FOR UPDATE`, number).Scan(&current, &userID, &promo)
		if db.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if current != "pending" {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE checkouts SET status = $2 WHERE number = $1`, number, status); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `UPDATE orders SET status = $2 WHERE checkout_number = $1 AND status = $3
			RETURNING product_id, size, color, qty, source_post_id`, number, domain.StatusCancelled, domain.StatusPending)
		if err != nil {
			return err
		}
		type released struct {
			product, size, color string
			qty                  int
			post                 *string
		}
		var list []released
		for rows.Next() {
			var l released
			if err := rows.Scan(&l.product, &l.size, &l.color, &l.qty, &l.post); err != nil {
				rows.Close()
				return err
			}
			list = append(list, l)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, l := range list {
			if _, err := tx.Exec(ctx, `UPDATE product_sizes SET stock = stock + $3 WHERE product_id = $1 AND label = $2`, l.product, l.size, l.qty); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO cart_lines (id, user_id, product_id, size, color, qty, source_post_id) VALUES ($1,$2,$3,$4,$5,$6,$7)
				ON CONFLICT (user_id, product_id, size, color) DO UPDATE SET qty = LEAST(cart_lines.qty + EXCLUDED.qty, $8)`,
				secure.ID("l"), userID, l.product, l.size, l.color, l.qty, l.post, maxLineQty); err != nil {
				return err
			}
		}
		if promo != nil {
			if _, err := tx.Exec(ctx, `UPDATE promotions SET uses = GREATEST(uses - 1, 0) WHERE code = $1`, *promo); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO cart_promos (user_id, code) VALUES ($1,$2) ON CONFLICT (user_id) DO UPDATE SET code = $2`, userID, *promo); err != nil {
				return err
			}
		}
		return nil
	})
}

// ExpireCheckouts closes unpaid checkouts past their window, checking the provider first in
// case a webhook went missing.
func (s *Service) ExpireCheckouts(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, `SELECT number, provider FROM checkouts WHERE status = 'pending' AND expires_at < $1 ORDER BY expires_at LIMIT 100`, s.Now())
	if err != nil {
		return err
	}
	type due struct{ number, provider string }
	var list []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.number, &d.provider); err != nil {
			rows.Close()
			return err
		}
		list = append(list, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, d := range list {
		if d.provider == s.Payments.Name() && !s.Payments.Test() {
			res, err := s.Payments.Verify(ctx, d.number)
			if err != nil {
				s.Log.Warn("verify expiring checkout", "number", d.number, "err", err)
				continue // try again next tick rather than release stock someone may have paid for
			}
			if res.Paid {
				if err := s.MarkPaid(ctx, d.number, res); err != nil {
					s.Log.Error("mark paid", "number", d.number, "err", err)
				}
				continue
			}
		}
		if err := s.cancelCheckout(ctx, d.number, "expired"); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) getCheckout(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	number := r.PathValue("number")
	load := func() (domain.Checkout, string, error) {
		var c domain.Checkout
		var provider string
		err := s.DB.QueryRow(ctx, `SELECT number, total, email, status, redirect_url, provider FROM checkouts WHERE number = $1 AND user_id = $2`, number, who.UserID).
			Scan(&c.Number, &c.Total, &c.Email, &c.Status, &c.RedirectURL, &provider)
		return c, provider, err
	}
	c, provider, err := load()
	if db.IsNotFound(err) {
		return nil, httpx.NotFound("Order not found")
	}
	if err != nil {
		return nil, err
	}
	// returning from the provider's page: ask the provider, since the redirect itself proves nothing
	if c.Status == "pending" && provider == s.Payments.Name() && !s.Payments.Test() {
		res, err := s.Payments.Verify(ctx, number)
		if err != nil {
			s.Log.Warn("verify checkout", "number", number, "err", err)
		} else if res.Paid {
			if err := s.MarkPaid(ctx, number, res); err != nil {
				return nil, err
			}
		} else if res.Failed {
			if err := s.cancelCheckout(ctx, number, "failed"); err != nil {
				return nil, err
			}
		}
		if c, _, err = load(); err != nil {
			return nil, err
		}
	}
	c.Email = visibleEmail(c.Email)
	c.TestMode = provider == "test"
	rows, err := s.DB.Query(ctx, `SELECT seq FROM orders WHERE checkout_number = $1 ORDER BY seq`, number)
	if err != nil {
		return nil, err
	}
	seqs, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, err
	}
	c.OrderIDs = make([]string, 0, len(seqs))
	for _, n := range seqs {
		c.OrderIDs = append(c.OrderIDs, domain.ShopperOrderID(n))
	}
	if c.Status != "pending" {
		c.RedirectURL = ""
	}
	return c, nil
}

// paystackWebhook authenticates by signature (no session), then confirms with Paystack itself.
func (s *Service) paystackWebhook(rt *httpx.Router) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		ref, err := s.Payments.ParseWebhook(body, r.Header.Get("x-paystack-signature"))
		if errors.Is(err, payments.ErrBadSignature) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if ref != "" {
			res, err := s.Payments.Verify(r.Context(), ref)
			if err != nil {
				rt.Log.Error("verify webhook charge", "err", err)
				http.Error(w, "try again", http.StatusBadGateway) // Paystack retries
				return
			}
			if res.Paid {
				if err := s.MarkPaid(r.Context(), ref, res); err != nil {
					rt.Log.Error("mark paid from webhook", "err", err)
					http.Error(w, "try again", http.StatusInternalServerError)
					return
				}
			}
		}
		w.WriteHeader(http.StatusOK)
	}
}
