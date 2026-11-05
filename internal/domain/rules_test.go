package domain

import (
	"testing"
	"time"
)

// Ported from wearwhat/src/lib/cart.test.ts and studio.test.ts, now that the server owns these rules.

func TestShippingCost(t *testing.T) {
	cases := []struct {
		name     string
		subtotal int
		speed    string
		promo    bool
		want     int
	}{
		{"standard below threshold", 14999, "standard", false, 800},
		{"standard at threshold is free", 15000, "standard", false, 0},
		{"standard above threshold is free", 24000, "standard", false, 0},
		{"express is always charged", 24000, "express", false, 1500},
		{"free ship promo waives standard", 5000, "standard", true, 0},
		{"free ship promo doesn't waive express", 5000, "express", true, 1500},
	}
	for _, c := range cases {
		if got := ShippingCost(c.subtotal, c.speed, c.promo); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

func TestSettle(t *testing.T) {
	m := LineMoney{Subtotal: 24000, Shipping: 800}
	m.Settle()
	if m.Fee != 1920 || m.Payout != 22880 {
		t.Fatalf("plain sale: fee %d payout %d", m.Fee, m.Payout)
	}
	// a brand-funded discount comes out of the sale the fee is charged on
	m = LineMoney{Subtotal: 10000, Discount: 1500, BrandDiscount: 1500, Shipping: 0}
	m.Settle()
	if m.Fee != 680 || m.Payout != 7820 {
		t.Fatalf("brand discount: fee %d payout %d", m.Fee, m.Payout)
	}
	// a wearwhat-funded discount leaves the brand whole
	m = LineMoney{Subtotal: 10000, Discount: 1000, BrandDiscount: 0}
	m.Settle()
	if m.Fee != 800 || m.Payout != 9200 {
		t.Fatalf("platform discount: fee %d payout %d", m.Fee, m.Payout)
	}
}

func TestAllocateAlwaysSumsExactly(t *testing.T) {
	cases := []struct {
		amount  int
		weights []int
	}{
		{800, []int{18000, 5800}},
		{1, []int{1, 1, 1}},
		{1000, []int{3333, 3333, 3334}},
		{799, []int{1, 2, 3, 4, 5, 6, 7}},
		{1500, []int{0, 100, 0}},
	}
	for _, c := range cases {
		parts := Allocate(c.amount, c.weights)
		sum := 0
		for i, p := range parts {
			sum += p
			if c.weights[i] == 0 && p != 0 {
				t.Errorf("allocate %d %v: zero weight got %d", c.amount, c.weights, p)
			}
		}
		if sum != c.amount {
			t.Errorf("allocate %d %v: parts %v sum to %d", c.amount, c.weights, parts, sum)
		}
	}
	if got := Allocate(100, []int{0, 0}); got[0] != 0 || got[1] != 0 {
		t.Errorf("no weight should allocate nothing, got %v", got)
	}
}

func TestPromoDiscount(t *testing.T) {
	sum := func(v []int) int {
		n := 0
		for _, x := range v {
			n += x
		}
		return n
	}
	if got := PromoDiscount("% off", "15", []int{10000, 0, 5000}); sum(got) != 2250 || got[1] != 0 {
		t.Errorf("percent: %v", got)
	}
	if got := PromoDiscount("$ off", "20", []int{10000, 5000}); sum(got) != 2000 {
		t.Errorf("dollars: %v", got)
	}
	if got := PromoDiscount("$ off", "500", []int{10000}); sum(got) != 10000 {
		t.Errorf("a dollar discount can't exceed the eligible total: %v", got)
	}
	if got := PromoDiscount("% off", "150", []int{10000}); sum(got) != 10000 {
		t.Errorf("a percentage is capped at 100: %v", got)
	}
	if got := PromoDiscount("Free ship", "", []int{10000}); sum(got) != 0 {
		t.Errorf("free shipping takes nothing off the goods: %v", got)
	}
	if got := PromoDiscount("% off", "abc", []int{10000}); sum(got) != 0 {
		t.Errorf("a bad value discounts nothing: %v", got)
	}
}

func TestOrderFlow(t *testing.T) {
	allowed := [][2]string{
		{StatusToFulfil, StatusShipped}, {StatusShipped, StatusOutForDelivery}, {StatusShipped, StatusDelivered},
		{StatusOutForDelivery, StatusDelivered}, {StatusReturnReq, StatusReturnOK}, {StatusReturnReq, StatusReturnNo},
	}
	for _, a := range allowed {
		if !CanTransition(a[0], a[1]) {
			t.Errorf("%s → %s should be allowed", a[0], a[1])
		}
	}
	blocked := [][2]string{
		{StatusToFulfil, StatusDelivered}, {StatusDelivered, StatusShipped}, {StatusDelivered, StatusReturnOK},
		{StatusReturnOK, StatusReturnNo}, {StatusReturnNo, StatusReturnOK}, {StatusToFulfil, StatusReturnReq},
		{StatusPending, StatusShipped}, {StatusCancelled, StatusShipped},
	}
	for _, b := range blocked {
		if CanTransition(b[0], b[1]) {
			t.Errorf("%s → %s should be blocked", b[0], b[1])
		}
	}
}

func TestCatalogStatus(t *testing.T) {
	cases := map[string]struct {
		stock  int
		hidden bool
	}{
		"Hidden": {10, true}, "Sold out": {0, false}, "Low stock": {5, false}, "Live": {6, false},
	}
	for want, c := range cases {
		if got := CatalogStatus(c.stock, c.hidden); got != want {
			t.Errorf("stock %d hidden %v: got %s, want %s", c.stock, c.hidden, got, want)
		}
	}
}

func TestDefaultSize(t *testing.T) {
	if got := DefaultSize([]SizeStock{{"S", 2}, {"M", 2}, {"L", 2}}); got != "M" {
		t.Errorf("prefers M: %s", got)
	}
	if got := DefaultSize([]SizeStock{{"S", 2}, {"M", 0}, {"L", 2}}); got != "S" {
		t.Errorf("skips a sold-out M: %s", got)
	}
	if got := DefaultSize([]SizeStock{{"8", 0}, {"10", 1}}); got != "10" {
		t.Errorf("prefers 10 for bottoms: %s", got)
	}
	if got := DefaultSize([]SizeStock{{"S", 0}}); got != "S" {
		t.Errorf("falls back to the first size: %s", got)
	}
	if got := DefaultSize(nil); got != "" {
		t.Errorf("no sizes: %q", got)
	}
}

func TestValidation(t *testing.T) {
	for _, h := range []string{"amara.k", "a_b", "abc", "user123"} {
		if !ValidHandle(h) {
			t.Errorf("%q should be valid", h)
		}
	}
	for _, h := range []string{"ab", "Amara", "has space", "emoji😀x", "<script>", "../etc"} {
		if ValidHandle(h) {
			t.Errorf("%q should be invalid", h)
		}
	}
	if NormalizeHandle(" @Amara.K ") != "amara.k" {
		t.Error("handles normalise to lowercase without @")
	}
	if NormalizePhone("+234 (803) 555-0142") != "+2348035550142" {
		t.Error("phones keep + and digits only")
	}
	if Initials("high fashion") != "HI" || Initials("123") != "WW" {
		t.Error("initials")
	}
	if n, ok := ParseOrderID("WW-20417"); !ok || n != 20417 {
		t.Error("shopper id form")
	}
	if n, ok := ParseOrderID("20417"); !ok || n != 20417 {
		t.Error("studio id form")
	}
	if _, ok := ParseOrderID("abc"); ok {
		t.Error("garbage id")
	}
}

func TestTimeLabels(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	if Ago(now.Add(-30*time.Second), now) != "now" || Ago(now.Add(-12*time.Minute), now) != "12m" ||
		Ago(now.Add(-3*time.Hour), now) != "3h" || Ago(now.Add(-48*time.Hour), now) != "2d" {
		t.Error("Ago")
	}
	if ChatTime(now.Add(-20*time.Second), now) != "Now" || ChatTime(now.Add(-26*time.Hour), now) != "Yesterday" {
		t.Error("ChatTime")
	}
	if NotificationGroup(now.Add(-time.Hour), now) != "Today" || NotificationGroup(now.Add(-48*time.Hour), now) != "This week" ||
		NotificationGroup(now.Add(-10*24*time.Hour), now) != "Earlier" {
		t.Error("NotificationGroup")
	}
}
