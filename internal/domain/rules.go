package domain

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

/* ---------- pricing (src/lib/cart.ts) ---------- */

const (
	FreeShippingFrom = 15000
	StandardShipping = 800
	ExpressShipping  = 1500
	PlatformFeeRate  = 0.08
)

// ShippingCost is the delivery charge for a bag. A free-shipping promo waives standard delivery only.
func ShippingCost(subtotal int, speed string, freeShipPromo bool) int {
	if speed == "express" {
		return ExpressShipping
	}
	if freeShipPromo || subtotal >= FreeShippingFrom {
		return 0
	}
	return StandardShipping
}

// LineMoney is what one order's money looks like once a checkout is split across its lines.
type LineMoney struct {
	Subtotal      int
	Discount      int // everything off this line
	BrandDiscount int // the part the brand pays for; wearwhat-funded codes leave this 0
	Shipping      int
	Fee           int
	Payout        int
}

// Fee is the platform's 8%, charged on what the brand actually sold the piece for.
func Fee(discountedSubtotal int) int {
	return int(math.Round(float64(discountedSubtotal) * PlatformFeeRate))
}

// Settle computes a line's fee and payout: the brand gets its discounted sale plus the
// shipping allocated to it, minus the platform fee.
func (m *LineMoney) Settle() {
	sold := m.Subtotal - m.BrandDiscount
	m.Fee = Fee(sold)
	m.Payout = sold + m.Shipping - m.Fee
}

// Allocate splits amount across weights in proportion, giving rounding leftovers to the
// largest weights so the parts always sum exactly to amount.
func Allocate(amount int, weights []int) []int {
	out := make([]int, len(weights))
	total := 0
	for _, w := range weights {
		total += w
	}
	if total == 0 || amount == 0 {
		return out
	}
	left := amount
	for i, w := range weights {
		out[i] = amount * w / total
		left -= out[i]
	}
	for i := 0; left > 0; i = (i + 1) % len(out) {
		if weights[i] > 0 {
			out[i]++
			left--
		}
	}
	return out
}

/* ---------- promotions ---------- */

// PromoDiscount is the money off for a promo across the eligible line subtotals.
func PromoDiscount(promoType, value string, eligible []int) []int {
	out := make([]int, len(eligible))
	total := 0
	for _, s := range eligible {
		total += s
	}
	if total == 0 {
		return out
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || v <= 0 {
		return out
	}
	switch promoType {
	case "% off":
		pct := math.Min(v, 100)
		amount := int(math.Round(float64(total) * pct / 100))
		return Allocate(amount, eligible)
	case "$ off":
		amount := int(math.Round(v * 100))
		if amount > total {
			amount = total
		}
		return Allocate(amount, eligible)
	}
	return out
}

// PromoLabel is the short description shown in the bag.
func PromoLabel(promoType, value string) string {
	switch promoType {
	case "% off":
		return value + "% off"
	case "$ off":
		return "$" + value + " off"
	default:
		return "Free standard shipping"
	}
}

// PromoEnds turns a "runs for" choice into an end time.
func PromoEnds(length string, from time.Time) *time.Time {
	var d time.Duration
	switch length {
	case "24h":
		d = 24 * time.Hour
	case "7 days":
		d = 7 * 24 * time.Hour
	case "30 days":
		d = 30 * 24 * time.Hour
	default:
		return nil
	}
	t := from.Add(d)
	return &t
}

/* ---------- catalog (src/lib/catalog.ts) ---------- */

const LowStockAt = 5

// CatalogStatus labels a catalog row.
func CatalogStatus(stock int, hidden bool) string {
	switch {
	case hidden:
		return "Hidden"
	case stock == 0:
		return "Sold out"
	case stock <= LowStockAt:
		return "Low stock"
	}
	return "Live"
}

// ColorHex is the swatch for each colour a brand can pick.
var ColorHex = map[string]string{
	"Black": "#0E0E0E", "White": "#F5F4F0", "Grey": "#8A8A8A", "Denim": "#3E5C7E",
	"Red": "#C5281C", "Brown": "#6B4A32", "Multi": "#B7A99A", "Bone": "#E6E2D8", "Charcoal": "#4A4A4A",
}

// Categories a product can belong to.
var Categories = []string{"Tops", "Outerwear", "Knitwear", "Bottoms"}

// DefaultSize picks the size offered by one-tap add to bag: the middle size when in stock,
// otherwise the first size with stock, otherwise the first size.
func DefaultSize(sizes []SizeStock) string {
	if len(sizes) == 0 {
		return ""
	}
	for _, want := range []string{"M", "10"} {
		for _, s := range sizes {
			if s.Label == want && s.Stock > 0 {
				return s.Label
			}
		}
	}
	for _, s := range sizes {
		if s.Stock > 0 {
			return s.Label
		}
	}
	return sizes[0].Label
}

// FitPosition maps a wear's fit report onto the product page's scale.
func FitPosition(fit string) int {
	switch fit {
	case "Runs small":
		return 0
	case "Runs large":
		return 100
	}
	return 50
}

// FitSummaryFor turns the average position into words. With no wears yet it says so.
func FitSummaryFor(avg float64, wears int) FitSummary {
	if wears == 0 {
		return FitSummary{Summary: "No fit reports yet", Wears: 0, Position: 50}
	}
	s := "Fits true to size"
	switch {
	case avg < 35:
		s = "Runs small"
	case avg > 65:
		s = "Runs large"
	}
	return FitSummary{Summary: s, Wears: wears, Position: int(math.Round(avg))}
}

/* ---------- orders (src/lib/studioOrders.ts) ---------- */

// Order statuses as stored. "PENDING" and "CANCELLED" never reach the studio or the shopper.
const (
	StatusPending        = "PENDING"
	StatusToFulfil       = "TO FULFIL"
	StatusShipped        = "SHIPPED"
	StatusOutForDelivery = "OUT FOR DELIVERY"
	StatusDelivered      = "DELIVERED"
	StatusReturnReq      = "RETURN REQUESTED"
	StatusReturnOK       = "RETURN APPROVED"
	StatusReturnNo       = "RETURN DECLINED"
	StatusCancelled      = "CANCELLED"
)

var orderFlow = map[string][]string{
	StatusToFulfil:       {StatusShipped},
	StatusShipped:        {StatusOutForDelivery, StatusDelivered},
	StatusOutForDelivery: {StatusDelivered},
	StatusReturnReq:      {StatusReturnOK, StatusReturnNo},
}

// CanTransition reports whether a brand may move an order between two statuses.
func CanTransition(from, to string) bool {
	for _, s := range orderFlow[from] {
		if s == to {
			return true
		}
	}
	return false
}

// ShopperStatus is the buyer's view of the single stored order status.
func ShopperStatus(status string) string {
	switch status {
	case StatusToFulfil, StatusPending:
		return "Confirmed"
	case StatusShipped:
		return "Shipped"
	case StatusOutForDelivery:
		return "Out for delivery"
	}
	return "Delivered"
}

// ShopperStage is the 1–4 progress used by the tracking screen.
func ShopperStage(status string) int {
	switch status {
	case StatusToFulfil, StatusPending:
		return 1
	case StatusShipped:
		return 2
	case StatusOutForDelivery:
		return 3
	}
	return 4
}

// ReturnStatus is the buyer-facing return state, empty when no return exists.
func ReturnStatus(status string) string {
	switch status {
	case StatusReturnReq:
		return "Requested"
	case StatusReturnOK:
		return "Approved"
	case StatusReturnNo:
		return "Declined"
	}
	return ""
}

// ReturnWindowDays parses a brand's returns setting. 0 means no returns.
func ReturnWindowDays(returns string) int {
	switch returns {
	case "14 days":
		return 14
	case "30 days":
		return 30
	}
	return 0
}

// IDs: a shopper sees "WW-20417", the studio sees "20417" and "#20417".
func ShopperOrderID(seq int64) string { return fmt.Sprintf("WW-%d", seq) }
func StudioOrderID(seq int64) string  { return strconv.FormatInt(seq, 10) }

// ParseOrderID accepts either id form.
func ParseOrderID(id string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimPrefix(id, "WW-"), 10, 64)
	return n, err == nil && n > 0
}

/* ---------- validation ---------- */

var (
	handleRe  = regexp.MustCompile(`^[a-z0-9._]{3,30}$`)
	emailRe   = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)
	phoneRe   = regexp.MustCompile(`^\+?[0-9]{8,15}$`)
	hashtagRe = regexp.MustCompile(`^#[A-Za-z0-9_]{1,40}$`)
	codeRe    = regexp.MustCompile(`^[A-Z0-9_-]{3,24}$`)
)

func ValidHandle(s string) bool  { return handleRe.MatchString(s) }
func ValidEmail(s string) bool   { return len(s) <= 254 && emailRe.MatchString(s) }
func ValidPhone(s string) bool   { return phoneRe.MatchString(s) }
func ValidHashtag(s string) bool { return hashtagRe.MatchString(s) }
func ValidPromo(s string) bool   { return codeRe.MatchString(s) }

// NormalizeHandle lowercases and strips a leading @.
func NormalizeHandle(s string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "@"))
}

// NormalizePhone keeps a leading + and digits.
func NormalizePhone(s string) string {
	var b strings.Builder
	for i, r := range strings.TrimSpace(s) {
		if (r == '+' && i == 0) || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Initials derives a brand's two-letter mark from its name.
func Initials(name string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(name) {
		if r >= 'A' && r <= 'Z' {
			b.WriteRune(r)
			if b.Len() == 2 {
				break
			}
		}
	}
	if b.Len() == 0 {
		return "WW"
	}
	return b.String()
}
