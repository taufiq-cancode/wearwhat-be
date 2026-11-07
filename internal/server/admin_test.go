package server_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"wearwhat-be/internal/admin"
)

// makeAdmin grants admin the way the admin tool does.
func (e *env) makeAdmin(c *client) {
	e.t.Helper()
	if _, err := admin.SetAdmin(context.Background(), e.pool, admin.CLI, c.email, true); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

type adminAction struct {
	Actor  string `json:"actor"`
	Action string `json:"action"`
	Target string `json:"target"`
	Detail string `json:"detail"`
}

func (c *client) recentActions() []adminAction {
	c.e.t.Helper()
	var ov struct {
		Recent []adminAction `json:"recent"`
	}
	c.ok("GET", "/admin/overview", nil, &ov)
	return ov.Recent
}

func TestAdminBrandReview(t *testing.T) {
	e := newEnv(t)
	owner := e.brandOwner()
	// development verifies brands instantly, so put this one back in the queue as production would
	e.exec(`UPDATE brands SET status = 'in_review', verified_at = NULL WHERE id = $1`, owner.brandID)
	adm := e.shopper()
	e.makeAdmin(adm)
	shopper := e.shopper()
	h := owner.brandHandle

	var me struct {
		Admin bool `json:"admin"`
	}
	adm.ok("GET", "/me", nil, &me)
	if !me.Admin {
		t.Error("/me should say admin")
	}
	shopper.ok("GET", "/me", nil, &me)
	if me.Admin {
		t.Error("a shopper isn't an admin")
	}

	handles := func(query string) []string {
		t.Helper()
		var list []struct {
			Handle string `json:"handle"`
		}
		adm.ok("GET", "/admin/brands"+query, nil, &list)
		out := []string{}
		for _, b := range list {
			out = append(out, b.Handle)
		}
		return out
	}
	if got := handles(""); len(got) != 1 || got[0] != h {
		t.Errorf("in review: %v", got)
	}
	if got := handles("?status=verified"); len(got) != 0 {
		t.Errorf("verified: %v", got)
	}
	adm.status(400, "GET", "/admin/brands?status=nope", nil)

	status, raw := adm.raw("GET", "/admin/brands/"+h, nil)
	if status != 200 {
		t.Fatalf("detail: %d %s", status, raw)
	}
	if bytes.Contains(raw, []byte("0123456789")) {
		t.Error("the full account number must never leave the server")
	}
	var detail struct {
		Status     string `json:"status"`
		OwnerEmail string `json:"ownerEmail"`
		Business   struct {
			Registration string `json:"registration"`
			DocumentType string `json:"documentType"`
		} `json:"business"`
		Payouts struct {
			Bank         string `json:"bank"`
			AccountLast4 string `json:"accountLast4"`
		} `json:"payouts"`
	}
	adm.ok("GET", "/admin/brands/"+h, nil, &detail)
	if detail.Status != "in_review" || detail.OwnerEmail != owner.email || detail.Business.Registration != "RC 123" ||
		detail.Business.DocumentType != "image/png" || detail.Payouts.Bank != "GTBank" || detail.Payouts.AccountLast4 != "6789" {
		t.Errorf("detail: %+v", detail)
	}
	adm.status(404, "GET", "/admin/brands/nobody.here", nil)

	// the document comes back readable and is never cached
	req, err := http.NewRequest("GET", e.srv.URL+api+"/admin/brands/"+h+"/document", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := adm.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 || !bytes.Equal(doc, pngBytes(t)) {
		t.Errorf("document: status %d, %d bytes", res.StatusCode, len(doc))
	}
	if res.Header.Get("Cache-Control") != "no-store" || res.Header.Get("Content-Type") != "image/png" {
		t.Errorf("document headers: %v", res.Header)
	}

	// admins can preview the storefront while shoppers can't see it
	shopper.status(404, "GET", "/brands/"+h, nil)
	adm.ok("GET", "/brands/"+h, nil, nil)

	adm.ok("POST", "/admin/brands/"+h+"/verify", m{}, nil)
	shopper.ok("GET", "/brands/"+h, nil, nil)
	if msg := adm.status(409, "POST", "/admin/brands/"+h+"/verify", m{}); !strings.Contains(msg, "already verified") {
		t.Errorf("verify twice: %q", msg)
	}
	if n := e.count(`SELECT count(*) FROM notifications WHERE brand_id = $1 AND text LIKE 'Your store is verified%'`, owner.brandID); n != 1 {
		t.Errorf("verified notifications: %d", n)
	}

	adm.ok("POST", "/admin/brands/"+h+"/reject", m{"note": "The document is blurry."}, nil)
	shopper.status(404, "GET", "/brands/"+h, nil)
	if n := e.count(`SELECT count(*) FROM notifications WHERE brand_id = $1 AND text = 'We couldn’t verify your store: The document is blurry. Contact support to review your documents.'`, owner.brandID); n != 1 {
		t.Errorf("rejection notifications with the note: %d", n)
	}
	adm.status(400, "POST", "/admin/brands/"+h+"/verify", nil) // the panel always sends a body

	recent := adm.recentActions()
	if len(recent) < 2 || recent[0] != (adminAction{"@" + adm.handle, "reject-brand", "@" + h, "The document is blurry."}) ||
		recent[1] != (adminAction{"@" + adm.handle, "verify-brand", "@" + h, ""}) {
		t.Errorf("recent actions: %+v", recent)
	}
}

func TestAdminReports(t *testing.T) {
	e := newEnv(t)
	owner := e.brandOwner()
	pid := owner.addProduct(5000, map[string]int{"M": 5})
	u := owner.upload("image", pngBytes(t))
	var tile, post, comment struct {
		ID string `json:"id"`
	}
	owner.ok("POST", "/studio/media", m{"url": u.URL}, &tile)
	owner.ok("POST", "/studio/posts", m{"type": "Post", "mediaIds": []string{tile.ID}, "productIds": []string{pid}, "caption": "reported caption", "when": "Now"}, &post)
	alice, bob := e.shopper(), e.shopper()
	bob.ok("POST", "/posts/"+post.ID+"/comments", m{"text": "rude comment"}, &comment)

	alice.ok("POST", "/reports", m{"kind": "post", "id": post.ID, "reason": "spam"}, nil)
	bob.ok("POST", "/reports", m{"kind": "post", "id": post.ID}, nil)
	alice.ok("POST", "/reports", m{"kind": "comment", "id": comment.ID, "reason": "rude"}, nil)
	alice.ok("POST", "/reports", m{"kind": "user", "id": bob.id}, nil)
	bob.ok("DELETE", "/comments/"+comment.ID, nil, nil) // deleted after it was reported

	adm := e.shopper()
	e.makeAdmin(adm)
	type report struct {
		ID       string `json:"id"`
		Kind     string `json:"kind"`
		Reporter string `json:"reporter"`
		Reason   string `json:"reason"`
		Open     int    `json:"open"`
		Target   struct {
			Gone       bool   `json:"gone"`
			Summary    string `json:"summary"`
			Handle     string `json:"handle"`
			HandleKind string `json:"handleKind"`
			PostID     string `json:"postId"`
		} `json:"target"`
	}
	var list []report
	adm.ok("GET", "/admin/reports", nil, &list)
	if len(list) != 4 {
		t.Fatalf("reports: %+v", list)
	}
	first, comm, user := list[0], list[2], list[3]
	if first.Kind != "post" || first.Reporter != alice.handle || first.Reason != "spam" || first.Open != 2 ||
		first.Target.PostID != post.ID || first.Target.Summary != "reported caption" || first.Target.Handle != owner.brandHandle || first.Target.HandleKind != "brand" {
		t.Errorf("post report: %+v", first)
	}
	if comm.Kind != "comment" || !comm.Target.Gone {
		t.Errorf("deleted comment: %+v", comm)
	}
	if user.Kind != "user" || user.Target.Handle != bob.handle || user.Target.HandleKind != "user" {
		t.Errorf("user report: %+v", user)
	}

	adm.ok("POST", "/admin/reports/"+first.ID+"/resolve", nil, nil)
	adm.status(404, "POST", "/admin/reports/"+first.ID+"/resolve", nil)
	adm.ok("GET", "/admin/reports", nil, &list)
	if len(list) != 3 || list[0].Open != 1 {
		t.Errorf("after resolving: %+v", list)
	}
	if got := adm.recentActions(); len(got) == 0 || got[0] != (adminAction{"@" + adm.handle, "resolve-report", first.ID, "post"}) {
		t.Errorf("recent: %+v", got)
	}
}

func TestAdminMoney(t *testing.T) {
	e := newEnv(t)
	owner := e.brandOwner()
	pid := owner.addProduct(10000, map[string]int{"M": 10})
	buyer := e.shopper().withAddress()
	order := buyer.buy(pid, "M").OrderIDs[0]
	owner.deliver(order)
	adm := e.shopper()
	e.makeAdmin(adm)

	type money struct {
		Payouts []struct {
			Handle string `json:"handle"`
			Orders int    `json:"orders"`
			Amount int    `json:"amount"`
		} `json:"payouts"`
		Refunds []struct {
			Order  string `json:"order"`
			Amount int    `json:"amount"`
		} `json:"refunds"`
		Attention []any `json:"attention"`
	}
	var got money
	adm.ok("GET", "/admin/money", nil, &got)
	if len(got.Payouts) != 0 || got.Refunds == nil || got.Attention == nil {
		t.Fatalf("just delivered, so nothing is due yet: %+v", got)
	}

	e.exec(`UPDATE orders SET delivered_at = now() - interval '3 days' WHERE brand_id = $1`, owner.brandID)
	adm.ok("GET", "/admin/money", nil, &got)
	if len(got.Payouts) != 1 || got.Payouts[0].Handle != owner.brandHandle || got.Payouts[0].Orders != 1 || got.Payouts[0].Amount <= 0 {
		t.Fatalf("payout due: %+v", got.Payouts)
	}
	amount := got.Payouts[0].Amount
	var ov struct {
		PayoutBrands int `json:"payoutBrands"`
		PayoutTotal  int `json:"payoutTotal"`
	}
	adm.ok("GET", "/admin/overview", nil, &ov)
	if ov.PayoutBrands != 1 || ov.PayoutTotal != amount {
		t.Errorf("overview: %+v", ov)
	}

	paid := "/admin/payouts/" + owner.brandHandle + "/paid"
	adm.status(400, "POST", paid, m{})
	// a different amount means something changed since the admin looked: nothing is marked
	adm.status(409, "POST", paid, m{"amount": amount + 1})
	if n := e.count(`SELECT count(*) FROM orders WHERE brand_id = $1 AND payout_paid_at IS NOT NULL`, owner.brandID); n != 0 {
		t.Fatalf("marked despite the mismatch: %d", n)
	}
	var res struct {
		Orders int `json:"orders"`
		Amount int `json:"amount"`
	}
	adm.ok("POST", paid, m{"amount": amount}, &res)
	if res.Orders != 1 || res.Amount != amount {
		t.Errorf("paid: %+v", res)
	}
	adm.ok("GET", "/admin/money", nil, &got)
	if len(got.Payouts) != 0 {
		t.Errorf("still due: %+v", got.Payouts)
	}

	e.exec(`UPDATE orders SET refund_status = 'due' WHERE brand_id = $1`, owner.brandID)
	adm.ok("GET", "/admin/money", nil, &got)
	if len(got.Refunds) != 1 || got.Refunds[0].Order != order {
		t.Fatalf("refunds: %+v", got.Refunds)
	}
	adm.ok("POST", "/admin/refunds/"+order+"/done", nil, nil)
	adm.status(404, "POST", "/admin/refunds/"+order+"/done", nil)
	adm.status(400, "POST", "/admin/refunds/nonsense/done", nil)
	if recent := adm.recentActions(); len(recent) < 2 || recent[0].Action != "mark-refund-done" || recent[1].Action != "mark-payout-paid" {
		t.Errorf("recent: %+v", recent)
	}
}

func TestRemovingAdminTakesEffectAtOnce(t *testing.T) {
	e := newEnv(t)
	adm := e.shopper()
	e.makeAdmin(adm)
	adm.ok("GET", "/admin/overview", nil, nil)
	if _, err := admin.SetAdmin(context.Background(), e.pool, admin.CLI, strings.ToUpper(adm.email), false); err != nil {
		t.Fatal(err)
	}
	adm.status(404, "GET", "/admin/overview", nil)
	if _, err := admin.SetAdmin(context.Background(), e.pool, admin.CLI, "nobody@test.dev", true); err == nil {
		t.Error("granting admin to an email with no account should fail")
	}
}
