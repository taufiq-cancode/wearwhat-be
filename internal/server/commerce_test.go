package server_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"wearwhat-be/internal/payments"
)

func TestCheckoutTotalsAreTheServers(t *testing.T) {
	e := newEnv(t)
	owner := e.brandOwner()
	cheap := owner.addProduct(5000, map[string]int{"M": 10})
	dear := owner.addProduct(20000, map[string]int{"M": 10})
	c := e.shopper().withAddress()

	c.ok("POST", "/cart/items", m{"productId": cheap, "size": "M", "color": "Black", "qty": 2}, nil)
	var res checkoutResult
	// the client can't influence the price: any extra fields are ignored
	c.ok("POST", "/checkout", m{"shipping": "standard", "payment": "card", "total": 1, "subtotal": 1}, &res)
	if res.Total != 10000+800 {
		t.Errorf("$50 × 2 + standard shipping: got %d", res.Total)
	}

	c.ok("POST", "/cart/items", m{"productId": dear, "size": "M", "color": "Black"}, nil)
	c.ok("POST", "/checkout", m{"shipping": "standard", "payment": "card"}, &res)
	if res.Total != 20000 {
		t.Errorf("over $150 ships free: got %d", res.Total)
	}

	c.ok("POST", "/cart/items", m{"productId": dear, "size": "M", "color": "Black"}, nil)
	c.ok("POST", "/checkout", m{"shipping": "express", "payment": "card"}, &res)
	if res.Total != 20000+1500 {
		t.Errorf("express is always charged: got %d", res.Total)
	}

	c.status(400, "POST", "/checkout", m{"shipping": "standard", "payment": "card"}) // empty bag
	c.ok("POST", "/cart/items", m{"productId": cheap, "size": "M", "color": "Black"}, nil)
	c.status(400, "POST", "/checkout", m{"shipping": "standard", "payment": "crypto"})
}

func TestCheckoutNeedsAnAddress(t *testing.T) {
	e := newEnv(t)
	owner := e.brandOwner()
	pid := owner.addProduct(5000, map[string]int{"M": 5})
	c := e.shopper()
	c.ok("POST", "/cart/items", m{"productId": pid, "size": "M", "color": "Black"}, nil)
	if msg := c.status(400, "POST", "/checkout", m{"shipping": "standard", "payment": "card"}); !strings.Contains(msg, "address") {
		t.Errorf("message: %q", msg)
	}
	// nothing was reserved by the failed attempt
	if n := e.count(`SELECT stock FROM product_sizes WHERE product_id = $1`, pid); n != 5 {
		t.Errorf("stock %d", n)
	}
	// someone else's address can't be used
	other := e.shopper().withAddress()
	var addrs []struct {
		ID string `json:"id"`
	}
	other.ok("GET", "/me/addresses", nil, &addrs)
	c.status(400, "POST", "/checkout", m{"shipping": "standard", "payment": "card", "addressId": addrs[0].ID})
}

func TestCartValidation(t *testing.T) {
	e := newEnv(t)
	owner := e.brandOwner()
	pid := owner.addProduct(5000, map[string]int{"M": 2, "L": 0})
	c := e.shopper()

	c.status(404, "POST", "/cart/items", m{"productId": "nope", "size": "M", "color": "Black"})
	c.status(400, "POST", "/cart/items", m{"productId": pid, "size": "XXXL", "color": "Black"})
	c.status(400, "POST", "/cart/items", m{"productId": pid, "size": "M", "color": "Plaid"})
	if msg := c.status(409, "POST", "/cart/items", m{"productId": pid, "size": "L", "color": "Black"}); !strings.Contains(msg, "sold out") {
		t.Errorf("message: %q", msg)
	}
	c.status(400, "POST", "/cart/items", m{"productId": pid, "size": "M", "color": "Black", "qty": -1})
	c.status(409, "POST", "/cart/items", m{"productId": pid, "size": "M", "color": "Black", "qty": 3})

	// the same line merges, and the combined quantity is still limited by stock
	var cart struct {
		Lines []struct {
			ID  string `json:"id"`
			Qty int    `json:"qty"`
		} `json:"lines"`
	}
	c.ok("POST", "/cart/items", m{"productId": pid, "size": "M", "color": "Black"}, nil)
	c.ok("POST", "/cart/items", m{"productId": pid, "size": "M", "color": "Black"}, &cart)
	if len(cart.Lines) != 1 || cart.Lines[0].Qty != 2 {
		t.Fatalf("merge: %+v", cart.Lines)
	}
	c.status(409, "POST", "/cart/items", m{"productId": pid, "size": "M", "color": "Black"})
	c.status(409, "PATCH", "/cart/items/"+cart.Lines[0].ID, m{"qty": 3})
	c.status(400, "PATCH", "/cart/items/"+cart.Lines[0].ID, m{"qty": 0})
	c.ok("PATCH", "/cart/items/"+cart.Lines[0].ID, m{"qty": 1}, nil)
	c.ok("DELETE", "/cart/items/"+cart.Lines[0].ID, nil, nil)
	c.ok("GET", "/cart", nil, &cart)
	if len(cart.Lines) != 0 {
		t.Errorf("after delete: %+v", cart.Lines)
	}
}

func TestHiddenProductCantBeBought(t *testing.T) {
	e := newEnv(t)
	owner := e.brandOwner()
	pid := owner.addProduct(5000, map[string]int{"M": 5})
	c := e.shopper().withAddress()
	c.ok("POST", "/cart/items", m{"productId": pid, "size": "M", "color": "Black"}, nil)

	owner.ok("PATCH", "/studio/products/"+pid, m{"name": "Hidden", "price": 5000, "category": "Tops", "colors": []string{"Black"}, "sizes": []string{"M"}, "sizeStock": m{"M": 5}, "live": false}, nil)

	c.status(404, "GET", "/products/"+pid, nil)
	c.status(404, "POST", "/cart/items", m{"productId": pid, "size": "M", "color": "Black"})
	if msg := c.status(409, "POST", "/checkout", m{"shipping": "standard", "payment": "card"}); !strings.Contains(msg, "available") {
		t.Errorf("message: %q", msg)
	}
	if n := e.count(`SELECT stock FROM product_sizes WHERE product_id = $1`, pid); n != 5 {
		t.Errorf("stock %d", n)
	}
	// the owner can still open it
	owner.ok("GET", "/products/"+pid, nil, nil)
}

func TestLastUnitGoesToExactlyOneBuyer(t *testing.T) {
	e := newEnv(t)
	owner := e.brandOwner()
	pid := owner.addProduct(5000, map[string]int{"M": 1})

	const buyers = 8
	clients := make([]*client, buyers)
	for i := range clients {
		clients[i] = e.shopper().withAddress()
		clients[i].ok("POST", "/cart/items", m{"productId": pid, "size": "M", "color": "Black"}, nil)
	}
	var wg sync.WaitGroup
	statuses := make([]int, buyers)
	start := make(chan struct{})
	for i, c := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			statuses[i], _ = c.raw("POST", "/checkout", m{"shipping": "standard", "payment": "card"})
		}()
	}
	close(start)
	wg.Wait()

	won, lost := 0, 0
	for i, s := range statuses {
		switch s {
		case 200:
			won++
		case 409:
			lost++
			// the loser keeps their bag, intact
			var cart struct {
				Lines []any `json:"lines"`
			}
			clients[i].ok("GET", "/cart", nil, &cart)
			if len(cart.Lines) != 1 {
				t.Errorf("a losing buyer's bag has %d lines", len(cart.Lines))
			}
		default:
			t.Errorf("unexpected status %d", s)
		}
	}
	if won != 1 || lost != buyers-1 {
		t.Fatalf("won %d, lost %d: %v", won, lost, statuses)
	}
	if n := e.count(`SELECT stock FROM product_sizes WHERE product_id = $1`, pid); n != 0 {
		t.Errorf("stock %d", n)
	}
	if n := e.count(`SELECT count(*) FROM orders WHERE product_id = $1 AND status <> 'CANCELLED'`, pid); n != 1 {
		t.Errorf("%d orders for one unit", n)
	}
}

func TestPromoCodes(t *testing.T) {
	e := newEnv(t)
	owner := e.brandOwner()
	other := e.brandOwner()
	tops := owner.addProduct(10000, map[string]int{"M": 50})
	theirs := other.addProduct(10000, map[string]int{"M": 50})
	owner.ok("POST", "/studio/promotions", m{"code": "save15", "type": "% off", "value": "15", "scope": "Everything", "length": "7 days"}, nil)
	owner.ok("POST", "/studio/promotions", m{"code": "FIVER", "type": "$ off", "value": "5", "scope": "Everything", "length": "7 days"}, nil)
	owner.ok("POST", "/studio/promotions", m{"code": "SHIPFREE", "type": "Free ship", "value": "", "scope": "Everything", "length": "7 days"}, nil)
	if msg := owner.status(409, "POST", "/studio/promotions", m{"code": "SAVE15", "type": "% off", "value": "15", "scope": "Everything", "length": "7 days"}); !strings.Contains(msg, "exists") {
		t.Errorf("duplicate code: %q", msg)
	}
	owner.status(400, "POST", "/studio/promotions", m{"code": "BADPCT", "type": "% off", "value": "150", "scope": "Everything", "length": "7 days"})
	owner.status(400, "POST", "/studio/promotions", m{"code": "NOSCOPE", "type": "% off", "value": "10", "scope": "Imaginary", "length": "7 days"})

	type cartView struct {
		Promo *struct {
			Code         string `json:"code"`
			Discount     int    `json:"discount"`
			FreeShipping bool   `json:"freeShipping"`
		} `json:"promo"`
	}

	t.Run("applies only to the issuing brand's pieces", func(t *testing.T) {
		c := e.shopper().withAddress()
		c.ok("POST", "/cart/items", m{"productId": theirs, "size": "M", "color": "Black"}, nil)
		c.status(400, "PUT", "/cart/promo", m{"code": "SAVE15"})
		c.ok("POST", "/cart/items", m{"productId": tops, "size": "M", "color": "Black"}, nil)
		var cart cartView
		c.ok("PUT", "/cart/promo", m{"code": " save15 "}, &cart)
		if cart.Promo == nil || cart.Promo.Discount != 1500 {
			t.Fatalf("15%% of the eligible $100 only: %+v", cart.Promo)
		}
		var res checkoutResult
		c.ok("POST", "/checkout", m{"shipping": "standard", "payment": "card"}, &res)
		if res.Total != 20000-1500 {
			t.Errorf("total %d", res.Total)
		}
		// the discount lands on the right brand's payout, and not the other brand's
		var brandDiscount int
		if err := e.pool.QueryRow(context.Background(), `SELECT discount FROM orders WHERE checkout_number = $1 AND brand_id = $2`, res.Number, owner.brandID).Scan(&brandDiscount); err != nil {
			t.Fatal(err)
		}
		if brandDiscount != 1500 {
			t.Errorf("issuing brand's order discount %d", brandDiscount)
		}
		var otherDiscount, otherPayout int
		if err := e.pool.QueryRow(context.Background(), `SELECT discount, payout FROM orders WHERE checkout_number = $1 AND brand_id = $2`, res.Number, other.brandID).Scan(&otherDiscount, &otherPayout); err != nil {
			t.Fatal(err)
		}
		if otherDiscount != 0 || otherPayout != 9200 {
			t.Errorf("other brand: discount %d payout %d", otherDiscount, otherPayout)
		}
	})

	t.Run("dollar and free-shipping codes", func(t *testing.T) {
		c := e.shopper().withAddress()
		cheap := owner.addProduct(4000, map[string]int{"M": 10})
		c.ok("POST", "/cart/items", m{"productId": cheap, "size": "M", "color": "Black"}, nil)
		var cart cartView
		c.ok("PUT", "/cart/promo", m{"code": "FIVER"}, &cart)
		if cart.Promo.Discount != 500 {
			t.Fatalf("$5 off: %+v", cart.Promo)
		}
		c.ok("PUT", "/cart/promo", m{"code": "SHIPFREE"}, &cart)
		if !cart.Promo.FreeShipping || cart.Promo.Discount != 0 {
			t.Fatalf("free ship: %+v", cart.Promo)
		}
		var res checkoutResult
		c.ok("POST", "/checkout", m{"shipping": "standard", "payment": "card"}, &res)
		if res.Total != 4000 {
			t.Errorf("free standard shipping: total %d", res.Total)
		}
		c.ok("POST", "/cart/items", m{"productId": cheap, "size": "M", "color": "Black"}, nil)
		c.ok("PUT", "/cart/promo", m{"code": "SHIPFREE"}, nil)
		c.ok("POST", "/checkout", m{"shipping": "express", "payment": "card"}, &res)
		if res.Total != 4000+1500 {
			t.Errorf("express still charged: total %d", res.Total)
		}
	})

	t.Run("paused and unknown codes are refused, and removal works", func(t *testing.T) {
		c := e.shopper()
		c.ok("POST", "/cart/items", m{"productId": tops, "size": "M", "color": "Black"}, nil)
		c.status(400, "PUT", "/cart/promo", m{"code": "NOTREAL"})
		var cart cartView
		c.ok("PUT", "/cart/promo", m{"code": "FIVER"}, &cart)
		owner.ok("PATCH", "/studio/promotions/FIVER", m{"live": false}, nil)
		// a code that stops working falls off the bag instead of failing checkout later
		c.ok("GET", "/cart", nil, &cart)
		if cart.Promo != nil {
			t.Errorf("a paused code stayed on the bag: %+v", cart.Promo)
		}
		c.status(400, "PUT", "/cart/promo", m{"code": "FIVER"})
		owner.ok("PATCH", "/studio/promotions/FIVER", m{"live": true}, nil)
		c.ok("PUT", "/cart/promo", m{"code": "FIVER"}, nil)
		c.ok("DELETE", "/cart/promo", nil, &cart)
		if cart.Promo != nil {
			t.Error("promo not removed")
		}
	})

	t.Run("expired codes stop working", func(t *testing.T) {
		c := e.shopper()
		c.ok("POST", "/cart/items", m{"productId": tops, "size": "M", "color": "Black"}, nil)
		owner.ok("POST", "/studio/promotions", m{"code": "BRIEF", "type": "% off", "value": "10", "scope": "Everything", "length": "24h"}, nil)
		c.ok("PUT", "/cart/promo", m{"code": "BRIEF"}, nil)
		e.clock.advance(25 * time.Hour)
		defer e.clock.advance(-25 * time.Hour)
		c.status(400, "PUT", "/cart/promo", m{"code": "BRIEF"})
	})

	t.Run("uses count only for checkouts that went through", func(t *testing.T) {
		before := e.count(`SELECT uses FROM promotions WHERE code = 'SAVE15'`)
		c := e.shopper().withAddress()
		c.ok("POST", "/cart/items", m{"productId": tops, "size": "M", "color": "Black"}, nil)
		c.ok("PUT", "/cart/promo", m{"code": "SAVE15"}, nil)
		c.ok("POST", "/checkout", m{"shipping": "standard", "payment": "card"}, nil)
		if after := e.count(`SELECT uses FROM promotions WHERE code = 'SAVE15'`); after != before+1 {
			t.Errorf("uses %d → %d", before, after)
		}
	})
}

func TestOrderLifecycleSeenFromBothSides(t *testing.T) {
	e := newEnv(t)
	owner := e.brandOwner()
	pid := owner.addProduct(10000, map[string]int{"M": 5})
	c := e.shopper().withAddress()
	c.ok("POST", "/cart/items", m{"productId": pid, "size": "M", "color": "Black", "qty": 2}, nil)
	var res checkoutResult
	c.ok("POST", "/checkout", m{"shipping": "standard", "payment": "card"}, &res)
	id := res.OrderIDs[0]
	sid := studioID(id)

	type shopperView struct {
		Status       string  `json:"status"`
		Qty          int     `json:"qty"`
		Stage        int     `json:"stage"`
		Carrier      string  `json:"carrier"`
		TrackingCode *string `json:"trackingCode"`
		CanReturn    bool    `json:"canReturn"`
		CanReview    bool    `json:"canReview"`
		ReturnStatus string  `json:"returnStatus"`
		Events       []struct {
			Title string `json:"title"`
			Done  bool   `json:"done"`
		} `json:"events"`
	}
	type studioView struct {
		Status     string `json:"status"`
		Qty        int    `json:"qty"`
		Subtotal   int    `json:"subtotal"`
		Shipping   int    `json:"shipping"`
		Fee        int    `json:"fee"`
		Payout     int    `json:"payout"`
		ReturnNote string `json:"returnNote"`
		Customer   struct {
			ID string `json:"id"`
		} `json:"customer"`
	}

	// stock came off the shelf, and the bag emptied
	if n := e.count(`SELECT stock FROM product_sizes WHERE product_id = $1`, pid); n != 3 {
		t.Errorf("stock %d", n)
	}
	var cart struct {
		Lines []any `json:"lines"`
	}
	c.ok("GET", "/cart", nil, &cart)
	if len(cart.Lines) != 0 {
		t.Errorf("bag has %d lines", len(cart.Lines))
	}
	// the buyer can now tag it in a wear
	var closet []struct {
		ID string `json:"id"`
	}
	c.ok("GET", "/me/closet", nil, &closet)
	if len(closet) != 1 || closet[0].ID != pid {
		t.Errorf("closet: %+v", closet)
	}

	var so shopperView
	c.ok("GET", "/orders/"+id, nil, &so)
	if so.Status != "Confirmed" || so.Qty != 2 || so.Stage != 1 || so.TrackingCode != nil {
		t.Fatalf("shopper at start: %+v", so)
	}
	var br studioView
	owner.ok("GET", "/studio/orders/"+sid, nil, &br)
	if br.Status != "TO FULFIL" || br.Qty != 2 || br.Customer.ID != c.id {
		t.Fatalf("brand at start: %+v", br)
	}
	if br.Subtotal != 20000 || br.Fee != 1600 || br.Payout != 18400 {
		t.Errorf("money: %+v", br)
	}

	// the brand can't skip ahead, and the buyer can't return it yet
	owner.status(409, "PATCH", "/studio/orders/"+sid, m{"status": "DELIVERED"})
	owner.status(409, "PATCH", "/studio/orders/"+sid, m{"status": "RETURN APPROVED"})
	owner.status(409, "PATCH", "/studio/orders/"+sid, m{"status": "NONSENSE"})
	c.status(409, "POST", "/orders/"+id+"/return", m{"note": "too soon"})
	c.status(409, "POST", "/products/"+pid+"/reviews", m{"orderId": id, "rating": 5, "text": "early"})

	owner.ok("PATCH", "/studio/orders/"+sid, m{"status": "SHIPPED", "carrier": "GIG Logistics", "trackingCode": "GIG-123"}, nil)
	c.ok("GET", "/orders/"+id, nil, &so)
	if so.Status != "Shipped" || so.Stage != 2 || so.Carrier != "GIG Logistics" || so.TrackingCode == nil || *so.TrackingCode != "GIG-123" {
		t.Fatalf("shopper after shipping: %+v", so)
	}
	// the buyer heard about it, in the app and by email
	var notes []struct {
		Text   string `json:"text"`
		Action *struct {
			Type, Target string
		} `json:"action"`
	}
	c.ok("GET", "/notifications", nil, &notes)
	if len(notes) == 0 || !strings.Contains(notes[0].Text, "shipped") || notes[0].Action == nil || notes[0].Action.Target != id {
		t.Fatalf("notification: %+v", notes)
	}
	if body := e.mailTo(c.email, "Shipped"); !strings.Contains(body, "GIG-123") {
		t.Errorf("email: %q", body)
	}

	owner.ok("PATCH", "/studio/orders/"+sid, m{"status": "OUT FOR DELIVERY"}, nil)
	c.ok("GET", "/orders/"+id, nil, &so)
	if so.Status != "Out for delivery" || so.Stage != 3 {
		t.Fatalf("out for delivery: %+v", so)
	}

	owner.ok("PATCH", "/studio/orders/"+sid, m{"status": "DELIVERED"}, nil)
	c.ok("GET", "/orders/"+id, nil, &so)
	if so.Status != "Delivered" || so.Stage != 4 || !so.CanReturn || !so.CanReview {
		t.Fatalf("delivered: %+v", so)
	}
	for _, ev := range so.Events {
		if !ev.Done {
			t.Errorf("event %q still pending after delivery", ev.Title)
		}
	}

	// a review, once per order
	var review struct {
		Meta string `json:"meta"`
	}
	c.status(400, "POST", "/products/"+pid+"/reviews", m{"orderId": id, "rating": 6})
	c.ok("POST", "/products/"+pid+"/reviews", m{"orderId": id, "rating": 4, "text": "Solid."}, &review)
	if review.Meta != "Size M · ★★★★" {
		t.Errorf("meta %q", review.Meta)
	}
	c.status(409, "POST", "/products/"+pid+"/reviews", m{"orderId": id, "rating": 5})
	var prod struct {
		Rating struct {
			Average float64 `json:"average"`
			Count   int     `json:"count"`
		} `json:"rating"`
		Reviews []any `json:"reviews"`
	}
	c.ok("GET", "/products/"+pid, nil, &prod)
	if prod.Rating.Count != 1 || prod.Rating.Average != 4 || len(prod.Reviews) != 1 {
		t.Errorf("product rating: %+v", prod)
	}

	// return, seen by the brand, resolved by the brand
	c.status(400, "POST", "/orders/"+id+"/return", m{"note": "  "})
	c.ok("POST", "/orders/"+id+"/return", m{"note": "Too big on me"}, nil)
	owner.ok("GET", "/studio/orders/"+sid, nil, &br)
	if br.Status != "RETURN REQUESTED" || br.ReturnNote != "Too big on me" {
		t.Fatalf("brand sees return: %+v", br)
	}
	c.ok("GET", "/orders/"+id, nil, &so)
	if so.ReturnStatus != "Requested" || so.CanReturn {
		t.Fatalf("shopper sees return: %+v", so)
	}
	c.status(409, "POST", "/orders/"+id+"/return", m{"note": "again"})
	var brandNotes []struct {
		Who string `json:"who"`
	}
	owner.ok("GET", "/studio/notifications", nil, &brandNotes)
	if len(brandNotes) == 0 || brandNotes[0].Who != "Return" {
		t.Errorf("brand notification: %+v", brandNotes)
	}
	owner.ok("PATCH", "/studio/orders/"+sid, m{"status": "RETURN APPROVED"}, nil)
	c.ok("GET", "/orders/"+id, nil, &so)
	if so.ReturnStatus != "Approved" {
		t.Errorf("approved: %+v", so)
	}
	if n := e.count(`SELECT count(*) FROM orders WHERE seq = $1 AND refund_status = 'due'`, sidInt(t, sid)); n != 1 {
		t.Error("an approved return should be recorded as a refund due")
	}
	owner.status(409, "PATCH", "/studio/orders/"+sid, m{"status": "RETURN DECLINED"})
}

func sidInt(t *testing.T, s string) int64 {
	t.Helper()
	var n int64
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestReturnWindowEnforced(t *testing.T) {
	e := newEnv(t)
	owner := e.brandOwner()
	pid := owner.addProduct(10000, map[string]int{"M": 5})
	c := e.shopper().withAddress()
	res := c.buy(pid, "M")
	owner.deliver(res.OrderIDs[0])

	e.clock.advance(15 * 24 * time.Hour)
	defer e.clock.advance(-15 * 24 * time.Hour)
	var so struct {
		CanReturn bool `json:"canReturn"`
	}
	c.ok("GET", "/orders/"+res.OrderIDs[0], nil, &so)
	if so.CanReturn {
		t.Error("the 14-day window has closed")
	}
	if msg := c.status(409, "POST", "/orders/"+res.OrderIDs[0]+"/return", m{"note": "late"}); !strings.Contains(msg, "window") {
		t.Errorf("message %q", msg)
	}

	// a brand with no returns policy
	owner.ok("PUT", "/studio/settings/shipping", m{"shipsFrom": "Lagos", "zones": m{"lagos": true}, "processing": "48h", "returns": "None"}, nil)
	res2 := c.buy(pid, "M")
	owner.deliver(res2.OrderIDs[0])
	c.status(409, "POST", "/orders/"+res2.OrderIDs[0]+"/return", m{"note": "no"})
}

func TestStockAlertOnRestock(t *testing.T) {
	e := newEnv(t)
	owner := e.brandOwner()
	pid := owner.addProduct(10000, map[string]int{"M": 0, "L": 2})
	c := e.shopper()

	c.status(400, "POST", "/products/"+pid+"/stock-alerts", m{"size": "L"})
	c.status(404, "POST", "/products/"+pid+"/stock-alerts", m{"size": "XXXL"})
	c.ok("POST", "/products/"+pid+"/stock-alerts", m{"size": "M"}, nil)

	owner.ok("PATCH", "/studio/products/"+pid+"/stock", m{"size": "M", "stock": 4}, nil)
	var notes []struct {
		Text string `json:"text"`
		Href string `json:"href"`
	}
	c.ok("GET", "/notifications", nil, &notes)
	if len(notes) != 1 || !strings.Contains(notes[0].Text, "back in M") {
		t.Fatalf("notifications: %+v", notes)
	}
	// only once
	owner.ok("PATCH", "/studio/products/"+pid+"/stock", m{"size": "M", "stock": 0}, nil)
	owner.ok("PATCH", "/studio/products/"+pid+"/stock", m{"size": "M", "stock": 2}, nil)
	c.ok("GET", "/notifications", nil, &notes)
	if len(notes) != 1 {
		t.Errorf("notified %d times", len(notes))
	}
}

/* ---------- Paystack, against a local fake ---------- */

type fakePaystack struct {
	mu          sync.Mutex
	status      map[string]string // reference → status
	amount      map[string]int64
	currency    map[string]string
	failInit    bool
	declineAuth bool
	initBodies  []map[string]any
}

const paystackSecret = "sk_test_fixture"

func newFakePaystack(t *testing.T) (*fakePaystack, *payments.Paystack) {
	t.Helper()
	f := &fakePaystack{status: map[string]string{}, amount: map[string]int64{}, currency: map[string]string{}}
	mux := http.NewServeMux()
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer "+paystackSecret {
			http.Error(w, "unauthorized", 401)
			return false
		}
		return true
	}
	mux.HandleFunc("POST /transaction/initialize", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.failInit {
			http.Error(w, "boom", 500)
			return
		}
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.initBodies = append(f.initBodies, b)
		ref := b["reference"].(string)
		f.status[ref] = "abandoned"
		f.amount[ref] = int64(b["amount"].(float64))
		f.currency[ref] = b["currency"].(string)
		_ = json.NewEncoder(w).Encode(m{"status": true, "data": m{"authorization_url": "https://paystack.test/pay/" + ref, "reference": ref}})
	})
	mux.HandleFunc("POST /transaction/charge_authorization", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		st := "success"
		if f.declineAuth {
			st = "failed"
		}
		_ = json.NewEncoder(w).Encode(m{"status": true, "data": m{"status": st, "amount": b["amount"], "currency": b["currency"]}})
	})
	mux.HandleFunc("GET /transaction/verify/{ref}", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		ref := r.PathValue("ref")
		st, ok := f.status[ref]
		if !ok {
			http.Error(w, "unknown", 404)
			return
		}
		_ = json.NewEncoder(w).Encode(m{"status": true, "data": m{
			"status": st, "amount": f.amount[ref], "currency": f.currency[ref],
			"authorization": m{"authorization_code": "AUTH_real", "last4": "1234", "exp_month": "08", "exp_year": "2029", "brand": "visa", "signature": "SIG_1", "reusable": true, "channel": "card"},
		}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, &payments.Paystack{Secret: paystackSecret, Base: srv.URL}
}

func (f *fakePaystack) set(ref, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[ref] = status
}

func (f *fakePaystack) setAmount(ref string, amount int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.amount[ref] = amount
}

func webhookBody(ref string) []byte {
	b, _ := json.Marshal(m{"event": "charge.success", "data": m{"reference": ref}})
	return b
}

func sign(body []byte) string {
	mac := hmac.New(sha512.New, []byte(paystackSecret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func (e *env) webhook(body []byte, signature string) int {
	e.t.Helper()
	req, err := http.NewRequest("POST", e.srv.URL+api+"/webhooks/paystack", strings.NewReader(string(body)))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("x-paystack-signature", signature)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	return res.StatusCode
}

type paystackFixture struct {
	e     *env
	fake  *fakePaystack
	owner *client
	buyer *client
	pid   string
}

func newPaystackFixture(t *testing.T) *paystackFixture {
	t.Helper()
	fake, provider := newFakePaystack(t)
	e := newEnv(t, func(o *options) { o.pay = provider })
	owner := e.brandOwner()
	return &paystackFixture{e: e, fake: fake, owner: owner, buyer: e.shopper().withAddress(), pid: owner.addProduct(10000, map[string]int{"M": 5})}
}

func (p *paystackFixture) startCheckout(t *testing.T) checkoutResult {
	t.Helper()
	p.buyer.ok("POST", "/cart/items", m{"productId": p.pid, "size": "M", "color": "Black"}, nil)
	var res checkoutResult
	p.buyer.ok("POST", "/checkout", m{"shipping": "standard", "payment": "card"}, &res)
	if res.RedirectURL == "" || res.Status != "pending" {
		t.Fatalf("expected a redirect to Paystack: %+v", res)
	}
	return res
}

func TestPaystackUnpaidOrdersStayInvisible(t *testing.T) {
	p := newPaystackFixture(t)
	res := p.startCheckout(t)

	// stock is held and the bag is empty while they pay...
	if n := p.e.count(`SELECT stock FROM product_sizes WHERE product_id = $1`, p.pid); n != 4 {
		t.Errorf("stock %d", n)
	}
	// ...but nobody treats it as an order yet
	var mine []any
	p.buyer.ok("GET", "/me/orders", nil, &mine)
	if len(mine) != 0 {
		t.Errorf("shopper sees an unpaid order: %d", len(mine))
	}
	var theirs []any
	p.owner.ok("GET", "/studio/orders", nil, &theirs)
	if len(theirs) != 0 {
		t.Errorf("brand sees an unpaid order: %d", len(theirs))
	}
	p.owner.status(404, "PATCH", "/studio/orders/"+studioID(res.OrderIDs[0]), m{"status": "SHIPPED"})
	p.buyer.status(404, "GET", "/orders/"+res.OrderIDs[0], nil)
	var notes []any
	p.owner.ok("GET", "/studio/notifications", nil, &notes)
	if len(notes) != 0 {
		t.Errorf("brand notified before payment: %d", len(notes))
	}
}

func TestPaystackWebhookSecurity(t *testing.T) {
	p := newPaystackFixture(t)
	res := p.startCheckout(t)
	p.fake.set(res.Number, "success")
	body := webhookBody(res.Number)

	for name, sig := range map[string]string{"missing": "", "wrong": strings.Repeat("0", 128), "signed with another key": func() string {
		mac := hmac.New(sha512.New, []byte("attacker"))
		mac.Write(body)
		return hex.EncodeToString(mac.Sum(nil))
	}()} {
		if got := p.e.webhook(body, sig); got != 401 {
			t.Errorf("%s signature: status %d", name, got)
		}
	}
	if n := p.e.count(`SELECT count(*) FROM checkouts WHERE number = $1 AND status = 'paid'`, res.Number); n != 0 {
		t.Fatal("a forged webhook marked the order paid")
	}
	// tampering with a correctly signed body breaks the signature too
	tampered := []byte(strings.Replace(string(body), res.Number, "WC-99999", 1))
	if got := p.e.webhook(tampered, sign(body)); got != 401 {
		t.Errorf("tampered body: status %d", got)
	}
	// a real signature on an event we don't act on is acknowledged and ignored
	other, _ := json.Marshal(m{"event": "transfer.success", "data": m{"reference": res.Number}})
	if got := p.e.webhook(other, sign(other)); got != 200 {
		t.Errorf("other event: status %d", got)
	}
	if n := p.e.count(`SELECT count(*) FROM checkouts WHERE number = $1 AND status = 'paid'`, res.Number); n != 0 {
		t.Fatal("an unrelated event marked the order paid")
	}
	// unknown references don't error, so Paystack doesn't retry forever
	unknown := webhookBody("WC-404")
	p.fake.set("WC-404", "success")
	if got := p.e.webhook(unknown, sign(unknown)); got != 200 {
		t.Errorf("unknown reference: status %d", got)
	}
}

func TestPaystackDuplicateWebhooksProcessOnce(t *testing.T) {
	p := newPaystackFixture(t)
	res := p.startCheckout(t)
	p.fake.set(res.Number, "success")
	body := webhookBody(res.Number)

	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := p.e.webhook(body, sign(body)); got != 200 {
				t.Errorf("status %d", got)
			}
		}()
	}
	wg.Wait()
	if got := p.e.webhook(body, sign(body)); got != 200 {
		t.Errorf("late duplicate: status %d", got)
	}

	if n := p.e.count(`SELECT count(*) FROM checkouts WHERE number = $1 AND status = 'paid'`, res.Number); n != 1 {
		t.Fatal("not marked paid")
	}
	if n := p.e.count(`SELECT count(*) FROM orders WHERE checkout_number = $1 AND status = 'TO FULFIL'`, res.Number); n != 1 {
		t.Error("order not released to the brand")
	}
	seq := sidInt(t, studioID(res.OrderIDs[0]))
	if n := p.e.count(`SELECT count(*) FROM order_events WHERE order_seq = $1 AND stage = 'paid'`, seq); n != 1 {
		t.Errorf("%d paid events", n)
	}
	if n := p.e.count(`SELECT count(*) FROM notifications WHERE brand_id = $1`, p.owner.brandID); n != 1 {
		t.Errorf("brand notified %d times", n)
	}
	if n := p.e.count(`SELECT count(*) FROM payment_methods WHERE user_id = $1`, p.buyer.id); n != 1 {
		t.Errorf("%d saved cards", n)
	}
	// the card is usable and its reusable token isn't stored in plaintext
	if n := p.e.count(`SELECT count(*) FROM payment_methods WHERE user_id = $1 AND auth_enc LIKE '%AUTH_real%'`, p.buyer.id); n != 0 {
		t.Error("the card authorisation is stored in plaintext")
	}
	var methods []struct {
		Label     string `json:"label"`
		IsDefault bool   `json:"isDefault"`
	}
	p.buyer.ok("GET", "/me/payment-methods", nil, &methods)
	if len(methods) != 1 || methods[0].Label != "Visa •••• 1234" || !methods[0].IsDefault {
		t.Errorf("methods: %+v", methods)
	}
	if body := p.e.mailTo(p.buyer.email, "confirmed"); !strings.Contains(body, res.Number) {
		t.Errorf("confirmation email: %q", body)
	}
}

func TestPaystackWrongAmountIsNotPaid(t *testing.T) {
	p := newPaystackFixture(t)
	res := p.startCheckout(t)
	p.fake.set(res.Number, "success")
	p.fake.setAmount(res.Number, 1) // someone paid one kobo

	// the customer's return page doesn't break, and the checkout isn't left hanging
	var c struct {
		Status string `json:"status"`
	}
	p.buyer.ok("GET", "/checkouts/"+res.Number, nil, &c)
	if c.Status != "amount_mismatch" {
		t.Fatalf("status %q", c.Status)
	}
	// acknowledged, so Paystack doesn't retry something retrying can't fix
	body := webhookBody(res.Number)
	if got := p.e.webhook(body, sign(body)); got != 200 {
		t.Errorf("webhook status %d", got)
	}
	// never fulfilled, and everything held for it comes back
	if n := p.e.count(`SELECT count(*) FROM orders WHERE checkout_number = $1 AND status = 'TO FULFIL'`, res.Number); n != 0 {
		t.Fatal("a short payment released the order to the brand")
	}
	if n := p.e.count(`SELECT count(*) FROM checkouts WHERE number = $1 AND paid_at IS NOT NULL`, res.Number); n != 0 {
		t.Error("a short payment was recorded as paid")
	}
	assertRestored(t, p)
}

func TestPaystackWrongAmountAtExpiryDoesntHangForever(t *testing.T) {
	p := newPaystackFixture(t)
	res := p.startCheckout(t)
	p.fake.set(res.Number, "success")
	p.fake.setAmount(res.Number, 1)
	p.e.clock.advance(31 * time.Minute)
	p.e.server.Jobs.Tick(context.Background())
	if n := p.e.count(`SELECT count(*) FROM checkouts WHERE number = $1 AND status = 'amount_mismatch'`, res.Number); n != 1 {
		t.Fatal("expiry left the checkout pending, holding its stock forever")
	}
	assertRestored(t, p)
}

func TestPaystackReturnURLVerifiesWithProvider(t *testing.T) {
	p := newPaystackFixture(t)
	res := p.startCheckout(t)
	type view struct {
		Status      string   `json:"status"`
		OrderIDs    []string `json:"orderIds"`
		RedirectURL string   `json:"redirectUrl"`
	}
	// coming back from Paystack without having paid proves nothing
	var before view
	p.buyer.ok("GET", "/checkouts/"+res.Number, nil, &before)
	if before.Status != "pending" || before.RedirectURL == "" {
		t.Fatalf("before paying: %+v", before)
	}
	p.fake.set(res.Number, "success")
	var after view
	p.buyer.ok("GET", "/checkouts/"+res.Number, nil, &after)
	if after.Status != "paid" || after.RedirectURL != "" {
		t.Fatalf("after paying: %+v", after)
	}
	p.buyer.ok("GET", "/orders/"+after.OrderIDs[0], nil, nil)
}

func TestPaystackFailedAndAbandonedPayments(t *testing.T) {
	t.Run("a failed charge restores the bag right away", func(t *testing.T) {
		p := newPaystackFixture(t)
		res := p.startCheckout(t)
		p.fake.set(res.Number, "failed")
		var c struct {
			Status string `json:"status"`
		}
		p.buyer.ok("GET", "/checkouts/"+res.Number, nil, &c)
		if c.Status != "failed" {
			t.Fatalf("status %q", c.Status)
		}
		assertRestored(t, p)
	})

	t.Run("abandoned checkouts expire, releasing stock back and refilling the bag", func(t *testing.T) {
		p := newPaystackFixture(t)
		p.owner.ok("POST", "/studio/promotions", m{"code": "BACK", "type": "% off", "value": "10", "scope": "Everything", "length": "30 days"}, nil)
		p.buyer.ok("POST", "/cart/items", m{"productId": p.pid, "size": "M", "color": "Black"}, nil)
		p.buyer.ok("PUT", "/cart/promo", m{"code": "BACK"}, nil)
		var res checkoutResult
		p.buyer.ok("POST", "/checkout", m{"shipping": "standard", "payment": "card"}, &res)
		if n := p.e.count(`SELECT uses FROM promotions WHERE code = 'BACK'`); n != 1 {
			t.Errorf("uses during payment: %d", n)
		}

		// not yet: still inside the 30 minutes
		p.e.server.Jobs.Tick(context.Background())
		if n := p.e.count(`SELECT stock FROM product_sizes WHERE product_id = $1`, p.pid); n != 4 {
			t.Errorf("released too early: stock %d", n)
		}

		p.e.clock.advance(31 * time.Minute)
		p.e.server.Jobs.Tick(context.Background())

		if n := p.e.count(`SELECT count(*) FROM checkouts WHERE number = $1 AND status = 'expired'`, res.Number); n != 1 {
			t.Fatal("not expired")
		}
		assertRestored(t, p)
		if n := p.e.count(`SELECT uses FROM promotions WHERE code = 'BACK'`); n != 0 {
			t.Errorf("promo use not released: %d", n)
		}
		var cart struct {
			Promo *struct {
				Code string `json:"code"`
			} `json:"promo"`
		}
		p.buyer.ok("GET", "/cart", nil, &cart)
		if cart.Promo == nil || cart.Promo.Code != "BACK" {
			t.Errorf("promo not back on the bag: %+v", cart.Promo)
		}
	})

	t.Run("expiry checks the provider first in case a webhook went missing", func(t *testing.T) {
		p := newPaystackFixture(t)
		res := p.startCheckout(t)
		p.fake.set(res.Number, "success")
		p.e.clock.advance(31 * time.Minute)
		p.e.server.Jobs.Tick(context.Background())
		if n := p.e.count(`SELECT count(*) FROM checkouts WHERE number = $1 AND status = 'paid'`, res.Number); n != 1 {
			t.Fatal("a paid checkout was expired")
		}
		if n := p.e.count(`SELECT stock FROM product_sizes WHERE product_id = $1`, p.pid); n != 4 {
			t.Errorf("stock %d", n)
		}
	})

	t.Run("payment arriving after expiry is flagged, not silently fulfilled", func(t *testing.T) {
		p := newPaystackFixture(t)
		res := p.startCheckout(t)
		p.e.clock.advance(31 * time.Minute)
		p.e.server.Jobs.Tick(context.Background())
		p.fake.set(res.Number, "success")
		body := webhookBody(res.Number)
		if got := p.e.webhook(body, sign(body)); got != 200 {
			t.Fatalf("status %d", got)
		}
		if n := p.e.count(`SELECT count(*) FROM checkouts WHERE number = $1 AND status = 'paid_after_expiry'`, res.Number); n != 1 {
			t.Fatal("late payment not flagged for a person to handle")
		}
		if n := p.e.count(`SELECT count(*) FROM orders WHERE checkout_number = $1 AND status = 'TO FULFIL'`, res.Number); n != 0 {
			t.Error("stock was already released; this must not ship automatically")
		}
	})

	t.Run("provider outage leaves the customer whole", func(t *testing.T) {
		p := newPaystackFixture(t)
		p.fake.failInit = true
		p.buyer.ok("POST", "/cart/items", m{"productId": p.pid, "size": "M", "color": "Black"}, nil)
		if msg := p.buyer.status(502, "POST", "/checkout", m{"shipping": "standard", "payment": "card"}); !strings.Contains(msg, "haven’t been charged") {
			t.Errorf("message %q", msg)
		}
		assertRestored(t, p)
	})
}

func assertRestored(t *testing.T, p *paystackFixture) {
	t.Helper()
	if n := p.e.count(`SELECT stock FROM product_sizes WHERE product_id = $1`, p.pid); n != 5 {
		t.Errorf("stock %d, want 5", n)
	}
	var cart struct {
		Lines []struct {
			Qty int `json:"qty"`
		} `json:"lines"`
	}
	p.buyer.ok("GET", "/cart", nil, &cart)
	if len(cart.Lines) != 1 || cart.Lines[0].Qty != 1 {
		t.Errorf("bag not restored: %+v", cart.Lines)
	}
	var orders []any
	p.buyer.ok("GET", "/me/orders", nil, &orders)
	if len(orders) != 0 {
		t.Errorf("a cancelled order is showing: %d", len(orders))
	}
}

func TestPaystackSavedCard(t *testing.T) {
	p := newPaystackFixture(t)
	res := p.startCheckout(t)
	p.fake.set(res.Number, "success")
	body := webhookBody(res.Number)
	p.e.webhook(body, sign(body))
	var methods []struct {
		ID string `json:"id"`
	}
	p.buyer.ok("GET", "/me/payment-methods", nil, &methods)
	if len(methods) != 1 {
		t.Fatalf("methods: %+v", methods)
	}

	t.Run("charges straight through", func(t *testing.T) {
		p.buyer.ok("POST", "/cart/items", m{"productId": p.pid, "size": "M", "color": "Black"}, nil)
		var res checkoutResult
		p.buyer.ok("POST", "/checkout", m{"shipping": "standard", "payment": "card", "paymentMethodId": methods[0].ID}, &res)
		if res.Status != "paid" || res.RedirectURL != "" {
			t.Fatalf("%+v", res)
		}
	})
	t.Run("a decline restores the bag", func(t *testing.T) {
		p.fake.declineAuth = true
		defer func() { p.fake.declineAuth = false }()
		before := p.e.count(`SELECT stock FROM product_sizes WHERE product_id = $1`, p.pid)
		p.buyer.ok("POST", "/cart/items", m{"productId": p.pid, "size": "M", "color": "Black"}, nil)
		p.buyer.status(402, "POST", "/checkout", m{"shipping": "standard", "payment": "card", "paymentMethodId": methods[0].ID})
		if after := p.e.count(`SELECT stock FROM product_sizes WHERE product_id = $1`, p.pid); after != before {
			t.Errorf("stock %d → %d", before, after)
		}
		var cart struct {
			Lines []any `json:"lines"`
		}
		p.buyer.ok("GET", "/cart", nil, &cart)
		if len(cart.Lines) != 1 {
			t.Errorf("bag: %d lines", len(cart.Lines))
		}
		p.buyer.ok("DELETE", "/cart/items/x", nil, nil)
	})
	t.Run("another person's saved card can't be used", func(t *testing.T) {
		thief := p.e.shopper().withAddress()
		thief.ok("POST", "/cart/items", m{"productId": p.pid, "size": "M", "color": "Black"}, nil)
		thief.status(400, "POST", "/checkout", m{"shipping": "standard", "payment": "card", "paymentMethodId": methods[0].ID})
		thief.status(404, "DELETE", "/me/payment-methods/"+methods[0].ID, nil)
	})
	t.Run("removing it works", func(t *testing.T) {
		p.buyer.ok("DELETE", "/me/payment-methods/"+methods[0].ID, nil, nil)
		var left []any
		p.buyer.ok("GET", "/me/payment-methods", nil, &left)
		if len(left) != 0 {
			t.Errorf("%d left", len(left))
		}
	})
}

func TestFXConversionChargesTheConvertedAmount(t *testing.T) {
	fake, provider := newFakePaystack(t)
	e := newEnv(t, func(o *options) { o.pay = provider })
	e.deps.Cfg.PaymentCurrency = "NGN"
	e.deps.Cfg.FXRate = 1500
	owner := e.brandOwner()
	pid := owner.addProduct(10000, map[string]int{"M": 5})
	c := e.shopper().withAddress()
	c.ok("POST", "/cart/items", m{"productId": pid, "size": "M", "color": "Black"}, nil)
	var res checkoutResult
	c.ok("POST", "/checkout", m{"shipping": "standard", "payment": "bank_transfer"}, &res)
	if res.Total != 10800 {
		t.Errorf("the shopper's total stays in USD: %d", res.Total)
	}
	fake.mu.Lock()
	b := fake.initBodies[0]
	fake.mu.Unlock()
	if b["amount"].(float64) != 10800*1500 || b["currency"] != "NGN" {
		t.Errorf("sent to Paystack: %v %v", b["amount"], b["currency"])
	}
	if channels := fmt.Sprint(b["channels"]); !strings.Contains(channels, "bank_transfer") {
		t.Errorf("channels: %s", channels)
	}
}

var _ = errors.New
