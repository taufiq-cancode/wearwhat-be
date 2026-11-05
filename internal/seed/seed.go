// Package seed loads demo content into an empty development database: the sample catalog
// from the prototype, restructured so orders and conversations link real accounts.
package seed

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"wearwhat-be/internal/db"
	"wearwhat-be/internal/domain"
	"wearwhat-be/internal/secure"
)

// DemoPassword signs in every demo account. Seeding is refused in production.
const DemoPassword = "wearwhat-demo"

// IfEmpty seeds only when the database has no accounts, so restarts never duplicate data.
func IfEmpty(ctx context.Context, pool *pgxpool.Pool, loc *time.Location, key []byte) error {
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	return Run(ctx, pool, loc, key)
}

// Run inserts the demo data in one transaction.
func Run(ctx context.Context, pool *pgxpool.Pool, loc *time.Location, key []byte) error {
	return db.Tx(ctx, pool, func(tx pgx.Tx) error {
		s := &seeder{ctx: ctx, tx: tx, now: time.Now(), loc: loc, key: key, rng: rand.New(rand.NewPCG(2026, 11))}
		s.hash = secure.HashPassword(DemoPassword)
		s.users()
		s.brands()
		s.drops()
		s.products()
		s.posts()
		s.follows()
		s.commerce()
		s.history()
		s.conversations()
		s.notifications()
		return s.err
	})
}

type seeder struct {
	ctx  context.Context
	tx   pgx.Tx
	err  error
	now  time.Time
	loc  *time.Location
	key  []byte
	hash string
	rng  *rand.Rand
	// filled in as products are inserted, for use by later steps
	prices map[string]int
}

func (s *seeder) exec(sql string, args ...any) {
	if s.err != nil {
		return
	}
	if _, err := s.tx.Exec(s.ctx, sql, args...); err != nil {
		s.err = fmt.Errorf("seed: %w\n%s", err, sql)
	}
}

func (s *seeder) ago(d time.Duration) time.Time { return s.now.Add(-d) }

const (
	minute = time.Minute
	hour   = time.Hour
	day    = 24 * time.Hour
)

func photo(key string) *domain.Photo {
	p := &domain.Photo{URL: "/assets/photos/" + key + ".jpg"}
	if strings.HasPrefix(key, "isb") {
		p.Fit = "contain"
		p.BG = map[string]string{"isb-5": "#F9E6D8", "isb-5f": "#F9E6D8", "isb-10": "#EAD8CC"}[key]
		if p.BG == "" {
			p.BG = "#FFFFFF"
		}
	}
	return p
}

func photoOrNil(key string) *domain.Photo {
	if key == "" {
		return nil
	}
	return photo(key)
}

/* ---------- accounts ---------- */

type demoUser struct {
	id, handle, name, email, bio, avatar, city string
	followers, following                       int
	line1, phone                               string
}

func (s *seeder) users() {
	list := []demoUser{
		{"u_amara", "amara.k", "Amara Kalu", "amara@kalu.co", "Lagos → London. Tailoring, wide legs, nothing loud.", "debit-1", "Lagos 106104", 1200, 318, "14 Admiralty Way, Lekki Phase 1", "+234 803 555 0142"},
		{"u_tobi", "tobi.wears", "Tobi Adeyemi", "tobi@wearwhat.test", "Track jackets, always.", "redeye-1", "Lagos", 842, 190, "22 Adeola Odeku Street, Victoria Island", "+234 803 555 0177"},
		{"u_zee", "zee_style", "Zainab Okoro", "zee@wearwhat.test", "Stylist. Collabs in the DMs.", "redeye-3", "Abuja", 5300, 410, "7 Aminu Kano Crescent, Wuse 2", "+234 803 555 0188"},
		{"u_kofi", "kofi.fits", "Kofi Mensah", "kofi@wearwhat.test", "Accra fits.", "debit-3", "Accra", 960, 280, "12 Oxford Street, Osu", "+233 24 555 0133"},
		{"u_ife", "ife.b", "Ife Bello", "ife@wearwhat.test", "", "hf-2", "Lagos", 210, 95, "3 Bode Thomas Street, Surulere", "+234 803 555 0120"},
		// brand owners and staff
		{"u_debit", "debit.studio", "Dayo Bakare", "debit@wearwhat.test", "", "", "Lagos", 40, 12, "9 Adelabu Street, Surulere", ""},
		{"u_isb", "isb.studio", "Ibrahim Sule", "isb@wearwhat.test", "", "", "Lagos", 25, 8, "4 Herbert Macaulay Way, Yaba", ""},
		{"u_redeye", "redeye.studio", "Thando Dlamini", "redeye@wearwhat.test", "", "", "Cape Town", 31, 6, "18 Long Street", ""},
		{"u_gocrazy", "gocrazy.studio", "Nana Boateng", "gocrazy@wearwhat.test", "", "", "Accra", 19, 5, "5 Ring Road", ""},
		{"u_seyi", "seyi.b", "Seyi Balogun", "seyi@wearwhat.test", "", "", "Lagos", 12, 30, "2 Awolowo Road, Ikoyi", ""},
		{"u_kemi", "kemi.a", "Kemi Ade", "kemi@wearwhat.test", "", "", "Lagos", 18, 44, "11 Allen Avenue, Ikeja", ""},
	}
	for _, u := range list {
		s.exec(`INSERT INTO users (id, handle, name, email, password_hash, verified_at, bio, avatar, followers, following, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$6)`,
			u.id, u.handle, u.name, u.email, s.hash, s.ago(200*day), u.bio, photoOrNil(u.avatar), u.followers, u.following)
		s.exec(`INSERT INTO addresses (id, user_id, name, line1, city, phone, is_default) VALUES ($1,$2,$3,$4,$5,$6,true)`,
			"adr_"+u.id, u.id, u.name, u.line1, u.city, u.phone)
	}
	s.exec(`UPDATE users SET fit = $2, styles = $3, shop_for = $4 WHERE id = $1`, "u_amara",
		map[string]string{"height": "165–175cm", "top": "M", "bottom": "10", "shoe": "39"}, []string{"Tailoring", "Minimal"}, "Womenswear")
}

/* ---------- brands ---------- */

func (s *seeder) encrypt(v string) string {
	if s.err != nil {
		return ""
	}
	out, err := secure.Encrypt(s.key, v)
	if err != nil {
		s.err = err
	}
	return out
}

func (s *seeder) brands() {
	type b struct {
		id, owner, handle, name, initials, avatar, meta, desc, bio, location string
		categories                                                           []string
		followers                                                            int
		shipsFrom, legal, bank, account                                      string
		zones                                                                domain.ShippingZones
		media                                                                []string
	}
	lagos := domain.ShippingZones{Lagos: true, Nigeria: true}
	list := []b{
		{"hf", "u_amara", "highfashion", "HIGHFASHION", "HF", "hf-3", "Womenswear & tailoring · Lagos · Est. 2019", "Womenswear · tailoring", "Sharp shoulders, honest fabric. Every piece try-on ready.", "Lekki, Lagos", []string{"Womenswear", "Tailoring"}, 48200, "Lekki, Lagos", "High Fashion Studio Ltd", "GTBank", "0123456789", lagos, []string{"hf-1", "hf-2", "hf-3", "hf-4", "hf-5", "hf-6", "hf-7", "hf-8"}},
		{"isb", "u_isb", "isb", "ISB", "IS", "isb-3f", "Hoodies & knits · Lagos · Est. 2020", "Hoodies & knits", "Heavy fleece, clean logos, made to be lived in.", "Yaba, Lagos", []string{"Streetwear", "Knitwear"}, 21400, "Yaba, Lagos", "ISB Clothing Ltd", "Access Bank", "0234567891", lagos, []string{"isb-1", "isb-2", "isb-3", "isb-4", "isb-6", "isb-7", "isb-8", "isb-9", "isb-10"}},
		{"debit", "u_debit", "debit", "DEBIT", "DB", "debit-3", "Streetwear · Lagos · Est. 2018", "Streetwear · Lagos", "Track jackets and knits for the long way home.", "Surulere, Lagos", []string{"Streetwear"}, 33900, "Surulere, Lagos", "Debit Apparel Ltd", "Zenith Bank", "0345678912", lagos, []string{"debit-1", "debit-2", "debit-3", "debit-4", "debit-5", "debit-6"}},
		{"redeye", "u_redeye", "redeyexclusive", "REDEYEXCLUSIVE", "RE", "redeye-2", "Graphic tees · drops · Cape Town", "Graphic tees · drops", "Limited runs. Team Redeye forever.", "Cape Town", []string{"Streetwear", "Graphic tees"}, 18700, "Cape Town", "Redeye Exclusive (Pty) Ltd", "FNB", "0456789123", domain.ShippingZones{Africa: true, International: true}, []string{"redeye-1", "redeye-2", "redeye-3", "redeye-4"}},
		{"gocrazy", "u_gocrazy", "gocrazy", "GOCRAZY", "GC", "", "Denim · Accra · Est. 2022", "Denim · Accra", "Washed denim, loud fits.", "Accra", []string{"Denim"}, 9100, "Accra", "GoCrazy Denim Ltd", "GCB Bank", "0567891234", domain.ShippingZones{Africa: true}, nil},
	}
	for _, x := range list {
		s.exec(`INSERT INTO brands (id, owner_id, handle, name, initials, status, verified_at, avatar, followers, meta, description, bio, location, categories, payouts, shipping, business, created_at)
			VALUES ($1,$2,$3,$4,$5,'verified',$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$6)`,
			x.id, x.owner, x.handle, x.name, x.initials, s.ago(120*day), photoOrNil(x.avatar), x.followers, x.meta, x.desc, x.bio, x.location, x.categories,
			map[string]string{"provider": "Paystack", "bank": x.bank, "accountEnc": s.encrypt(x.account), "schedule": "Weekly", "currency": "NGN", "legalName": x.legal},
			domain.ShippingSettings{ShipsFrom: x.shipsFrom, Zones: x.zones, Processing: "48h", Returns: "14 days"},
			map[string]string{"type": "Registered business", "legalName": x.legal, "registrationEnc": s.encrypt("RC 1234567")})
		var email string
		if s.err == nil {
			if err := s.tx.QueryRow(s.ctx, `SELECT email FROM users WHERE id = $1`, x.owner).Scan(&email); err != nil {
				s.err = err
			}
		}
		s.exec(`INSERT INTO team_members (id, brand_id, user_id, email, name, role, status) SELECT $1,$2,id,email,name,'OWNER','active' FROM users WHERE id = $3`, "tm_"+x.id, x.id, x.owner)
		for i, key := range x.media {
			s.exec(`INSERT INTO brand_media (id, brand_id, media, created_at) VALUES ($1,$2,$3,$4)`,
				fmt.Sprintf("md_%s_%d", x.id, i), x.id, photo(key), s.ago(time.Duration(i)*day))
		}
	}
	for _, staff := range []struct{ id, user, role string }{{"tm_seyi", "u_seyi", "Orders"}, {"tm_kemi", "u_kemi", "Content"}} {
		s.exec(`INSERT INTO team_members (id, brand_id, user_id, email, name, role, status) SELECT $1,'hf',id,email,name,$3,'active' FROM users WHERE id = $2`, staff.id, staff.user, staff.role)
	}
}

// nextFriday is the coming Friday at 18:00 local time.
func (s *seeder) nextFriday() time.Time {
	t := s.now.In(s.loc)
	days := (int(time.Friday) - int(t.Weekday()) + 7) % 7
	if days == 0 {
		days = 7
	}
	d := t.AddDate(0, 0, days)
	return time.Date(d.Year(), d.Month(), d.Day(), 18, 0, 0, 0, s.loc)
}

func (s *seeder) drops() {
	released := func(id, brand, title string, d time.Duration) {
		at := s.ago(d)
		s.exec(`INSERT INTO drops (id, brand_id, title, releases_at, released_at) VALUES ($1,$2,$3,$4,$4)`, id, brand, title, at)
	}
	released("dr_hf_02", "hf", "Drop 02", 3*day)
	released("dr_debit_r", "debit", "Racer", 5*day)
	released("dr_red_team", "redeye", "Team Redeye", 8*day)
	released("dr_gc_01", "gocrazy", "Drop 01", day)
	s.exec(`INSERT INTO drops (id, brand_id, title, releases_at) VALUES ('dr_hf_03','hf','Drop 03',$1)`, s.nextFriday())
}

/* ---------- products ---------- */

var (
	topSizes    = []string{"XS", "S", "M", "L", "XL", "XXL"}
	bottomSizes = []string{"6", "8", "10", "12", "14", "16"}
)

func (s *seeder) product(id, brand, name string, dollars int, img, placeholder, cat, desc, color string, soldOut []string, tryOn bool, drop string, extra ...string) {
	sizes := topSizes
	if cat == "Bottoms" {
		sizes = bottomSizes
	}
	s.productSized(id, brand, name, dollars, img, placeholder, cat, desc, color, tryOn, drop, extra, func(i int, label string) (string, int) {
		// the largest size starts sold out, plus any per-product gaps
		for _, z := range soldOut {
			if z == label {
				return label, 0
			}
		}
		if i == len(sizes)-1 {
			return label, 0
		}
		return label, 8
	}, sizes)
}

func (s *seeder) productSized(id, brand, name string, dollars int, img, placeholder, cat, desc, color string, tryOn bool, drop string, extra []string, stock func(int, string) (string, int), sizes []string) {
	images := []domain.Photo{}
	if img != "" {
		images = append(images, *photo(img))
	}
	for _, e := range extra {
		images = append(images, *photo(e))
	}
	var flat *domain.Photo
	if tryOn && img != "" {
		flat = photo(img)
	}
	colors := []domain.ColorOption{{Name: color, Hex: domain.ColorHex[color]}}
	for _, c := range []string{"Black", "Bone", "Charcoal"} {
		if c != color && len(colors) < 3 {
			colors = append(colors, domain.ColorOption{Name: c, Hex: domain.ColorHex[c]})
		}
	}
	var dropID *string
	if drop != "" {
		dropID = &drop
	}
	// older ids read as older products, which keeps the "newest" sort meaningful
	created := s.ago(time.Duration(30+s.rng.IntN(60)) * day)
	s.exec(`INSERT INTO products (id, brand_id, name, price, category, description, color, colors, images, flat_lay, placeholder, drop_id, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$13)`,
		id, brand, name, dollars*100, cat, desc, color, colors, images, flat, placeholder, dropID, created)
	for i, label := range sizes {
		l, n := stock(i, label)
		s.exec(`INSERT INTO product_sizes (product_id, label, stock, position) VALUES ($1,$2,$3,$4)`, id, l, n, i)
	}
	if s.prices == nil {
		s.prices = map[string]int{}
	}
	s.prices[id] = dollars * 100
}

// split shares a total across the sizes: mostly mid sizes, like a real allocation.
func split(sizes []string, total int) func(int, string) (string, int) {
	middle := len(sizes) / 2
	weights := 0
	for i := range sizes {
		if i == middle {
			weights += 5
		} else {
			weights += 3
		}
	}
	counts := make([]int, len(sizes))
	left := total
	for i := range sizes {
		w := 3
		if i == middle {
			w = 5
		}
		counts[i] = total * w / weights
		left -= counts[i]
	}
	// whatever rounding left over goes out one at a time, starting from the middle size
	for i := middle; left > 0; i = (i + 1) % len(counts) {
		counts[i]++
		left--
	}
	return func(i int, label string) (string, int) { return label, counts[i] }
}

func (s *seeder) products() {
	sml := []string{"S", "M", "L"}
	numeric := []string{"8", "10", "12", "14"}

	s.product("p1", "redeye", "Team Redeye mesh jersey", 85, "redeye-3", "", "Tops", "Athletic mesh · Oversized fit", "Red", nil, true, "dr_red_team")
	s.product("p2", "isb", "Firestrike tracksuit", 180, "isb-1f", "", "Outerwear", "Panelled tricot · Flared leg", "Red", []string{"XS"}, true, "")
	s.productSized("p3", "hf", "Acid-wash denim set", 240, "hf-1", "", "Outerwear", "Washed 14oz denim · Boxy shirt + wide leg", "Denim", true, "dr_hf_02", []string{"hf-2", "hf-4"}, split(sml, 12), sml)
	s.product("p4", "debit", "Squiggle knit vest", 94, "debit-2", "", "Knitwear", "Chunky intarsia knit · Relaxed fit", "Multi", nil, true, "")
	s.product("p5", "gocrazy", "Washed denim jacket", 165, "", "Denim jacket — model shot", "Outerwear", "14oz denim · Stone washed", "Denim", nil, false, "dr_gc_01")
	s.product("p6", "debit", "Racer track jacket", 180, "debit-1", "", "Outerwear", "Nylon shell · Contrast piping", "Black", nil, true, "dr_debit_r", "debit-4")
	s.product("p7", "hf", "Paint-splatter set", 260, "hf-4", "", "Outerwear", "Cotton twill · Hand-splattered", "White", []string{"XS"}, true, "dr_hf_02")
	s.productSized("p8", "hf", "Indigo wide-leg jean", 175, "hf-2", "", "Bottoms", "Rigid denim · Wide leg", "Denim", true, "dr_hf_02", nil, split(numeric, 7), numeric)
	s.productSized("p9", "hf", "Velour track set", 320, "hf-8", "", "Outerwear", "Crushed velour · Relaxed fit", "Black", true, "", nil, split(sml, 4), sml)
	s.product("p10", "hf", "Cream twill set", 210, "hf-3", "", "Tops", "Heavy twill · Short sleeve", "White", nil, true, "")
	s.product("p11", "redeye", "Box logo heavy tee", 58, "redeye-1", "", "Tops", "320gsm cotton · Boxy fit", "Black", nil, true, "")
	s.product("p12", "isb", "Boy12 signature hoodie", 117, "isb-3f", "", "Tops", "Brushed fleece · Regular fit", "Black", nil, true, "")
	s.product("p13", "debit", "Wave knit vest", 110, "debit-3", "", "Knitwear", "Chunky knit · Sleeveless", "Multi", nil, true, "")
	s.productSized("p14", "hf", "Studded brown shirt", 150, "hf-6", "", "Tops", "Cotton poplin · Stud detail", "Brown", false, "", nil, split(sml, 0), sml)
	s.product("p15", "isb", "Signature hoodie — heather", 117, "isb-4f", "", "Tops", "Brushed fleece · Regular fit", "Grey", nil, true, "")
	s.product("p16", "hf", "Black & amber track set", 290, "hf-7", "", "Outerwear", "Satin tricot · Contrast stripe", "Black", []string{"XL"}, true, "")
	s.product("p17", "redeye", "Team Redeye jersey — navy", 85, "redeye-4", "", "Tops", "Athletic mesh · Oversized fit", "Black", nil, true, "dr_red_team")
	s.productSized("p18", "hf", "Splatter overshirt — black", 180, "hf-5", "", "Tops", "Cotton twill · Oversized", "Black", false, "", nil, split(sml, 0), sml)
	// held back until Drop 03 releases
	s.product("p19", "hf", "Cropped wool blazer", 280, "", "Cropped wool blazer — coming in Drop 03", "Outerwear", "Virgin wool · Sharp shoulder", "Black", nil, false, "dr_hf_03")
	s.product("p20", "hf", "Pleated column skirt", 190, "", "Pleated column skirt — coming in Drop 03", "Bottoms", "Fluid crepe · Knife pleats", "Grey", nil, false, "dr_hf_03")
	s.exec(`UPDATE products SET created_at = now() - interval '2 days' WHERE id IN ('p3','p8','p7')`)
}

/* ---------- posts ---------- */

type tag struct {
	label   string
	x, y    float64
	product string
}

const frameWidth = 390.0

func at(label string, x, y, height float64, product string) tag {
	return tag{label, round1(x / frameWidth * 100), round1(y / height * 100), product}
}

func round1(v float64) float64 { return float64(int(v*10+0.5)) / 10 }

type postSpec struct {
	id, brand, user, sub, img, caption string
	height                             float64
	likes                              int
	age                                time.Duration
	tags                               []tag
	shop                               []string
	worn, wornSize, wornFit            string
	hashtags                           []string
}

func (s *seeder) post(p postSpec) {
	var brand, user, worn *string
	if p.brand != "" {
		brand = &p.brand
	}
	if p.user != "" {
		user = &p.user
	}
	if p.worn != "" {
		worn = &p.worn
	}
	postType := "Post"
	if p.user != "" {
		postType = "Wear"
	}
	published := s.ago(p.age)
	s.exec(`INSERT INTO posts (id, brand_id, user_id, type, media, aspect, subtitle, caption, likes, worn_product_id, worn_size, worn_fit, created_at, published_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$13)`,
		p.id, brand, user, postType, []domain.Photo{*photo(p.img)}, frameWidth/p.height, p.sub, p.caption, p.likes, worn, p.wornSize, p.wornFit, published)
	for i, t := range p.tags {
		s.exec(`INSERT INTO post_tags (post_id, position, product_id, label, x, y) VALUES ($1,$2,$3,$4,$5,$6)`, p.id, i, t.product, t.label, t.x, t.y)
	}
	shop := p.shop
	if shop == nil {
		seen := map[string]bool{}
		for _, t := range p.tags {
			if !seen[t.product] {
				shop = append(shop, t.product)
				seen[t.product] = true
			}
		}
	}
	for i, id := range shop {
		s.exec(`INSERT INTO post_shop (post_id, position, product_id) VALUES ($1,$2,$3)`, p.id, i, id)
	}
	for _, h := range p.hashtags {
		s.exec(`INSERT INTO post_hashtags (post_id, tag) VALUES ($1,$2)`, p.id, h)
	}
}

func (s *seeder) posts() {
	s.post(postSpec{id: "f1", brand: "hf", sub: "Drop 02 · London", img: "hf-1", height: 480, likes: 4211, age: 3 * hour,
		caption: "Drop 02 is live. Washed denim, wide legs, nothing extra. Try it on before you cop.",
		tags:    []tag{at("Acid-wash denim set — $240", 150, 150, 480, "p3"), at("Wide-leg jean — $175", 20, 350, 480, "p8")},
		shop:    []string{"p3", "p8", "p16"}})
	s.post(postSpec{id: "f2", user: "u_amara", sub: "Wearing DEBIT · 2 days after delivery", img: "debit-1", height: 480, likes: 812, age: 8 * hour,
		caption: "Finally. M sits right through the shoulders. #OOTD #DebitWears", worn: "p6", wornSize: "M", wornFit: "True to size",
		tags: []tag{at("DEBIT track jacket", 170, 210, 480, "p6")}, hashtags: []string{"#OOTD", "#DebitWears"}})
	s.post(postSpec{id: "f3", brand: "redeye", sub: "Team Redeye · Cape Town", img: "redeye-3", height: 470, likes: 2380, age: 5 * hour,
		caption: "Team Redeye is back. Mesh jerseys in red and navy, limited run.",
		tags:    []tag{at("Team Redeye jersey — $85", 16, 330, 470, "p1")}, shop: []string{"p1", "p17", "p11"}})

	// the rest of each brand's grid
	type back struct {
		img, product, caption string
		age                   time.Duration
	}
	archive := map[string][]back{
		"hf": {{"hf-2", "p8", "Rigid denim, cut wide.", 4 * day}, {"hf-3", "p10", "Cream twill, heavy hand.", 9 * day}, {"hf-4", "p7", "Every splatter is by hand.", 13 * day},
			{"hf-5", "p18", "The overshirt, in black.", 18 * day}, {"hf-6", "p14", "Studs, placed one at a time.", 24 * day}, {"hf-7", "p16", "Satin tricot with a contrast stripe.", 31 * day}, {"hf-8", "p9", "Crushed velour for the long weekend.", 40 * day}},
		"debit":  {{"debit-2", "p4", "Squiggle vest, back in stock.", 6 * day}, {"debit-3", "p13", "Wave knit for cool evenings.", 12 * day}, {"debit-5", "p6", "Track jackets for the long way home.", 21 * day}},
		"isb":    {{"isb-1", "p2", "Firestrike. Panelled tricot, flared leg.", 7 * day}, {"isb-3", "p12", "The Boy12 hoodie.", 14 * day}, {"isb-4", "p15", "Heather, the same heavy fleece.", 22 * day}},
		"redeye": {{"redeye-1", "p11", "320gsm. Boxy on purpose.", 10 * day}, {"redeye-4", "p17", "Navy joins the team.", 16 * day}},
	}
	locations := map[string]string{"hf": "Lekki, Lagos", "debit": "Surulere, Lagos", "isb": "Yaba, Lagos", "redeye": "Cape Town"}
	for brand, list := range archive {
		for i, b := range list {
			s.post(postSpec{id: fmt.Sprintf("bp_%s_%d", brand, i), brand: brand, sub: locations[brand], img: b.img, height: 480, likes: 300 + s.rng.IntN(1800), age: b.age,
				caption: b.caption, tags: []tag{at(b.caption, 24, 300, 480, b.product)}, shop: []string{b.product}})
		}
	}

	// wears: what makes fit reports, "worn by" and trends real
	wears := []struct {
		id, user, img, product, size, fit, caption string
		age                                        time.Duration
		tags                                       []string
	}{
		{"w_amara_1", "u_amara", "debit-2", "p4", "M", "True to size", "Knit vest weather.", 3 * day, []string{"#OOTD", "#StreetStyle"}},
		{"w_amara_2", "u_amara", "redeye-2", "p1", "M", "Runs large", "Size down if you don’t want it boxy.", 6 * day, []string{"#StreetStyle"}},
		{"w_amara_3", "u_amara", "hf-3", "p10", "M", "True to size", "Cream on cream.", 11 * day, []string{"#Tailored", "#OOTD"}},
		{"w_amara_4", "u_amara", "debit-4", "p6", "M", "True to size", "Still my favourite jacket.", 16 * day, []string{"#DebitWears"}},
		{"w_amara_5", "u_amara", "redeye-1", "p11", "L", "True to size", "Heavy tee, heavy rotation.", 25 * day, []string{"#OOTD"}},
		{"w_kofi_1", "u_kofi", "debit-3", "p13", "M", "True to size", "That drape.", 2 * day, []string{"#OOTD", "#StreetStyle"}},
		{"w_kofi_2", "u_kofi", "debit-6", "p6", "L", "Runs large", "Roomy, how I like it.", 9 * day, []string{"#DebitWears", "#StreetStyle"}},
		{"w_zee_1", "u_zee", "debit-5", "p4", "S", "Runs small", "Snug in the best way.", 4 * day, []string{"#OOTD"}},
		{"w_zee_2", "u_zee", "hf-2", "p8", "10", "True to size", "Wide-leg forever.", 7 * day, []string{"#Tailored", "#OOTD"}},
		{"w_zee_3", "u_zee", "hf-1", "p3", "M", "True to size", "Fits exactly like the try-on showed.", 1 * day, []string{"#OOTD", "#Tailored"}},
		{"w_tobi_1", "u_tobi", "redeye-4", "p17", "L", "Runs large", "Went up one for a looser fit.", 5 * day, []string{"#StreetStyle"}},
		{"w_tobi_2", "u_tobi", "hf-7", "p16", "L", "True to size", "The stripe does the work.", 12 * day, []string{"#Tailored"}},
		{"w_ife_1", "u_ife", "hf-4", "p7", "M", "True to size", "Nobody else has this one.", 10 * day, []string{"#OOTD"}},
	}
	for _, w := range wears {
		s.post(postSpec{id: w.id, user: w.user, sub: "Wearing", img: w.img, height: 480, likes: 40 + s.rng.IntN(600), age: w.age, caption: w.caption,
			worn: w.product, wornSize: w.size, wornFit: w.fit, tags: []tag{{"Tagged piece", 18, 62, w.product}}, hashtags: w.tags})
	}
	s.exec(`UPDATE posts SET subtitle = 'Wearing ' || b.name FROM products p JOIN brands b ON b.id = p.brand_id WHERE posts.worn_product_id = p.id AND posts.subtitle = 'Wearing'`)
	s.exec(`INSERT INTO post_likes (user_id, post_id) VALUES ('u_amara','f1')`)
	s.exec(`INSERT INTO post_amplifications (post_id, brand_id) VALUES ('w_zee_3','hf')`)

	// comments on the lead posts
	for i, c := range []struct{ id, post, user, text string }{
		{"c_1", "f1", "u_zee", "That wash is unreal."},
		{"c_2", "f1", "u_kofi", "Does the shirt run big?"},
		{"c_3", "f2", "u_kofi", "That drape. What size?"},
		{"c_4", "f2", "u_tobi", "Copped because of this."},
		{"c_5", "f3", "u_amara", "The red is so good."},
	} {
		s.exec(`INSERT INTO comments (id, post_id, user_id, text, created_at) VALUES ($1,$2,$3,$4,$5)`, c.id, c.post, c.user, c.text, s.ago(time.Duration(30+i*20)*minute))
		s.exec(`UPDATE posts SET comments_count = comments_count + 1 WHERE id = $1`, c.post)
	}
}

func (s *seeder) follows() {
	// counters were set directly above, so these rows don't adjust them
	for _, f := range [][2]string{{"u_amara", "debit"}, {"u_amara", "redeye"}, {"u_tobi", "hf"}, {"u_tobi", "debit"}, {"u_zee", "hf"}, {"u_kofi", "debit"}, {"u_kofi", "hf"}, {"u_ife", "hf"}} {
		s.exec(`INSERT INTO follows_brand (user_id, brand_id) VALUES ($1,$2)`, f[0], f[1])
	}
	for _, f := range [][2]string{{"u_zee", "u_amara"}, {"u_tobi", "u_amara"}, {"u_amara", "u_tobi"}, {"u_kofi", "u_amara"}} {
		s.exec(`INSERT INTO follows_user (follower_id, followee_id) VALUES ($1,$2)`, f[0], f[1])
	}
}

/* ---------- commerce ---------- */

type orderSpec struct {
	seq                    int64
	user, brand, product   string
	size, color            string
	status                 string
	placed                 time.Duration
	shipped, delivered     *time.Duration
	carrier, tracking      string
	note                   string
	returned               *time.Duration
	tryOn                  bool
	post                   string
}

func dur(d time.Duration) *time.Duration { return &d }

func (s *seeder) order(o orderSpec) {
	price := s.prices[o.product]
	money := domain.LineMoney{Subtotal: price, Shipping: domain.ShippingCost(price, "standard", false)}
	money.Settle()
	placed := s.ago(o.placed)
	var shipped, delivered, returned *time.Time
	if o.shipped != nil {
		t := s.ago(*o.shipped)
		shipped = &t
	}
	if o.delivered != nil {
		t := s.ago(*o.delivered)
		delivered = &t
	}
	if o.returned != nil {
		t := s.ago(*o.returned)
		returned = &t
	}
	var post *string
	if o.post != "" {
		post = &o.post
	}
	s.exec(`INSERT INTO orders (seq, user_id, brand_id, product_id, size, color, qty, unit_price, subtotal, shipping, fee, payout, status, carrier, tracking_code, return_note,
			from_try_on, source_post_id, ship_to, placed_at, shipped_at, delivered_at, return_requested_at)
		SELECT $1,$2,$3,$4,$5,$6,1,$7,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,
			jsonb_build_object('name', a.name, 'line1', a.line1, 'city', a.city, 'phone', a.phone), $17,$18,$19,$20
		FROM addresses a WHERE a.user_id = $2 AND a.is_default`,
		o.seq, o.user, o.brand, o.product, o.size, o.color, price, money.Shipping, money.Fee, money.Payout, o.status, o.carrier, o.tracking, o.note, o.tryOn, post, placed, shipped, delivered, returned)
	s.exec(`INSERT INTO order_events (order_seq, stage, at) VALUES ($1,'placed',$2), ($1,'paid',$2)`, o.seq, placed)
	if shipped != nil {
		s.exec(`INSERT INTO order_events (order_seq, stage, at) VALUES ($1,'shipped',$2)`, o.seq, *shipped)
	}
	if delivered != nil {
		s.exec(`INSERT INTO order_events (order_seq, stage, at) VALUES ($1,'delivered',$2)`, o.seq, *delivered)
	}
	if returned != nil {
		s.exec(`INSERT INTO order_events (order_seq, stage, detail, at) VALUES ($1,'return_requested',$2,$3)`, o.seq, o.note, *returned)
	}
}

func (s *seeder) commerce() {
	const gig = "GIG Logistics"
	// Amara as a shopper
	s.order(orderSpec{seq: 20417, user: "u_amara", brand: "isb", product: "p12", size: "M", color: "Black", status: domain.StatusShipped, placed: 2 * day, shipped: dur(5 * hour), carrier: gig, tracking: "GIG-88412093"})
	s.order(orderSpec{seq: 20388, user: "u_amara", brand: "debit", product: "p6", size: "M", color: "Black", status: domain.StatusDelivered, placed: 9 * day, shipped: dur(8 * day), delivered: dur(6 * day), carrier: gig, tracking: "GIG-88390021"})
	s.order(orderSpec{seq: 20351, user: "u_amara", brand: "redeye", product: "p11", size: "L", color: "Black", status: domain.StatusDelivered, placed: 26 * day, shipped: dur(25 * day), delivered: dur(21 * day), carrier: "DHL", tracking: "DHL-55120873"})
	// HIGHFASHION's order queue
	s.order(orderSpec{seq: 20421, user: "u_ife", brand: "hf", product: "p3", size: "M", color: "Denim", status: domain.StatusToFulfil, placed: 12 * minute, tryOn: true, post: "f1"})
	s.order(orderSpec{seq: 20419, user: "u_tobi", brand: "hf", product: "p8", size: "10", color: "Denim", status: domain.StatusToFulfil, placed: 48 * minute})
	s.order(orderSpec{seq: 20412, user: "u_zee", brand: "hf", product: "p7", size: "S", color: "White", status: domain.StatusShipped, placed: 2 * day, shipped: dur(3 * hour), carrier: gig, tracking: "GIG-88412511", tryOn: true})
	s.order(orderSpec{seq: 20390, user: "u_kofi", brand: "hf", product: "p8", size: "12", color: "Denim", status: domain.StatusReturnReq, placed: 8 * day, shipped: dur(7 * day), delivered: dur(5 * day), returned: dur(day), carrier: gig, tracking: "GIG-88390410", note: "Too long in the leg — I’m 165cm. Item unworn, tags on."})
	s.order(orderSpec{seq: 20377, user: "u_ife", brand: "hf", product: "p9", size: "M", color: "Black", status: domain.StatusDelivered, placed: 6 * day, shipped: dur(5 * day), delivered: dur(2 * day), carrier: gig, tracking: "GIG-88377002"})

	for _, c := range []string{"p6", "p11", "p5", "p12", "p4", "p1", "p10"} {
		s.exec(`INSERT INTO closet_items (user_id, product_id) VALUES ('u_amara',$1)`, c)
	}
	for _, c := range [][2]string{{"u_zee", "p3"}, {"u_zee", "p7"}, {"u_tobi", "p8"}, {"u_kofi", "p8"}, {"u_ife", "p9"}} {
		s.exec(`INSERT INTO closet_items (user_id, product_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, c[0], c[1])
	}

	s.exec(`INSERT INTO cart_lines (id, user_id, product_id, size, color, qty) VALUES ('l_seed_1','u_amara','p6','M','Black',1), ('l_seed_2','u_amara','p11','L','Black',1)`)

	s.exec(`INSERT INTO promotions (code, brand_id, type, value, scope, length, ends_at, uses, live, banner, created_at) VALUES
		('DROP02','hf','% off','15','Drop 02','7 days',$1,184,true,true,$3),
		('FREESHIPLAG','hf','Free ship','','Everything','30 days',$2,61,true,false,$4),
		('WELCOME10','hf','% off','10','Everything','30 days',$2,402,false,false,$5),
		('DEBIT20','debit','$ off','20','Everything','30 days',$2,9,true,false,$3)`,
		s.now.Add(4*day), s.now.Add(20*day), s.ago(3*day), s.ago(10*day), s.ago(60*day))

	s.exec(`INSERT INTO product_saves (user_id, product_id) VALUES ('u_amara','p3'), ('u_tobi','p6'), ('u_zee','p16')`)
	s.exec(`INSERT INTO post_saves (user_id, post_id) VALUES ('u_amara','f3')`)

	// someone's waiting on a sold-out size, so restocking has a visible effect in the demo
	s.exec(`INSERT INTO stock_alerts (user_id, product_id, size) VALUES ('u_tobi','p14','M')`)
}

// history generates 90 days of sales and traffic so dashboards have real numbers to compute.
func (s *seeder) history() {
	if s.err != nil {
		return
	}
	customers := []string{"u_tobi", "u_zee", "u_kofi", "u_ife"}
	reviewTexts := []string{
		"Fits exactly like the try-on showed. Shoulders sit right, no tailoring needed.",
		"Went up one for a looser fit. Fabric is heavier than expected — in a good way.",
		"Beautifully made. Worth waiting for the drop.",
		"True to size and the colour is even better in person.",
		"Took a week to arrive but the quality is there.",
	}
	lines := map[string][]struct {
		product, size, color string
		post                 string
	}{
		"hf":    {{"p3", "M", "Denim", "f1"}, {"p8", "10", "Denim", "f1"}, {"p7", "M", "White", "bp_hf_2"}, {"p16", "M", "Black", "w_tobi_2"}, {"p10", "M", "White", ""}, {"p9", "M", "Black", ""}},
		"debit": {{"p6", "M", "Black", "f2"}, {"p4", "M", "Multi", ""}, {"p13", "L", "Multi", "w_kofi_1"}},
	}
	seq := int64(10000)
	type dayStat struct{ orders, tried int }
	stats := map[string][]dayStat{"hf": make([]dayStat, 90), "debit": make([]dayStat, 90)}
	for brand, list := range lines {
		rate := 2.6
		if brand == "debit" {
			rate = 1.0
		}
		for d := 89; d >= 1; d-- {
			// sales grow a little toward the present, with weekly variation
			expect := rate * (0.55 + 0.45*float64(89-d)/89) * (0.8 + 0.4*float64((d*37)%7)/7)
			n := int(expect)
			if s.rng.Float64() < expect-float64(n) {
				n++
			}
			for i := 0; i < n; i++ {
				seq++
				l := list[s.rng.IntN(len(list))]
				tried := s.rng.Float64() < 0.38
				post := ""
				if s.rng.Float64() < 0.45 {
					post = l.post
				}
				placed := time.Duration(d)*day + time.Duration(s.rng.IntN(20))*hour
				status := domain.StatusDelivered
				shipped := placed - 24*hour
				delivered := placed - 72*hour
				if delivered < 0 {
					delivered = hour
				}
				if shipped < 0 {
					shipped = 30 * minute
				}
				user := customers[s.rng.IntN(len(customers))]
				s.order(orderSpec{seq: seq, user: user, brand: brand, product: l.product, size: l.size, color: l.color, status: status, placed: placed,
					shipped: dur(shipped), delivered: dur(delivered), carrier: "GIG Logistics", tracking: fmt.Sprintf("GIG-%d", 70000000+seq), tryOn: tried, post: post})
				stats[brand][d].orders++
				if tried {
					stats[brand][d].tried++
				}
				if s.rng.Float64() < 0.1 {
					s.exec(`INSERT INTO reviews (id, product_id, user_id, order_seq, rating, text, size, created_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
						fmt.Sprintf("rv_%d", seq), l.product, user, seq, 4+s.rng.IntN(2), reviewTexts[s.rng.IntN(len(reviewTexts))], l.size, s.ago(delivered-time.Hour))
				}
			}
		}
	}
	if s.err != nil {
		return
	}

	// traffic that makes those sales a believable conversion rate, with a real try-on lift
	var events [][]any
	products := map[string][]string{"hf": {"p3", "p7", "p8", "p9", "p10", "p16"}, "debit": {"p4", "p6", "p13"}}
	for brand, days := range stats {
		for d, st := range days {
			at := func() time.Time { return s.ago(time.Duration(d)*day + time.Duration(s.rng.IntN(1440))*minute) }
			// tuned so the dashboard reads like a real shop: around 3% conversion, with
			// try-on converting modestly better than browsing alone
			views := 18 + st.orders*28 + s.rng.IntN(12)
			tries := 3 + st.tried*26 + s.rng.IntN(4)
			added := st.tried*5 + s.rng.IntN(3)
			pick := func() string { return products[brand][s.rng.IntN(len(products[brand]))] }
			for i := 0; i < views; i++ {
				events = append(events, []any{"product_view", brand, pick(), nil, customers[s.rng.IntN(len(customers))], false, at()})
			}
			for i := 0; i < tries; i++ {
				events = append(events, []any{"try_on", brand, pick(), nil, customers[s.rng.IntN(len(customers))], false, at()})
			}
			for i := 0; i < added; i++ {
				events = append(events, []any{"add_to_bag", brand, pick(), nil, customers[s.rng.IntN(len(customers))], true, at()})
			}
		}
	}
	for _, p := range []struct {
		post, brand string
		views       int
	}{{"f1", "hf", 620}, {"w_tobi_2", "hf", 210}, {"bp_hf_2", "hf", 180}, {"f2", "debit", 240}, {"w_kofi_1", "debit", 130}} {
		for i := 0; i < p.views; i++ {
			events = append(events, []any{"post_view", p.brand, nil, p.post, customers[s.rng.IntN(len(customers))], false, s.ago(time.Duration(s.rng.IntN(25*1440)) * minute)})
		}
	}
	if _, err := s.tx.CopyFrom(s.ctx, pgx.Identifier{"analytics_events"}, []string{"type", "brand_id", "product_id", "post_id", "user_id", "from_try_on", "at"}, pgx.CopyFromRows(events)); err != nil {
		s.err = err
	}
}

/* ---------- conversations ---------- */

type seedMsg struct {
	side string
	text string
	age  time.Duration
}

func (s *seeder) thread(id, kind, a, b, brand, product string, aUnread, bUnread int, msgs []seedMsg) {
	var bUser, brandID, prod *string
	if b != "" {
		bUser = &b
	}
	if brand != "" {
		brandID = &brand
	}
	if product != "" {
		prod = &product
	}
	last := s.ago(msgs[len(msgs)-1].age)
	s.exec(`INSERT INTO threads (id, kind, a_user_id, b_user_id, brand_id, product_id, a_unread, b_unread, last_message_at, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, id, kind, a, bUser, brandID, prod, aUnread, bUnread, last, s.ago(msgs[0].age))
	for i, m := range msgs {
		s.exec(`INSERT INTO messages (id, thread_id, side, text, created_at) VALUES ($1,$2,$3,$4,$5)`, fmt.Sprintf("m_%s_%d", id, i), id, m.side, m.text, s.ago(m.age))
	}
}

func (s *seeder) conversations() {
	// Amara, shopping
	s.thread("t_amara_isb", "brand", "u_amara", "", "isb", "p12", 1, 0, []seedMsg{
		{"b", "Hi Amara — your order is confirmed.", 2 * day},
		{"b", "Your Boy12 hoodie shipped today via GIG. Tracking GIG-88412093.", 5 * hour},
	})
	s.thread("t_amara_debit", "brand", "u_amara", "", "debit", "p6", 0, 0, []seedMsg{
		{"a", "The racer jacket fits perfectly. Posting a wear this weekend.", 6 * day},
		{"b", "Tag us — we’ll repost it.", 6*day - 20*minute},
	})
	s.thread("t_zee_amara", "person", "u_zee", "u_amara", "", "", 0, 2, []seedMsg{
		{"a", "Your DEBIT wear went off.", 2 * hour},
		{"a", "Collab shoot next month? Lagos, the 14th.", 2*hour - time.Minute},
	})
	s.thread("t_tobi_amara", "person", "u_tobi", "u_amara", "", "", 0, 0, []seedMsg{
		{"a", "Where’d you get that track jacket?", 3 * day},
		{"b", "DEBIT — tagged it in the post.", 3*day - 10*minute},
		{"a", "Copped. Thanks!", 3*day - 15*minute},
	})
	// HIGHFASHION's customers, seen by Amara and her team in the studio
	s.thread("t_ife_hf", "brand", "u_ife", "", "hf", "p3", 0, 2, []seedMsg{
		{"a", "Hi! Does the denim set run true to size?", 40 * minute},
		{"a", "I’m 168cm, usually an M.", 39 * minute},
	})
	s.thread("t_kofi_hf", "brand", "u_kofi", "", "hf", "p8", 0, 1, []seedMsg{
		{"a", "Started a return for #20390 — the jean is too long.", day},
	})
	s.thread("t_tobi_hf", "brand", "u_tobi", "", "hf", "p8", 0, 0, []seedMsg{
		{"a", "When does #20419 ship?", 3 * day},
		{"b", "Packing it today — you’ll get tracking tonight.", 3*day - 30*minute},
		{"a", "Legend, thanks.", 3*day - 35*minute},
	})
	s.thread("t_zee_hf", "brand", "u_zee", "", "hf", "", 0, 0, []seedMsg{
		{"a", "Will the shirt dress restock in S?", 10 * day},
		{"b", "Yes — Drop 03, Friday 18:00.", 10*day - time.Hour},
	})
}

/* ---------- notifications ---------- */

func (s *seeder) notifications() {
	track := &domain.NotificationAction{Type: "track", Label: "TRACK", Target: "WW-20417"}
	follow := &domain.NotificationAction{Type: "follow", Label: "FOLLOW", DoneLabel: "FOLLOWING", Target: "zee_style"}
	try := &domain.NotificationAction{Type: "try", Label: "TRY ON", Target: "p3"}
	amplify := &domain.NotificationAction{Type: "amplify", Label: "AMPLIFY", DoneLabel: "AMPLIFIED", Target: "w_tobi_2"}
	shopper := []struct {
		id, who, text, kind, avatar, thumb, href string
		action                                   *domain.NotificationAction
		age                                      time.Duration
	}{
		{"n1", "HIGHFASHION", "dropped Drop 02 — 3 pieces, all try-on ready.", "Drops", "hf-3", "hf-1", "/b/highfashion", nil, 2 * hour},
		{"n2", "tobi.wears", "copped your DEBIT track jacket look.", "Social", "redeye-1", "debit-1", "/u/tobi.wears", nil, 3 * hour},
		{"n3", "ISB", "Order #WW-20417 has shipped.", "Orders", "isb-3f", "", "", track, 5 * hour},
		{"n4", "zee_style", "started following you.", "Social", "redeye-3", "", "/u/zee_style", follow, 6 * hour},
		{"n5", "wearwhat", "The denim set you saved is back in M.", "Drops", "", "", "/p/p3", try, 2 * day},
		{"n6", "DEBIT", "featured your wear on their page.", "Social", "debit-3", "debit-1", "/post/f2", nil, 3 * day},
		{"n7", "kofi.fits", "commented: “That drape. What size?”", "Social", "debit-3", "debit-1", "/post/f2", nil, 4 * day},
	}
	for _, n := range shopper {
		s.exec(`INSERT INTO notifications (id, scope, user_id, who, text, kind, avatar, thumb, href, action, created_at) VALUES ($1,'shopper','u_amara',$2,$3,$4,$5,$6,$7,$8,$9)`,
			n.id, n.who, n.text, n.kind, photoOrNil(n.avatar), photoOrNil(n.thumb), n.href, n.action, s.ago(n.age))
	}
	studio := []struct {
		id, who, text, kind, avatar, thumb, href string
		action                                   *domain.NotificationAction
		age                                      time.Duration
	}{
		{"b1", "Order #20421", "Ife Bello bought Acid-wash denim set · M.", "Orders", "", "hf-1", "/studio/orders/20421", nil, 12 * minute},
		{"b2", "tobi.wears", "posted a wear of your Black & amber track set.", "Social", "redeye-1", "hf-7", "/post/w_tobi_2", amplify, 40 * minute},
		{"b3", "Return", "#20390 · Indigo wide-leg jean — “Too long in the leg”.", "Orders", "", "", "/studio/orders/20390", nil, day},
		{"b4", "Drop 02", "is live. Your followers have been notified.", "Orders", "", "hf-4", "/studio", nil, 3 * day},
		{"b5", "zee_style", "tagged HIGHFASHION in a wear.", "Social", "redeye-3", "hf-1", "/post/w_zee_3", nil, 3 * day},
		{"b6", "wearwhat", "Splatter overshirt needs a flat lay for try-on.", "Orders", "", "hf-5", "/studio/catalog", nil, 4 * day},
	}
	for _, n := range studio {
		s.exec(`INSERT INTO notifications (id, scope, brand_id, who, text, kind, avatar, thumb, href, action, created_at) VALUES ($1,'studio','hf',$2,$3,$4,$5,$6,$7,$8,$9)`,
			n.id, n.who, n.text, n.kind, photoOrNil(n.avatar), photoOrNil(n.thumb), n.href, n.action, s.ago(n.age))
	}
}
