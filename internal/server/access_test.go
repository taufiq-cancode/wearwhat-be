package server_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Every /api/v1 route registered must require sign-in unless it's listed here on purpose.
var publicRoutes = map[string]bool{
	"POST /auth/signup": true, "POST /auth/verify": true, "POST /auth/resend": true, "POST /auth/login": true,
	"POST /auth/logout": true, "POST /auth/forgot": true, "POST /auth/reset": true, "POST /auth/apple": true,
	"GET /handles/x/available": true, "GET /config": true, "GET /brands": true, "GET /team/invites/x": true,
}

var userRoutes = []string{
	"GET /me", "PATCH /me", "GET /me/addresses", "POST /me/addresses", "PATCH /me/addresses/x", "DELETE /me/addresses/x",
	"GET /me/payment-methods", "POST /me/payment-methods/x/default", "DELETE /me/payment-methods/x", "GET /me/blocks",
	"GET /users/x", "PUT /users/x/follow", "DELETE /users/x/follow", "PUT /users/x/block", "DELETE /users/x/block",
	"GET /explore", "GET /explore/trends", "GET /products", "GET /products/x", "PUT /products/x/save", "DELETE /products/x/save",
	"POST /products/x/stock-alerts", "POST /products/x/reviews", "GET /brands/x", "PUT /brands/x/follow", "DELETE /brands/x/follow",
	"GET /feed", "GET /posts/x", "PUT /posts/x/like", "DELETE /posts/x/like", "PUT /posts/x/save", "DELETE /posts/x/save", "DELETE /posts/x",
	"GET /posts/x/comments", "POST /posts/x/comments", "DELETE /comments/x", "GET /me/wears", "GET /me/saved", "GET /me/closet", "POST /wears",
	"POST /reports", "POST /events", "GET /cart", "POST /cart/items", "PATCH /cart/items/x", "DELETE /cart/items/x",
	"PUT /cart/promo", "DELETE /cart/promo", "POST /checkout", "GET /checkouts/x", "GET /me/orders", "GET /orders/x", "POST /orders/x/return",
	"GET /threads", "POST /threads", "GET /threads/x", "POST /threads/x/read", "POST /threads/x/messages",
	"GET /notifications", "GET /notifications/unread-count", "POST /notifications/read", "POST /notifications/x/action",
	"POST /uploads", "POST /studio/onboarding", "POST /team/invites/accept",
}

var studioRoutes = []string{
	"GET /studio/metrics", "GET /studio/orders", "GET /studio/orders/1", "PATCH /studio/orders/1", "GET /studio/catalog",
	"PATCH /studio/products/x/stock", "GET /studio/products/x", "POST /studio/products", "PATCH /studio/products/x",
	"GET /studio/promotions", "POST /studio/promotions", "PATCH /studio/promotions/X", "GET /studio/drops/next", "PUT /studio/drops/next",
	"GET /studio/media", "POST /studio/media", "POST /studio/posts", "GET /studio/settings", "PUT /studio/settings/store",
	"PUT /studio/settings/payouts", "PUT /studio/settings/shipping", "POST /studio/team/invites", "DELETE /studio/team/x",
	"GET /studio/threads", "POST /studio/threads", "GET /studio/threads/x", "POST /studio/threads/x/read", "POST /studio/threads/x/messages",
	"GET /studio/notifications", "GET /studio/notifications/unread-count", "POST /studio/notifications/read", "POST /studio/notifications/x/action",
}

var adminRoutes = []string{
	"GET /admin/overview", "GET /admin/brands", "GET /admin/brands/x", "GET /admin/brands/x/document",
	"POST /admin/brands/x/verify", "POST /admin/brands/x/reject", "GET /admin/reports", "POST /admin/reports/x/resolve",
	"GET /admin/money", "POST /admin/payouts/x/paid", "POST /admin/refunds/WW-1/done",
}

func split(route string) (string, string) {
	parts := strings.SplitN(route, " ", 2)
	return parts[0], parts[1]
}

func body(method string) any {
	if method == "GET" || method == "DELETE" {
		return nil
	}
	return m{}
}

func TestSignedOutGets401Everywhere(t *testing.T) {
	e := newEnv(t)
	anon := e.anon()
	for _, route := range slices.Concat(userRoutes, studioRoutes, adminRoutes) {
		method, path := split(route)
		if got, raw := anon.raw(method, path, body(method)); got != 401 {
			t.Errorf("%s: status %d, want 401: %s", route, got, raw)
		}
	}
}

func TestAdminRoutesAreHiddenFromEveryoneElse(t *testing.T) {
	// a shopper and a brand owner (whose team role is the highest there is) both get "not found"
	e := newEnv(t)
	for _, c := range []*client{e.shopper(), e.brandOwner()} {
		for _, route := range adminRoutes {
			method, path := split(route)
			if got, raw := c.raw(method, path, body(method)); got != 404 {
				t.Errorf("%s: status %d, want 404: %s", route, got, raw)
			}
		}
	}
}

func TestStudioRoutesNeedABrand(t *testing.T) {
	e := newEnv(t)
	shopper := e.shopper()
	for _, route := range studioRoutes {
		method, path := split(route)
		if got, raw := shopper.raw(method, path, body(method)); got != 403 {
			t.Errorf("%s: status %d, want 403: %s", route, got, raw)
		}
	}
}

func TestNoUnlistedRouteIsPublic(t *testing.T) {
	// guards against a new route being registered as Public by accident
	for route := range publicRoutes {
		for _, u := range userRoutes {
			if u == route {
				t.Errorf("%s is listed as both public and signed-in", route)
			}
		}
	}
}

// fixture: two shoppers and two brands, with a real order and conversation for the first of each.
type fixture struct {
	e                *env
	alice, bob       *client
	brandA, brandB   *client
	productA, orderA string
	lineA            string
	addressA         string
	threadA          string
	checkoutA        string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	e := newEnv(t)
	f := &fixture{e: e, alice: e.shopper().withAddress(), bob: e.shopper().withAddress(), brandA: e.brandOwner(), brandB: e.brandOwner()}
	f.productA = f.brandA.addProduct(10000, map[string]int{"M": 10})
	res := f.alice.buy(f.productA, "M")
	f.orderA, f.checkoutA = res.OrderIDs[0], res.Number

	var cart struct {
		Lines []struct {
			ID string `json:"id"`
		} `json:"lines"`
	}
	f.alice.ok("POST", "/cart/items", m{"productId": f.productA, "size": "M", "color": "Black"}, &cart)
	f.lineA = cart.Lines[0].ID

	var addrs []struct {
		ID string `json:"id"`
	}
	f.alice.ok("GET", "/me/addresses", nil, &addrs)
	f.addressA = addrs[0].ID

	var th struct {
		ID string `json:"id"`
	}
	f.alice.ok("POST", "/threads", m{"brandId": f.brandA.brandID}, &th)
	f.threadA = th.ID
	f.alice.ok("POST", "/threads/"+th.ID+"/messages", m{"text": "hello"}, nil)
	return f
}

func TestShoppersCantReachEachOthersThings(t *testing.T) {
	f := newFixture(t)
	bob := f.bob
	cases := []struct {
		method, path string
		body         any
	}{
		{"GET", "/orders/" + f.orderA, nil},
		{"POST", "/orders/" + f.orderA + "/return", m{"note": "mine now"}},
		{"GET", "/checkouts/" + f.checkoutA, nil},
		{"PATCH", "/cart/items/" + f.lineA, m{"qty": 5}},
		{"PATCH", "/me/addresses/" + f.addressA, m{"name": "x", "line1": "y", "city": "z"}},
		{"DELETE", "/me/addresses/" + f.addressA, nil},
		{"GET", "/threads/" + f.threadA, nil},
		{"POST", "/threads/" + f.threadA + "/read", nil},
		{"POST", "/threads/" + f.threadA + "/messages", m{"text": "intruder"}},
		{"POST", "/products/" + f.productA + "/reviews", m{"orderId": f.orderA, "rating": 1, "text": "fake"}},
	}
	for _, c := range cases {
		if got, raw := bob.raw(c.method, c.path, c.body); got != 404 {
			t.Errorf("%s %s: status %d, want 404: %s", c.method, c.path, got, raw)
		}
	}
	// removing someone else's bag line is a quiet no-op, and must not touch it
	bob.raw("DELETE", "/cart/items/"+f.lineA, nil)
	if n := f.e.count(`SELECT count(*) FROM cart_lines WHERE id = $1`, f.lineA); n != 1 {
		t.Error("Bob removed Alice's bag line")
	}
	// nothing leaked into Bob's own lists
	var orders []any
	bob.ok("GET", "/me/orders", nil, &orders)
	if len(orders) != 0 {
		t.Errorf("Bob sees %d orders", len(orders))
	}
	var threads []any
	bob.ok("GET", "/threads", nil, &threads)
	if len(threads) != 0 {
		t.Errorf("Bob sees %d threads", len(threads))
	}
	var msgs struct {
		Messages []struct {
			Text string `json:"text"`
		} `json:"messages"`
	}
	f.alice.ok("GET", "/threads/"+f.threadA, nil, &msgs)
	if len(msgs.Messages) != 1 {
		t.Errorf("thread has %d messages", len(msgs.Messages))
	}
}

func TestBrandsCantReachEachOthersThings(t *testing.T) {
	f := newFixture(t)
	other := f.brandB
	id := studioID(f.orderA)
	cases := []struct {
		method, path string
		body         any
	}{
		{"GET", "/studio/orders/" + id, nil},
		{"PATCH", "/studio/orders/" + id, m{"status": "SHIPPED"}},
		{"GET", "/studio/products/" + f.productA, nil},
		{"PATCH", "/studio/products/" + f.productA, m{"name": "stolen", "price": 1, "category": "Tops", "sizes": []string{"M"}, "sizeStock": m{"M": 1}}},
		{"PATCH", "/studio/products/" + f.productA + "/stock", m{"size": "M", "stock": 0}},
		{"GET", "/studio/threads/" + f.threadA, nil},
		{"POST", "/studio/threads/" + f.threadA + "/messages", m{"text": "poach"}},
	}
	for _, c := range cases {
		if got, raw := other.raw(c.method, c.path, c.body); got != 404 {
			t.Errorf("%s %s: status %d, want 404: %s", c.method, c.path, got, raw)
		}
	}
	var orders []any
	other.ok("GET", "/studio/orders", nil, &orders)
	if len(orders) != 0 {
		t.Errorf("brand B sees %d orders", len(orders))
	}
	// promo codes
	f.brandA.ok("POST", "/studio/promotions", m{"code": "ONLYA", "type": "% off", "value": "10", "scope": "Everything", "length": "7 days"}, nil)
	other.status(404, "PATCH", "/studio/promotions/ONLYA", m{"live": false})
	// can't tag another brand's product
	var media []struct {
		ID string `json:"id"`
	}
	u := other.upload("image", pngBytes(t))
	var tile struct {
		ID string `json:"id"`
	}
	other.ok("POST", "/studio/media", m{"url": u.URL}, &tile)
	other.status(400, "POST", "/studio/posts", m{"type": "Post", "mediaIds": []string{tile.ID}, "productIds": []string{f.productA}, "when": "Now"})
	// can't use another brand's media library
	f.brandA.ok("GET", "/studio/media", nil, &media)
	ua := f.brandA.upload("image", pngBytes(t))
	var tileA struct {
		ID string `json:"id"`
	}
	f.brandA.ok("POST", "/studio/media", m{"url": ua.URL}, &tileA)
	other.status(400, "POST", "/studio/posts", m{"type": "Post", "mediaIds": []string{tileA.ID}, "productIds": []string{}, "when": "Now"})
	// can't remove another brand's team
	var settings struct {
		Team []struct {
			ID string `json:"id"`
		} `json:"team"`
	}
	f.brandA.ok("GET", "/studio/settings", nil, &settings)
	other.status(404, "DELETE", "/studio/team/"+settings.Team[0].ID, nil)
	// a brand can't start a conversation with someone who never bought from it
	other.status(403, "POST", "/studio/threads", m{"customerId": f.alice.id})
}

func TestOwnerCantBeRemoved(t *testing.T) {
	f := newFixture(t)
	var settings struct {
		Team []struct {
			ID   string `json:"id"`
			Role string `json:"role"`
		} `json:"team"`
	}
	f.brandA.ok("GET", "/studio/settings", nil, &settings)
	f.brandA.status(404, "DELETE", "/studio/team/"+settings.Team[0].ID, nil)
}

func TestTeamInvitesAndRoles(t *testing.T) {
	f := newFixture(t)
	owner := f.brandA

	invite := func(role string) *client {
		member := f.e.shopper()
		owner.ok("POST", "/studio/team/invites", m{"email": member.email, "role": role}, nil)
		token := extractToken(t, f.e.mailTo(member.email, "Join"))

		var info struct {
			BrandName string `json:"brandName"`
			Role      string `json:"role"`
		}
		f.e.anon().ok("GET", "/team/invites/"+token, nil, &info)
		if info.Role != role {
			t.Errorf("invite role %q", info.Role)
		}
		member.ok("POST", "/team/invites/accept", m{"token": token}, nil)
		// single use
		f.e.shopper().status(404, "POST", "/team/invites/accept", m{"token": token})
		var me struct {
			Brand *struct {
				ID   string `json:"id"`
				Role string `json:"role"`
			} `json:"brand"`
		}
		member.ok("GET", "/me", nil, &me)
		if me.Brand == nil || me.Brand.ID != owner.brandID || me.Brand.Role != role {
			t.Fatalf("after accepting: %+v", me.Brand)
		}
		return member
	}

	id := studioID(f.orderA)

	t.Run("Orders staff run orders but not the catalog", func(t *testing.T) {
		staff := invite("Orders")
		staff.ok("GET", "/studio/orders", nil, nil)
		staff.ok("GET", "/studio/orders/"+id, nil, nil)
		staff.ok("GET", "/studio/threads", nil, nil)
		staff.status(403, "GET", "/studio/catalog", nil)
		staff.status(403, "POST", "/studio/products", m{})
		staff.status(403, "POST", "/studio/promotions", m{})
		staff.status(403, "PUT", "/studio/settings/payouts", m{})
		staff.status(403, "POST", "/studio/team/invites", m{"email": "x@y.co", "role": "Admin"})
		assertPayoutHidden(t, staff)
	})
	t.Run("Content staff run the catalog but not orders", func(t *testing.T) {
		staff := invite("Content")
		staff.ok("GET", "/studio/catalog", nil, nil)
		staff.ok("GET", "/studio/promotions", nil, nil)
		staff.status(403, "GET", "/studio/orders", nil)
		staff.status(403, "GET", "/studio/threads", nil)
		staff.status(403, "PUT", "/studio/settings/store", m{})
		assertPayoutHidden(t, staff)
	})
	t.Run("Admins can do everything except remove the owner", func(t *testing.T) {
		staff := invite("Admin")
		staff.ok("GET", "/studio/orders", nil, nil)
		staff.ok("GET", "/studio/catalog", nil, nil)
		var s struct {
			Payouts struct {
				Account string `json:"account"`
			} `json:"payouts"`
			Team []struct {
				ID, Role string
			} `json:"team"`
		}
		staff.ok("GET", "/studio/settings", nil, &s)
		if s.Payouts.Account != "0123456789" {
			t.Errorf("admins can manage payouts, got %q", s.Payouts.Account)
		}
		staff.status(404, "DELETE", "/studio/team/"+s.Team[0].ID, nil)
	})
	t.Run("removing someone ends their access immediately", func(t *testing.T) {
		staff := invite("Orders")
		var s struct {
			Team []struct {
				ID, Name string
			} `json:"team"`
		}
		owner.ok("GET", "/studio/settings", nil, &s)
		var id string
		for _, member := range s.Team {
			if strings.Contains(member.Name, strings.TrimPrefix(staff.handle, "user.")) {
				id = member.ID
			}
		}
		if id == "" {
			t.Fatalf("member not found in %+v", s.Team)
		}
		owner.ok("DELETE", "/studio/team/"+id, nil, nil)
		staff.status(403, "GET", "/studio/orders", nil)
	})
	t.Run("someone already on a team can't accept another invite", func(t *testing.T) {
		owner.ok("POST", "/studio/team/invites", m{"email": "busy@test.dev", "role": "Orders"}, nil)
		token := extractToken(t, f.e.mailTo("busy@test.dev", "Join"))
		f.brandB.status(409, "POST", "/team/invites/accept", m{"token": token})
	})
}

func assertPayoutHidden(t *testing.T, c *client) {
	t.Helper()
	var s struct {
		Payouts struct {
			Account string `json:"account"`
		} `json:"payouts"`
	}
	c.ok("GET", "/studio/settings", nil, &s)
	if s.Payouts.Account != "" {
		t.Errorf("staff can read the bank account: %q", s.Payouts.Account)
	}
}

func TestSensitiveFieldsAreEncryptedAtRest(t *testing.T) {
	f := newFixture(t)
	var raw string
	if err := f.e.pool.QueryRow(t.Context(), `SELECT payouts::text || business::text FROM brands WHERE id = $1`, f.brandA.brandID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	for _, plain := range []string{"0123456789", "RC 123"} {
		if strings.Contains(raw, plain) {
			t.Errorf("%q is stored in plaintext: %s", plain, raw)
		}
	}
	// and it round-trips through the API for the owner
	var s struct {
		Payouts struct {
			Account string `json:"account"`
		} `json:"payouts"`
	}
	f.brandA.ok("GET", "/studio/settings", nil, &s)
	if s.Payouts.Account != "0123456789" {
		t.Errorf("owner got %q", s.Payouts.Account)
	}
}

func TestUploadRules(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.shopper(), e.shopper()
	owner := e.brandOwner()
	pid := owner.addProduct(5000, map[string]int{"M": 3})

	img := alice.upload("image", pngBytes(t))
	if img.URL == "" || img.Aspect == 0 {
		t.Fatalf("image upload: %+v", img)
	}

	t.Run("public images are served with safe headers", func(t *testing.T) {
		res, err := http.Get(e.srv.URL + img.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, _ := io.ReadAll(res.Body)
		if res.StatusCode != 200 || len(data) == 0 {
			t.Fatalf("status %d", res.StatusCode)
		}
		if res.Header.Get("Content-Type") != "image/png" {
			t.Errorf("type %q", res.Header.Get("Content-Type"))
		}
		if res.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Error("missing nosniff")
		}
	})

	t.Run("identity documents never get a public URL", func(t *testing.T) {
		doc := alice.upload("document", pngBytes(t))
		if doc.URL != "" {
			t.Fatalf("document has a URL: %q", doc.URL)
		}
		res, err := http.Get(e.srv.URL + "/uploads/" + doc.ID)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 404 {
			t.Errorf("anonymous fetch: %d", res.StatusCode)
		}
		// nor by its owner over HTTP, signed in
		req, _ := http.NewRequest("GET", e.srv.URL+"/uploads/"+doc.ID, nil)
		ownerRes, err := alice.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		ownerRes.Body.Close()
		if ownerRes.StatusCode != 404 {
			t.Errorf("owner fetch: %d", ownerRes.StatusCode)
		}
		// someone else's document can't be attached to a brand application
		bob.status(400, "POST", "/studio/onboarding", m{
			"store":    m{"name": "Thief", "handle": "thief.brand", "categories": []string{}, "location": "x", "bio": ""},
			"business": m{"type": "Sole trader", "legalName": "x", "registration": "1", "documentId": doc.ID},
			"payouts":  m{"provider": "Paystack", "bank": "x", "account": "0123456789", "currency": "NGN"},
			"shipping": m{"shipsFrom": "x", "zones": m{"lagos": true}},
		})
	})

	t.Run("the file's real content decides what it is", func(t *testing.T) {
		for name, data := range map[string][]byte{
			"svg with script": []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
			"html":            []byte(`<html><script>alert(1)</script></html>`),
			"plain text":      []byte(`just some text`),
			"exe":             append([]byte("MZ"), make([]byte, 100)...),
		} {
			if status, raw := alice.uploadRaw("image", data); status != 400 {
				t.Errorf("%s: status %d: %s", name, status, raw)
			}
		}
		if status, raw := alice.uploadRaw("image", nil); status != 400 {
			t.Errorf("empty file: status %d: %s", status, raw)
		}
		pdf := []byte("%PDF-1.4\n%binary\n1 0 obj\n<<>>\nendobj\ntrailer\n<<>>\n%%EOF\n")
		if status, _ := alice.uploadRaw("image", pdf); status != 400 {
			t.Errorf("a PDF as an image: status %d", status)
		}
		if doc := alice.upload("document", pdf); doc.Kind != "document" {
			t.Errorf("a PDF document: %+v", doc)
		}
		// corrupt image with a valid header
		bad := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 30)...)
		if status, _ := alice.uploadRaw("image", bad); status != 400 {
			t.Errorf("corrupt PNG: status %d", status)
		}
	})

	t.Run("oversized files are refused", func(t *testing.T) {
		big := append(append([]byte{}, pngBytes(t)...), make([]byte, 11<<20)...)
		if status, raw := alice.uploadRaw("image", big); status != 400 {
			t.Errorf("status %d: %s", status, raw)
		}
	})

	wear := func(c *client, url string) (int, []byte) {
		return c.raw("POST", "/wears", m{"imageUrl": url, "productId": pid, "fit": "True to size", "caption": "x", "hashtags": []string{}})
	}
	t.Run("only your own uploads can be posted", func(t *testing.T) {
		if status, raw := wear(bob, img.URL); status != 400 {
			t.Errorf("someone else's upload: %d %s", status, raw)
		}
		for _, url := range []string{"https://evil.example/x.jpg", "javascript:alert(1)", "data:image/png;base64,AAAA", "/uploads/../../secret", "/assets/photos/../../../etc/passwd"} {
			if status, raw := wear(alice, url); status != 400 {
				t.Errorf("%q: %d %s", url, status, raw)
			}
		}
		if status, raw := wear(alice, img.URL); status != 200 {
			t.Errorf("your own upload: %d %s", status, raw)
		}
	})
}

func TestEmptyListsAreArraysNotNull(t *testing.T) {
	e := newEnv(t)
	shopper := e.shopper()
	owner := e.brandOwner()
	nullArray := regexp.MustCompile(`"(lines|posts|drops|items|tags|shop|media|messages|events|timeline|team|bars|funnel|topPosts|promotions|scopes|reviews|wornBy|colors|sizes|images|styles|wears|orderIds|categories)":null`)
	check := func(c *client, path string) {
		t.Helper()
		status, raw := c.raw("GET", path, nil)
		if status != 200 {
			t.Errorf("%s: status %d: %s", path, status, raw)
			return
		}
		if bad := nullArray.Find(raw); bad != nil {
			t.Errorf("%s: %s would crash the app: %s", path, bad, raw)
		}
		var v any
		if json.Unmarshal(raw, &v) != nil {
			t.Errorf("%s: not JSON: %s", path, raw)
		}
		if string(raw) == "null\n" {
			t.Errorf("%s: returned null instead of a list", path)
		}
	}
	for _, p := range []string{"/me", "/feed", "/explore", "/explore/trends", "/products", "/cart", "/me/orders", "/me/wears", "/me/saved", "/me/closet",
		"/threads", "/notifications", "/me/addresses", "/me/payment-methods", "/me/blocks", "/brands"} {
		check(shopper, p)
	}
	for _, p := range []string{"/studio/metrics", "/studio/metrics?period=7D", "/studio/orders", "/studio/catalog", "/studio/promotions", "/studio/media", "/studio/settings",
		"/studio/threads", "/studio/notifications", fmt.Sprintf("/brands/%s", owner.brandHandle)} {
		check(owner, p)
	}
	pid := owner.addProduct(5000, map[string]int{"M": 1})
	check(shopper, "/products/"+pid)
	check(owner, "/studio/products/"+pid)
}
