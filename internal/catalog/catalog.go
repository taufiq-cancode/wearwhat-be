// Package catalog serves brands, products, explore and trends.
package catalog

import (
	"context"
	"fmt"
	"hash/fnv"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"wearwhat-be/internal/account"
	"wearwhat-be/internal/app"
	"wearwhat-be/internal/db"
	"wearwhat-be/internal/domain"
	"wearwhat-be/internal/httpx"
	"wearwhat-be/internal/views"
)

type Service struct{ *app.Deps }

func (s *Service) Routes(r *httpx.Router) {
	r.User("GET /explore", s.explore)
	r.User("GET /explore/trends", s.trends)
	r.User("GET /products", s.products)
	r.User("GET /products/{id}", s.product)
	r.User("PUT /products/{id}/save", s.saveProduct(true))
	r.User("DELETE /products/{id}/save", s.saveProduct(false))
	r.User("POST /products/{id}/stock-alerts", s.stockAlert)

	r.Public("GET /brands", s.brands)
	r.User("GET /brands/{handle}", s.brand)
	r.User("PUT /brands/{handle}/follow", s.followBrand(true))
	r.User("DELETE /brands/{handle}/follow", s.followBrand(false))
}

/* ---------- brands ---------- */

func (s *Service) brands(_ http.ResponseWriter, r *http.Request) (any, error) {
	rows, err := s.DB.Query(r.Context(), `SELECT id, handle, name, initials, avatar, CASE WHEN description <> '' THEN description ELSE meta END
		FROM brands WHERE status = 'verified' ORDER BY followers DESC, name LIMIT 20`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.BrandCard{}
	for rows.Next() {
		var b domain.BrandCard
		if err := rows.Scan(&b.ID, &b.Handle, &b.Name, &b.Initials, &b.Avatar, &b.Description); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// recentDropDays is how long a released drop's pieces keep their badge in the shop.
const recentDropDays = 14

func (s *Service) brand(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	var b domain.Brand
	var status string
	var accepts bool
	err := s.DB.QueryRow(ctx, `SELECT b.id, b.handle, b.name, b.initials, b.status, b.avatar, b.followers, b.meta, b.bio, b.accept_messages,
			EXISTS (SELECT 1 FROM follows_brand f WHERE f.user_id = $2 AND f.brand_id = b.id),
			(SELECT count(*) FROM posts p WHERE p.brand_id = b.id AND p.status = 'published' AND (p.expires_at IS NULL OR p.expires_at > now())),
			(SELECT count(*) FROM posts p JOIN products x ON x.id = p.worn_product_id WHERE x.brand_id = b.id AND p.status = 'published')
		FROM brands b WHERE b.handle = $1`, domain.NormalizeHandle(r.PathValue("handle")), who.UserID).
		Scan(&b.ID, &b.Handle, &b.Name, &b.Initials, &status, &b.Avatar, &b.Followers, &b.Meta, &b.Bio, &accepts, &b.Following, &b.PostsCount, &b.WearsCount)
	if db.IsNotFound(err) {
		return nil, httpx.NotFound("Brand not found")
	}
	if err != nil {
		return nil, err
	}
	// the banner comes from a live promotion, so it disappears as soon as the code ends
	var code, ptype, pvalue string
	err = s.DB.QueryRow(ctx, `SELECT code, type, value FROM promotions WHERE brand_id = $1 AND banner AND live
		AND (ends_at IS NULL OR ends_at > now()) ORDER BY created_at DESC LIMIT 1`, b.ID).Scan(&code, &ptype, &pvalue)
	switch {
	case err == nil:
		b.Banner = domain.PromoLabel(ptype, pvalue) + " · use " + code + " at checkout"
	case !db.IsNotFound(err):
		return nil, err
	}
	own := who.BrandID == b.ID
	// a brand under review isn't public yet, but its own team can preview the storefront
	if status != "verified" && !own {
		return nil, httpx.NotFound("Brand not found")
	}
	b.Verified = status == "verified"
	b.CanMessage = accepts && !own

	// the owner also sees pieces held back for a drop that hasn't released
	cond := `WHERE p.brand_id = $1 AND ` + views.ProductVisible
	if own {
		cond = `WHERE p.brand_id = $1 AND NOT p.hidden`
	}
	products, err := views.ProductsWhere(ctx, s.DB, cond+` ORDER BY p.created_at DESC, p.id`, b.ID)
	if err != nil {
		return nil, err
	}
	badges, err := s.dropBadges(ctx, b.ID)
	if err != nil {
		return nil, err
	}
	b.Shop = make([]domain.BrandShopItem, 0, len(products))
	for _, p := range products {
		item := domain.BrandShopItem{Product: p.Summary}
		if p.DropID != nil {
			item.Badge = badges[*p.DropID]
		}
		b.Shop = append(b.Shop, item)
	}

	rows, err := s.DB.Query(ctx, `SELECT p.id, p.media -> 0, p.published_at
		FROM posts p LEFT JOIN brands b ON b.id = p.brand_id LEFT JOIN users u ON u.id = p.user_id
		WHERE `+views.PostVisible+` AND (p.brand_id = $2 OR EXISTS (SELECT 1 FROM post_amplifications a WHERE a.post_id = p.id AND a.brand_id = $2))
		ORDER BY p.published_at DESC LIMIT 60`, who.UserID, b.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	b.Posts = []domain.GridTile{}
	now := s.Now()
	for rows.Next() {
		var t domain.GridTile
		var at *time.Time
		if err := rows.Scan(&t.ID, &t.Image, &at); err != nil {
			return nil, err
		}
		if at != nil && now.Sub(*at) < 7*24*time.Hour {
			t.Badge = "NEW"
		}
		b.Posts = append(b.Posts, t)
	}
	return b, rows.Err()
}

func (s *Service) dropBadges(ctx context.Context, brandID string) (map[string]string, error) {
	rows, err := s.DB.Query(ctx, `SELECT id, title FROM drops WHERE brand_id = $1 AND
		(released_at IS NULL OR released_at > now() - make_interval(days => $2))`, brandID, recentDropDays)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, title string
		if err := rows.Scan(&id, &title); err != nil {
			return nil, err
		}
		out[id] = strings.ToUpper(title)
	}
	return out, rows.Err()
}

func (s *Service) followBrand(on bool) httpx.Handler {
	return func(_ http.ResponseWriter, r *http.Request) (any, error) {
		who := httpx.Who(r)
		ctx := r.Context()
		var id string
		err := s.DB.QueryRow(ctx, `SELECT id FROM brands WHERE handle = $1 AND (status = 'verified' OR id = $2)`,
			domain.NormalizeHandle(r.PathValue("handle")), who.BrandID).Scan(&id)
		if db.IsNotFound(err) {
			return nil, httpx.NotFound("Brand not found")
		}
		if err != nil {
			return nil, err
		}
		return nil, db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
			return account.SetBrandFollow(ctx, tx, who.UserID, id, on)
		})
	}
}

/* ---------- products ---------- */

func (s *Service) products(_ http.ResponseWriter, r *http.Request) (any, error) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	brand := r.URL.Query().Get("brand")
	rows, err := views.ProductsWhere(r.Context(), s.DB, `WHERE `+views.ProductVisible+`
		AND ($1 = '' OR b.name = $1)
		AND ($2 = '' OR strpos(lower(p.name || ' ' || b.name || ' ' || p.category), $2) > 0)
		ORDER BY p.created_at DESC, p.id LIMIT 100`, brand, q)
	if err != nil {
		return nil, err
	}
	out := make([]domain.ProductSummary, 0, len(rows))
	for _, p := range rows {
		out = append(out, p.Summary)
	}
	return out, nil
}

func (s *Service) product(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	id := r.PathValue("id")
	// anyone can open their own product, or one they've bought even if it's since been hidden
	rows, err := views.ProductsWhere(ctx, s.DB, `WHERE p.id = $1 AND (`+views.ProductVisible+` OR p.brand_id = $2
		OR EXISTS (SELECT 1 FROM orders o WHERE o.product_id = p.id AND o.user_id = $3 AND `+views.PaidOrder+`)
		OR EXISTS (SELECT 1 FROM closet_items c WHERE c.product_id = p.id AND c.user_id = $3))`, id, who.BrandID, who.UserID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, httpx.NotFound("This piece isn’t available any more")
	}
	row := rows[0]
	out := domain.Product{
		ProductSummary: row.Summary,
		Description:    row.Description,
		Images:         row.Images,
		FlatLay:        row.FlatLay,
		Colors:         row.Colors,
		Reviews:        []domain.Review{},
		WornBy:         []domain.WornBy{},
	}
	if out.Images == nil {
		out.Images = []domain.Photo{}
	}
	if out.Colors == nil {
		out.Colors = []domain.ColorOption{}
	}
	out.Sizes = make([]domain.SizeOption, 0, len(row.Sizes))
	for _, z := range row.Sizes {
		out.Sizes = append(out.Sizes, domain.SizeOption{Label: z.Label, Available: z.Stock > 0})
	}

	var showWorn bool
	var shipping domain.ShippingSettings
	if err := s.DB.QueryRow(ctx, `SELECT show_worn_by, shipping FROM brands WHERE id = $1`, row.Summary.Brand.ID).Scan(&showWorn, &shipping); err != nil {
		return nil, err
	}
	out.ShipsFrom, out.Processing, out.Returns = shipping.ShipsFrom, shipping.Processing, shipping.Returns

	if err := s.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM product_saves WHERE user_id = $1 AND product_id = $2)`, who.UserID, id).Scan(&out.Saved); err != nil {
		return nil, err
	}

	// fit comes from what wearers actually reported when posting
	var avg float64
	var wears int
	if err := s.DB.QueryRow(ctx, `SELECT COALESCE(avg(CASE worn_fit WHEN 'Runs small' THEN 0 WHEN 'Runs large' THEN 100 ELSE 50 END), 50), count(*)
		FROM posts WHERE worn_product_id = $1 AND status = 'published' AND worn_fit <> ''`, id).Scan(&avg, &wears); err != nil {
		return nil, err
	}
	out.Fit = domain.FitSummaryFor(avg, wears)

	if err := s.loadReviews(ctx, &out); err != nil {
		return nil, err
	}
	if showWorn {
		if err := s.loadWornBy(ctx, who.UserID, &out); err != nil {
			return nil, err
		}
	}
	if out.RecommendedSize, err = s.recommendSize(ctx, who.UserID, row); err != nil {
		return nil, err
	}

	// the delivered order, if any, this person hasn't reviewed yet
	var seq int64
	err = s.DB.QueryRow(ctx, `SELECT o.seq FROM orders o LEFT JOIN reviews rv ON rv.order_seq = o.seq
		WHERE o.user_id = $1 AND o.product_id = $2 AND rv.id IS NULL AND o.delivered_at IS NOT NULL
		ORDER BY o.delivered_at DESC LIMIT 1`, who.UserID, id).Scan(&seq)
	switch {
	case err == nil:
		out.CanReview = domain.ShopperOrderID(seq)
	case !db.IsNotFound(err):
		return nil, err
	}
	return out, nil
}

func (s *Service) loadReviews(ctx context.Context, p *domain.Product) error {
	if err := s.DB.QueryRow(ctx, `SELECT COALESCE(round(avg(rating)::numeric, 1), 0)::float8, count(*) FROM reviews WHERE product_id = $1`, p.ID).
		Scan(&p.Rating.Average, &p.Rating.Count); err != nil {
		return err
	}
	rows, err := s.DB.Query(ctx, `SELECT r.id, u.handle, r.rating, r.size, r.text FROM reviews r JOIN users u ON u.id = r.user_id
		WHERE r.product_id = $1 ORDER BY r.created_at DESC LIMIT 10`, p.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var rv domain.Review
		var size string
		if err := rows.Scan(&rv.ID, &rv.Author, &rv.Rating, &size, &rv.Text); err != nil {
			return err
		}
		stars := strings.Repeat("★", rv.Rating)
		rv.Meta = stars
		if size != "" {
			rv.Meta = "Size " + size + " · " + stars
		}
		p.Reviews = append(p.Reviews, rv)
	}
	return rows.Err()
}

func (s *Service) loadWornBy(ctx context.Context, viewerID string, p *domain.Product) error {
	rows, err := s.DB.Query(ctx, `SELECT p.id, u.handle, p.worn_size, p.worn_fit, p.media -> 0`+views.PostFrom+`
		WHERE p.worn_product_id = $2 AND p.user_id IS NOT NULL AND `+views.PostVisible+`
		ORDER BY p.published_at DESC LIMIT 4`, viewerID, p.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var w domain.WornBy
		var handle, size, fit string
		if err := rows.Scan(&w.PostID, &handle, &size, &fit, &w.Image); err != nil {
			return err
		}
		w.Handle = "@" + handle
		w.Fit = fitLabel(size, fit)
		p.WornBy = append(p.WornBy, w)
	}
	return rows.Err()
}

func fitLabel(size, fit string) string {
	word := map[string]string{"Runs small": "snug", "Runs large": "roomy", "True to size": "true"}[fit]
	switch {
	case size != "" && word != "":
		return "Size " + size + " · " + word
	case size != "":
		return "Size " + size
	}
	return word
}

// recommendSize uses the shopper's saved fit when this piece has it in stock.
func (s *Service) recommendSize(ctx context.Context, userID string, p views.ProductRow) (string, error) {
	var fit domain.FitProfile
	if err := s.DB.QueryRow(ctx, `SELECT fit FROM users WHERE id = $1`, userID).Scan(&fit); err != nil {
		return "", err
	}
	want := fit.Top
	if p.Summary.Category == "Bottoms" {
		want = fit.Bottom
	}
	if want != nil {
		for _, z := range p.Sizes {
			if z.Label == *want && z.Stock > 0 {
				return z.Label, nil
			}
		}
	}
	return p.Summary.DefaultSize, nil
}

func (s *Service) saveProduct(on bool) httpx.Handler {
	return func(_ http.ResponseWriter, r *http.Request) (any, error) {
		who := httpx.Who(r)
		id := r.PathValue("id")
		var err error
		if on {
			var exists bool
			if err = s.DB.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM products WHERE id = $1)`, id).Scan(&exists); err != nil {
				return nil, err
			}
			if !exists {
				return nil, httpx.NotFound("Product not found")
			}
			_, err = s.DB.Exec(r.Context(), `INSERT INTO product_saves (user_id, product_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, who.UserID, id)
		} else {
			_, err = s.DB.Exec(r.Context(), `DELETE FROM product_saves WHERE user_id = $1 AND product_id = $2`, who.UserID, id)
		}
		return nil, err
	}
}

// stockAlert remembers that someone wants to know when a sold-out size returns.
func (s *Service) stockAlert(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	var in struct {
		Size string `json:"size"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return nil, err
	}
	var stock int
	err := s.DB.QueryRow(r.Context(), `SELECT stock FROM product_sizes WHERE product_id = $1 AND label = $2`, r.PathValue("id"), in.Size).Scan(&stock)
	if db.IsNotFound(err) {
		return nil, httpx.NotFound("That size doesn’t exist")
	}
	if err != nil {
		return nil, err
	}
	if stock > 0 {
		return nil, httpx.BadRequest("That size is in stock")
	}
	_, err = s.DB.Exec(r.Context(), `INSERT INTO stock_alerts (user_id, product_id, size) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, who.UserID, r.PathValue("id"), in.Size)
	return nil, err
}

/* ---------- explore ---------- */

type exploreFilters struct {
	q, category, size, color, price, tryOn, sort string
}

func filtersFrom(r *http.Request) exploreFilters {
	v := r.URL.Query()
	get := func(key, fallback string) string {
		if s := v.Get(key); s != "" {
			return s
		}
		return fallback
	}
	return exploreFilters{
		q:        strings.ToLower(strings.TrimSpace(v.Get("q"))),
		category: get("category", "All"),
		size:     get("size", "All"),
		color:    get("color", "All"),
		price:    get("price", "any"),
		tryOn:    get("tryOn", "any"),
		sort:     get("sort", "trending"),
	}
}

// exploreSQL builds the product query from the filters; values are always bound parameters.
func exploreSQL(f exploreFilters) (string, []any) {
	args := []any{f.category, f.color, f.q}
	cond := `WHERE ` + views.ProductVisible + `
		AND ($1 = 'All' OR p.category = $1)
		AND ($2 = 'All' OR p.color = $2)
		AND ($3 = '' OR strpos(lower(p.name || ' ' || b.name || ' ' || p.category), $3) > 0)`
	if f.tryOn == "ready" {
		cond += ` AND p.flat_lay IS NOT NULL`
	}
	switch f.price {
	case "lt100":
		cond += ` AND p.price < 10000`
	case "100to200":
		cond += ` AND p.price BETWEEN 10000 AND 20000`
	case "gt200":
		cond += ` AND p.price > 20000`
	}
	if f.size != "All" {
		args = append(args, f.size)
		cond += fmt.Sprintf(` AND EXISTS (SELECT 1 FROM product_sizes z WHERE z.product_id = p.id AND z.label = $%d AND z.stock > 0)`, len(args))
	}
	switch f.sort {
	case "newest":
		cond += ` ORDER BY p.created_at DESC, p.id`
	case "price-asc":
		cond += ` ORDER BY p.price, p.id`
	case "price-desc":
		cond += ` ORDER BY p.price DESC, p.id`
	default: // trending: recent sales count most, then saves and wears
		cond += ` ORDER BY (
			5 * (SELECT count(*) FROM orders o WHERE o.product_id = p.id AND ` + views.PaidOrder + ` AND o.placed_at > now() - interval '30 days')
			+ 2 * (SELECT count(*) FROM product_saves ps WHERE ps.product_id = p.id)
			+ 3 * (SELECT count(*) FROM posts w WHERE w.worn_product_id = p.id AND w.status = 'published')
		) DESC, p.created_at DESC, p.id`
	}
	return cond + ` LIMIT 60`, args
}

// exploreByTag answers a "#hashtag" search with the posts using it, newest first.
func (s *Service) exploreByTag(ctx context.Context, viewerID, tag string) (any, error) {
	rows, err := s.DB.Query(ctx, `SELECT p.id, p.media -> 0, ps.product_id, b.name, u.handle
		FROM post_hashtags h JOIN posts p ON p.id = h.post_id
		JOIN post_shop ps ON ps.post_id = p.id AND ps.position = 0
		LEFT JOIN brands b ON b.id = p.brand_id LEFT JOIN users u ON u.id = p.user_id
		WHERE lower(h.tag) = lower($2) AND `+views.PostVisible+` ORDER BY p.published_at DESC LIMIT 60`, viewerID, tag)
	if err != nil {
		return nil, err
	}
	type hit struct {
		id, product string
		image       *domain.Photo
		chip        string
	}
	hits := []hit{}
	ids := []string{}
	for rows.Next() {
		var h hit
		var brand, handle *string
		if err := rows.Scan(&h.id, &h.image, &h.product, &brand, &handle); err != nil {
			rows.Close()
			return nil, err
		}
		if handle != nil {
			h.chip = "WEAR · @" + *handle
		} else if brand != nil {
			h.chip = "POST · " + *brand
		}
		hits = append(hits, h)
		ids = append(ids, h.product)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	products, err := views.Summaries(ctx, s.DB, ids)
	if err != nil {
		return nil, err
	}
	items := make([]domain.ExploreItem, 0, len(hits))
	for _, h := range hits {
		items = append(items, domain.ExploreItem{ID: "post-" + h.id, Product: products[h.product], Image: h.image, Chip: h.chip, Aspect: tileAspect(h.id), PostID: h.id})
	}
	return map[string]any{"items": items}, nil
}

func (s *Service) explore(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	filters := filtersFrom(r)
	if strings.HasPrefix(filters.q, "#") {
		return s.exploreByTag(ctx, who.UserID, filters.q)
	}
	cond, args := exploreSQL(filters)
	rows, err := views.ProductsWhere(ctx, s.DB, cond, args...)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(rows))
	for _, p := range rows {
		ids = append(ids, p.Summary.ID)
	}
	posts, err := s.latestPostByProduct(ctx, who.UserID, ids)
	if err != nil {
		return nil, err
	}
	items := make([]domain.ExploreItem, 0, len(rows)*2)
	for _, p := range rows {
		// every piece gets a shop tile, plus the latest post or wear featuring it
		if p.Summary.Image != nil || p.Summary.Placeholder != "" {
			items = append(items, domain.ExploreItem{
				ID:      "prod-" + p.Summary.ID,
				Product: p.Summary,
				Image:   p.Summary.Image,
				Chip:    fmt.Sprintf("$%s · %s", priceLabel(p.Summary.Price), p.Summary.Brand.Name),
				Aspect:  tileAspect(p.Summary.ID),
			})
		}
		if post, ok := posts[p.Summary.ID]; ok {
			items = append(items, domain.ExploreItem{
				ID:      "post-" + post.id + "-" + p.Summary.ID,
				Product: p.Summary,
				Image:   post.image,
				Chip:    post.chip,
				Aspect:  tileAspect(post.id),
				PostID:  post.id,
			})
		}
	}
	return map[string]any{"items": items}, nil
}

func priceLabel(cents int) string {
	if cents%100 == 0 {
		return strconv.Itoa(cents / 100)
	}
	return fmt.Sprintf("%.2f", float64(cents)/100)
}

// tileAspect gives each tile a stable height so the masonry grid keeps its rhythm between visits.
func tileAspect(id string) float64 {
	h := fnv.New32a()
	h.Write([]byte(id))
	heights := []float64{180, 200, 220, 230, 240, 250, 260}
	return 195 / heights[h.Sum32()%uint32(len(heights))]
}

type explorePost struct {
	id    string
	image *domain.Photo
	chip  string
}

func (s *Service) latestPostByProduct(ctx context.Context, viewerID string, ids []string) (map[string]explorePost, error) {
	out := map[string]explorePost{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.DB.Query(ctx, `SELECT DISTINCT ON (t.product_id) t.product_id, p.id, p.media -> 0, b.name, u.handle
		FROM post_shop t JOIN posts p ON p.id = t.post_id
		LEFT JOIN brands b ON b.id = p.brand_id LEFT JOIN users u ON u.id = p.user_id
		WHERE t.product_id = ANY($2) AND `+views.PostVisible+` AND p.type <> 'Reel' AND p.media <> '[]'
		ORDER BY t.product_id, p.published_at DESC`, viewerID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var pid string
		var e explorePost
		var brand, handle *string
		if err := rows.Scan(&pid, &e.id, &e.image, &brand, &handle); err != nil {
			return nil, err
		}
		switch {
		case handle != nil:
			e.chip = "WEAR · @" + *handle
		case brand != nil:
			e.chip = "POST · " + *brand
		}
		out[pid] = e
	}
	return out, rows.Err()
}

func (s *Service) trends(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	rows, err := s.DB.Query(r.Context(), `SELECT h.tag, count(*),
			(array_agg(p.media -> 0 ORDER BY p.published_at DESC))[1]
		FROM post_hashtags h JOIN posts p ON p.id = h.post_id
		LEFT JOIN brands b ON b.id = p.brand_id LEFT JOIN users u ON u.id = p.user_id
		WHERE `+views.PostVisible+` AND p.published_at > now() - interval '30 days'
		GROUP BY h.tag ORDER BY count(*) DESC, h.tag LIMIT 8`, who.UserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Trend{}
	for rows.Next() {
		var t domain.Trend
		var n int
		if err := rows.Scan(&t.Tag, &n, &t.Image); err != nil {
			return nil, err
		}
		word := "wears"
		if n == 1 {
			word = "wear"
		}
		t.Count = fmt.Sprintf("%d %s", n, word)
		out = append(out, t)
	}
	return out, rows.Err()
}
