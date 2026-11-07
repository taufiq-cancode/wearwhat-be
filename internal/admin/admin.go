// Package admin is the work wearwhat's operators do: verifying brands, handling reports and
// recording money sent by hand. The admin panel's routes and the admin tool both call it, and
// every change is written to admin_actions with who made it.
package admin

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"wearwhat-be/internal/db"
	"wearwhat-be/internal/domain"
	"wearwhat-be/internal/httpx"
	"wearwhat-be/internal/inbox"
	"wearwhat-be/internal/media"
	"wearwhat-be/internal/secure"
)

// CLI is the actor recorded for changes made with the admin tool.
const CLI = "cli"

// Brand review states.
const (
	InReview = "in_review"
	Verified = "verified"
	Rejected = "rejected"
)

// payoutDueWhere is an order whose payout is owed: delivered over 2 days ago (the return
// window's first stretch) and not yet paid. Alias o = orders.
const payoutDueWhere = `o.status = 'DELIVERED' AND o.payout_paid_at IS NULL AND o.delivered_at < now() - interval '2 days'`

// ErrChanged means the amount due moved between viewing it and marking it paid.
var ErrChanged = httpx.Conflict("The amount due has changed. Refresh and check it again.")

func record(ctx context.Context, q db.Q, actor, action, target, detail string) error {
	_, err := q.Exec(ctx, `INSERT INTO admin_actions (id, actor, action, target, detail) VALUES ($1,$2,$3,$4,$5)`,
		secure.ID("aa"), actor, action, target, detail)
	return err
}

/* ---------- overview ---------- */

type Action struct {
	Actor  string    `json:"actor"`
	Action string    `json:"action"`
	Target string    `json:"target"`
	Detail string    `json:"detail"`
	At     time.Time `json:"at"`
}

type Overview struct {
	BrandsInReview int      `json:"brandsInReview"`
	OpenReports    int      `json:"openReports"`
	PayoutBrands   int      `json:"payoutBrands"`
	PayoutTotal    int      `json:"payoutTotal"` // cents
	RefundsDue     int      `json:"refundsDue"`
	NeedsAttention int      `json:"needsAttention"`
	Recent         []Action `json:"recent"`
}

func GetOverview(ctx context.Context, q db.Q) (Overview, error) {
	var o Overview
	err := q.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM brands WHERE status = 'in_review'),
			(SELECT count(*) FROM reports WHERE resolved_at IS NULL),
			(SELECT count(DISTINCT o.brand_id) FROM orders o WHERE `+payoutDueWhere+`),
			(SELECT COALESCE(sum(o.payout), 0) FROM orders o WHERE `+payoutDueWhere+`),
			(SELECT count(*) FROM orders WHERE refund_status = 'due'),
			(SELECT count(*) FROM checkouts WHERE status IN ('paid_after_expiry', 'amount_mismatch'))`).
		Scan(&o.BrandsInReview, &o.OpenReports, &o.PayoutBrands, &o.PayoutTotal, &o.RefundsDue, &o.NeedsAttention)
	if err != nil {
		return o, err
	}
	o.Recent, err = RecentActions(ctx, q, 20)
	return o, err
}

func RecentActions(ctx context.Context, q db.Q, limit int) ([]Action, error) {
	rows, err := q.Query(ctx, `SELECT actor, action, target, detail, at FROM admin_actions ORDER BY at DESC, id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Action{}
	for rows.Next() {
		var a Action
		if err := rows.Scan(&a.Actor, &a.Action, &a.Target, &a.Detail, &a.At); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

/* ---------- brands ---------- */

type BrandRow struct {
	Handle      string        `json:"handle"`
	Name        string        `json:"name"`
	Initials    string        `json:"initials"`
	Avatar      *domain.Photo `json:"avatar"`
	Status      string        `json:"status"`
	LegalName   string        `json:"legalName"`
	Type        string        `json:"type"`
	SubmittedAt time.Time     `json:"submittedAt"`
}

const brandRowCols = `b.handle, b.name, b.initials, b.avatar, b.status,
	COALESCE(b.business ->> 'legalName', ''), COALESCE(b.business ->> 'type', ''), b.created_at`

func scanBrandRow(row pgx.Row, b *BrandRow, extra ...any) error {
	return row.Scan(append([]any{&b.Handle, &b.Name, &b.Initials, &b.Avatar, &b.Status, &b.LegalName, &b.Type, &b.SubmittedAt}, extra...)...)
}

// Brands lists brands in one review state. The review queue is oldest first, the others newest first.
func Brands(ctx context.Context, q db.Q, status string) ([]BrandRow, error) {
	if status != InReview && status != Verified && status != Rejected {
		return nil, httpx.BadRequest("Choose in_review, verified or rejected")
	}
	order := `b.created_at DESC`
	if status == InReview {
		order = `b.created_at`
	}
	rows, err := q.Query(ctx, `SELECT `+brandRowCols+` FROM brands b WHERE b.status = $1 ORDER BY `+order+`, b.id LIMIT 200`, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BrandRow{}
	for rows.Next() {
		var b BrandRow
		if err := scanBrandRow(rows, &b); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

type BrandDetail struct {
	BrandRow
	ID          string     `json:"id"`
	Bio         string     `json:"bio"`
	Location    string     `json:"location"`
	Categories  []string   `json:"categories"`
	ShipsFrom   string     `json:"shipsFrom"`
	Products    int        `json:"products"`
	VerifiedAt  *time.Time `json:"verifiedAt"`
	OwnerHandle string     `json:"ownerHandle"`
	OwnerEmail  string     `json:"ownerEmail"`
	Business    struct {
		Registration string `json:"registration"`
		// DocumentType is the document's content type, or empty when there's none on file.
		DocumentType string `json:"documentType"`
	} `json:"business"`
	Payouts struct {
		Provider string `json:"provider"`
		Bank     string `json:"bank"`
		Schedule string `json:"schedule"`
		// AccountLast4 is all an admin needs to match a transfer, so the full number never leaves the server.
		AccountLast4 string `json:"accountLast4"`
	} `json:"payouts"`
}

// Brand is everything needed to review one brand. The registration number is decrypted, because
// checking it is the point of the review.
func Brand(ctx context.Context, q db.Q, key []byte, handle string) (BrandDetail, error) {
	var b BrandDetail
	var regEnc, accountEnc string
	err := scanBrandRow(q.QueryRow(ctx, `SELECT `+brandRowCols+`, b.id, b.bio, b.location, b.categories,
			COALESCE(b.shipping ->> 'shipsFrom', ''), b.verified_at,
			(SELECT count(*) FROM products p WHERE p.brand_id = b.id),
			COALESCE(u.handle, ''), COALESCE(u.email, ''),
			COALESCE(b.business ->> 'registrationEnc', ''), COALESCE(up.content_type, ''),
			COALESCE(b.payouts ->> 'provider', ''), COALESCE(b.payouts ->> 'bank', ''), COALESCE(b.payouts ->> 'schedule', ''),
			COALESCE(b.payouts ->> 'accountEnc', '')
		FROM brands b LEFT JOIN users u ON u.id = b.owner_id
		LEFT JOIN uploads up ON up.id = b.business ->> 'documentId'
		WHERE b.handle = $1`, domain.NormalizeHandle(handle)), &b.BrandRow,
		&b.ID, &b.Bio, &b.Location, &b.Categories, &b.ShipsFrom, &b.VerifiedAt, &b.Products,
		&b.OwnerHandle, &b.OwnerEmail, &regEnc, &b.Business.DocumentType,
		&b.Payouts.Provider, &b.Payouts.Bank, &b.Payouts.Schedule, &accountEnc)
	if db.IsNotFound(err) {
		return b, httpx.NotFound("No brand @" + domain.NormalizeHandle(handle))
	}
	if err != nil {
		return b, err
	}
	if b.Categories == nil {
		b.Categories = []string{}
	}
	// a value that won't decrypt is shown as such rather than failing the page: the rest is still worth seeing
	const unreadable = "(can’t decrypt: DATA_KEY isn’t the one it was saved with)"
	if reg, ok := reveal(key, regEnc); ok {
		b.Business.Registration = reg
	} else {
		b.Business.Registration = unreadable
	}
	if account, ok := reveal(key, accountEnc); ok {
		b.Payouts.AccountLast4 = account[max(0, len(account)-4):]
	} else {
		b.Payouts.AccountLast4 = unreadable
	}
	return b, nil
}

// reveal decrypts a stored secret. An empty value is fine; one that won't decrypt is not.
func reveal(key []byte, sealed string) (string, bool) {
	if sealed == "" {
		return "", true
	}
	plain, err := secure.Decrypt(key, sealed)
	return plain, err == nil
}

// Document returns a readable copy of a brand's private document.
func Document(ctx context.Context, q db.Q, store media.Storage, key []byte, handle string) (*httpx.File, error) {
	handle = domain.NormalizeHandle(handle)
	var path, ctype string
	err := q.QueryRow(ctx, `SELECT u.path, u.content_type FROM brands b JOIN uploads u ON u.id = b.business ->> 'documentId' WHERE b.handle = $1`, handle).Scan(&path, &ctype)
	if db.IsNotFound(err) {
		return nil, httpx.NotFound("No document on file for @" + handle)
	}
	if err != nil {
		return nil, err
	}
	fetch, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	data, err := media.ReadDocument(fetch, store, key, path)
	if err != nil {
		return nil, err
	}
	return &httpx.File{ContentType: ctype, Name: handle + "-document" + media.Extension(ctype), Data: data}, nil
}

// SetBrandStatus approves or rejects a brand and tells its studio. A note on a rejection is passed on.
func SetBrandStatus(ctx context.Context, pool *pgxpool.Pool, actor, handle, status, note string) (string, error) {
	if status != Verified && status != Rejected {
		return "", fmt.Errorf("unknown brand status %q", status)
	}
	handle = domain.NormalizeHandle(handle)
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) > 300 {
		return "", httpx.BadRequest("Keep the note under 300 characters")
	}
	var name string
	err := db.Tx(ctx, pool, func(tx pgx.Tx) error {
		var id, current string
		err := tx.QueryRow(ctx, `SELECT id, name, status FROM brands WHERE handle = $1 FOR UPDATE`, handle).Scan(&id, &name, &current)
		if db.IsNotFound(err) {
			return httpx.NotFound("No brand @" + handle)
		}
		if err != nil {
			return err
		}
		if current == status {
			return httpx.Conflict(fmt.Sprintf("@%s is already %s", handle, status))
		}
		if _, err := tx.Exec(ctx, `UPDATE brands SET status = $2, verified_at = CASE WHEN $2 = 'verified' THEN now() ELSE NULL END WHERE id = $1`, id, status); err != nil {
			return err
		}
		text := "Your store is verified and live on wearwhat."
		if status == Rejected {
			text = "We couldn’t verify your store. Contact support to review your documents."
			if note != "" {
				text = "We couldn’t verify your store: " + strings.TrimRight(note, ".") + ". Contact support to review your documents."
			}
		}
		if err := inbox.Notify(ctx, tx, inbox.Note{BrandID: id, Who: "wearwhat", Text: text, Kind: "Orders", Href: "/studio/settings"}); err != nil {
			return err
		}
		action := "verify-brand"
		if status == Rejected {
			action = "reject-brand"
		}
		return record(ctx, tx, actor, action, "@"+handle, note)
	})
	return name, err
}

/* ---------- reports ---------- */

type ReportTarget struct {
	// Gone is true when the reported thing has since been deleted.
	Gone    bool   `json:"gone"`
	Summary string `json:"summary"`
	// Handle is who the target belongs to: the account itself, or a post's or comment's author.
	Handle     string `json:"handle,omitempty"`
	HandleKind string `json:"handleKind,omitempty"` // user | brand
	// PostID is the post to open, for a post or a comment.
	PostID string `json:"postId,omitempty"`
}

type Report struct {
	ID       string       `json:"id"`
	Kind     string       `json:"kind"` // post | comment | user | brand | thread
	TargetID string       `json:"targetId"`
	Reporter string       `json:"reporter"`
	Reason   string       `json:"reason"`
	At       time.Time    `json:"at"`
	Open     int          `json:"open"` // open reports on the same target, this one included
	Target   ReportTarget `json:"target"`
}

// Reports lists unresolved reports, oldest first, with enough about each target to judge it.
func Reports(ctx context.Context, q db.Q) ([]Report, error) {
	rows, err := q.Query(ctx, `SELECT r.id, r.target_kind, r.target_id, u.handle, r.reason, r.created_at,
			(SELECT count(*) FROM reports x WHERE x.target_kind = r.target_kind AND x.target_id = r.target_id AND x.resolved_at IS NULL)
		FROM reports r JOIN users u ON u.id = r.reporter_id WHERE r.resolved_at IS NULL ORDER BY r.created_at, r.id LIMIT 200`)
	if err != nil {
		return nil, err
	}
	out := []Report{}
	for rows.Next() {
		var r Report
		if err := rows.Scan(&r.ID, &r.Kind, &r.TargetID, &r.Reporter, &r.Reason, &r.At, &r.Open); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// one lookup per report: the list is short, and each kind lives in a different table
	for i := range out {
		if out[i].Target, err = target(ctx, q, out[i].Kind, out[i].TargetID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func target(ctx context.Context, q db.Q, kind, id string) (ReportTarget, error) {
	var t ReportTarget
	var err error
	switch kind {
	case "post":
		var user, brand *string
		err = q.QueryRow(ctx, `SELECT p.id, left(p.caption, 200), u.handle, b.handle FROM posts p
			LEFT JOIN users u ON u.id = p.user_id LEFT JOIN brands b ON b.id = p.brand_id WHERE p.id = $1`, id).
			Scan(&t.PostID, &t.Summary, &user, &brand)
		t.Handle, t.HandleKind = owner(user, brand)
	case "comment":
		t.HandleKind = "user"
		err = q.QueryRow(ctx, `SELECT c.post_id, left(c.text, 200), u.handle FROM comments c JOIN users u ON u.id = c.user_id WHERE c.id = $1`, id).
			Scan(&t.PostID, &t.Summary, &t.Handle)
	case "user":
		t.HandleKind = "user"
		err = q.QueryRow(ctx, `SELECT handle, name FROM users WHERE id = $1`, id).Scan(&t.Handle, &t.Summary)
	case "brand":
		t.HandleKind = "brand"
		err = q.QueryRow(ctx, `SELECT handle, name FROM brands WHERE id = $1`, id).Scan(&t.Handle, &t.Summary)
	case "thread":
		// who's in it, never what was said: messages stay private even from admins
		var a string
		var other *string
		err = q.QueryRow(ctx, `SELECT a.handle, COALESCE(b.handle, bu.handle) FROM threads t JOIN users a ON a.id = t.a_user_id
			LEFT JOIN brands b ON b.id = t.brand_id LEFT JOIN users bu ON bu.id = t.b_user_id WHERE t.id = $1`, id).Scan(&a, &other)
		if err == nil {
			t.Summary = "Conversation between @" + a + " and @" + deref(other)
		}
	default:
		t.Summary = "Unknown kind of report"
		return t, nil
	}
	if db.IsNotFound(err) {
		return ReportTarget{Gone: true, Summary: "Deleted since it was reported"}, nil
	}
	return t, err
}

func owner(user, brand *string) (string, string) {
	if user != nil {
		return *user, "user"
	}
	if brand != nil {
		return *brand, "brand"
	}
	return "", ""
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ResolveReport marks one open report handled.
func ResolveReport(ctx context.Context, pool *pgxpool.Pool, actor, id string) error {
	return db.Tx(ctx, pool, func(tx pgx.Tx) error {
		var kind string
		err := tx.QueryRow(ctx, `UPDATE reports SET resolved_at = now() WHERE id = $1 AND resolved_at IS NULL RETURNING target_kind`, id).Scan(&kind)
		if db.IsNotFound(err) {
			return httpx.NotFound("No open report " + id)
		}
		if err != nil {
			return err
		}
		return record(ctx, tx, actor, "resolve-report", id, kind)
	})
}

/* ---------- money ---------- */

type PayoutDue struct {
	Handle   string `json:"handle"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Bank     string `json:"bank"`
	Orders   int    `json:"orders"`
	Amount   int    `json:"amount"` // cents
}

type RefundDue struct {
	Order    string `json:"order"` // WW-20417
	Brand    string `json:"brand"`
	Customer string `json:"customer"`
	Amount   int    `json:"amount"` // cents
}

type Attention struct {
	Checkout string    `json:"checkout"`
	Problem  string    `json:"problem"` // paid_after_expiry | amount_mismatch
	Email    string    `json:"email"`
	Total    int       `json:"total"` // cents
	At       time.Time `json:"at"`
}

type Money struct {
	Payouts   []PayoutDue `json:"payouts"`
	Refunds   []RefundDue `json:"refunds"`
	Attention []Attention `json:"attention"`
}

// GetMoney is what's owed to brands, refunds due, and payments that arrived but weren't fulfilled.
func GetMoney(ctx context.Context, q db.Q) (Money, error) {
	m := Money{Payouts: []PayoutDue{}, Refunds: []RefundDue{}, Attention: []Attention{}}
	rows, err := q.Query(ctx, `SELECT b.handle, b.name, COALESCE(b.payouts ->> 'provider', ''), COALESCE(b.payouts ->> 'bank', ''), count(*), sum(o.payout)
		FROM orders o JOIN brands b ON b.id = o.brand_id WHERE `+payoutDueWhere+` GROUP BY b.id ORDER BY sum(o.payout) DESC, b.handle`)
	if err != nil {
		return m, err
	}
	for rows.Next() {
		var p PayoutDue
		if err := rows.Scan(&p.Handle, &p.Name, &p.Provider, &p.Bank, &p.Orders, &p.Amount); err != nil {
			rows.Close()
			return m, err
		}
		m.Payouts = append(m.Payouts, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return m, err
	}

	rows, err = q.Query(ctx, `SELECT o.seq, b.handle, u.handle, o.subtotal - o.discount + o.shipping FROM orders o
		JOIN brands b ON b.id = o.brand_id JOIN users u ON u.id = o.user_id WHERE o.refund_status = 'due' ORDER BY o.seq`)
	if err != nil {
		return m, err
	}
	for rows.Next() {
		var r RefundDue
		var seq int64
		if err := rows.Scan(&seq, &r.Brand, &r.Customer, &r.Amount); err != nil {
			rows.Close()
			return m, err
		}
		r.Order = domain.ShopperOrderID(seq)
		m.Refunds = append(m.Refunds, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return m, err
	}

	rows, err = q.Query(ctx, `SELECT number, status, email, total, created_at FROM checkouts WHERE status IN ('paid_after_expiry', 'amount_mismatch') ORDER BY created_at`)
	if err != nil {
		return m, err
	}
	defer rows.Close()
	for rows.Next() {
		var a Attention
		if err := rows.Scan(&a.Checkout, &a.Problem, &a.Email, &a.Total, &a.At); err != nil {
			return m, err
		}
		m.Attention = append(m.Attention, a)
	}
	return m, rows.Err()
}

// MarkPayoutPaid records that a brand has been paid everything currently due. With expect set, it
// only does so if that's still the amount due, so an order that comes due in the meantime isn't
// marked paid unseen. It returns how many orders were marked and their total.
func MarkPayoutPaid(ctx context.Context, pool *pgxpool.Pool, actor, handle string, expect *int) (int, int, error) {
	handle = domain.NormalizeHandle(handle)
	var orders, total int
	err := db.Tx(ctx, pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `UPDATE orders o SET payout_paid_at = now() FROM brands b
			WHERE b.id = o.brand_id AND b.handle = $1 AND `+payoutDueWhere+` RETURNING o.payout`, handle)
		if err != nil {
			return err
		}
		for rows.Next() {
			var p int
			if err := rows.Scan(&p); err != nil {
				rows.Close()
				return err
			}
			orders++
			total += p
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if expect != nil && *expect != total {
			return ErrChanged // rolls the update back
		}
		if orders == 0 {
			return nil
		}
		return record(ctx, tx, actor, "mark-payout-paid", "@"+handle, fmt.Sprintf("%d orders, %s", orders, Dollars(total)))
	})
	if err != nil {
		return 0, 0, err
	}
	return orders, total, nil
}

// MarkRefundDone records that an approved return has been refunded.
func MarkRefundDone(ctx context.Context, pool *pgxpool.Pool, actor, order string) (string, error) {
	seq, ok := domain.ParseOrderID(order)
	if !ok {
		return "", httpx.BadRequest(fmt.Sprintf("%q isn't an order id", order))
	}
	id := domain.ShopperOrderID(seq)
	err := db.Tx(ctx, pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE orders SET refund_status = 'done' WHERE seq = $1 AND refund_status = 'due'`, seq)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return httpx.NotFound("No refund due on " + id)
		}
		return record(ctx, tx, actor, "mark-refund-done", id, "")
	})
	return id, err
}

// Dollars formats cents the way the admin tool always has.
func Dollars(cents int) string { return fmt.Sprintf("$%.2f", float64(cents)/100) }

/* ---------- admins ---------- */

type Admin struct {
	Handle string `json:"handle"`
	Name   string `json:"name"`
	Email  string `json:"email"`
}

// SetAdmin grants or removes admin from the account with this email, and returns its handle.
func SetAdmin(ctx context.Context, pool *pgxpool.Pool, actor, email string, on bool) (string, error) {
	email = strings.TrimSpace(email)
	var handle string
	err := db.Tx(ctx, pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `UPDATE users SET is_admin = $2 WHERE lower(email) = lower($1) RETURNING handle`, email, on).Scan(&handle)
		if db.IsNotFound(err) {
			return fmt.Errorf("no account with the email %s: sign up first", email)
		}
		if err != nil {
			return err
		}
		action := "make-admin"
		if !on {
			action = "remove-admin"
		}
		return record(ctx, tx, actor, action, "@"+handle, "")
	})
	return handle, err
}

func Admins(ctx context.Context, q db.Q) ([]Admin, error) {
	rows, err := q.Query(ctx, `SELECT handle, name, COALESCE(email, '') FROM users WHERE is_admin ORDER BY handle`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Admin{}
	for rows.Next() {
		var a Admin
		if err := rows.Scan(&a.Handle, &a.Name, &a.Email); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
