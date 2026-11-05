package studio

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"wearwhat-be/internal/db"
	"wearwhat-be/internal/domain"
	"wearwhat-be/internal/httpx"
	"wearwhat-be/internal/inbox"
	"wearwhat-be/internal/media"
	"wearwhat-be/internal/secure"
	"wearwhat-be/internal/views"
)

const maxProductImages = 8

/* ---------- catalog list ---------- */

func (s *Service) catalogItem(ctx context.Context, q db.Q, p views.ProductRow) (domain.CatalogItem, error) {
	item := domain.CatalogItem{Product: p.Summary, Stock: p.Stock(), Sizes: p.Sizes, Hidden: p.Hidden, Status: domain.CatalogStatus(p.Stock(), p.Hidden)}
	if item.Sizes == nil {
		item.Sizes = []domain.SizeStock{}
	}
	err := q.QueryRow(ctx, `SELECT COALESCE(sum(o.qty), 0) FROM orders o WHERE o.product_id = $1 AND `+views.PaidOrder+` AND o.placed_at > $2::timestamptz - interval '30 days'`,
		p.Summary.ID, s.Now()).Scan(&item.Sold30d)
	return item, err
}

func (s *Service) catalog(_ http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	rows, err := views.ProductsWhere(ctx, s.DB, `WHERE p.brand_id = $1 ORDER BY p.created_at DESC, p.id`, httpx.Who(r).BrandID)
	if err != nil {
		return nil, err
	}
	out := make([]domain.CatalogItem, 0, len(rows))
	for _, p := range rows {
		item, err := s.catalogItem(ctx, s.DB, p)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}

// ownProduct loads a product only if it belongs to the caller's brand.
func ownProduct(ctx context.Context, q db.Q, who *httpx.Principal, id string) (views.ProductRow, error) {
	rows, err := views.ProductsWhere(ctx, q, `WHERE p.id = $1 AND p.brand_id = $2`, id, who.BrandID)
	if err != nil {
		return views.ProductRow{}, err
	}
	if len(rows) == 0 {
		return views.ProductRow{}, httpx.NotFound("Product not found")
	}
	return rows[0], nil
}

/* ---------- stock ---------- */

func (s *Service) setStock(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	var in struct {
		Size  string `json:"size"`
		Stock int    `json:"stock"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return nil, err
	}
	if in.Stock < 0 {
		in.Stock = 0
	}
	id := r.PathValue("id")
	var item domain.CatalogItem
	err := db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		if _, err := ownProduct(ctx, tx, who, id); err != nil {
			return err
		}
		var before int
		err := tx.QueryRow(ctx, `SELECT stock FROM product_sizes WHERE product_id = $1 AND label = $2 FOR UPDATE`, id, in.Size).Scan(&before)
		if db.IsNotFound(err) {
			return httpx.NotFound("That size isn’t on this product")
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE product_sizes SET stock = $3 WHERE product_id = $1 AND label = $2`, id, in.Size, in.Stock); err != nil {
			return err
		}
		if before == 0 && in.Stock > 0 {
			if err := s.notifyRestock(ctx, tx, id, in.Size); err != nil {
				return err
			}
		}
		p, err := ownProduct(ctx, tx, who, id)
		if err != nil {
			return err
		}
		item, err = s.catalogItem(ctx, tx, p)
		return err
	})
	if err != nil {
		return nil, err
	}
	return item, nil
}

// notifyRestock tells everyone waiting on this size, then clears the waitlist.
func (s *Service) notifyRestock(ctx context.Context, tx pgx.Tx, productID, size string) error {
	rows, err := tx.Query(ctx, `DELETE FROM stock_alerts WHERE product_id = $1 AND size = $2 RETURNING user_id`, productID, size)
	if err != nil {
		return err
	}
	users, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(users) == 0 {
		return err
	}
	p, err := views.ProductByID(ctx, tx, productID)
	if err != nil {
		return err
	}
	for _, u := range users {
		note := inbox.Note{UserID: u, Who: p.Summary.Brand.Name, Text: p.Summary.Name + " is back in " + size + ".", Kind: "Drops", Thumb: p.Summary.Image, Href: "/p/" + productID, Pref: "drops"}
		if p.Summary.TryOnReady {
			note.Action = &domain.NotificationAction{Type: "try", Label: "TRY ON", Target: productID}
		}
		if err := inbox.Notify(ctx, tx, note); err != nil {
			return err
		}
	}
	return nil
}

/* ---------- product editor ---------- */

func (s *Service) getProduct(_ http.ResponseWriter, r *http.Request) (any, error) {
	p, err := ownProduct(r.Context(), s.DB, httpx.Who(r), r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	out := domain.ProductInput{
		Name:        p.Summary.Name,
		Price:       p.Summary.Price,
		Description: p.Description,
		Category:    p.Summary.Category,
		Colors:      []string{},
		Sizes:       []string{},
		SizeStock:   map[string]int{},
		Images:      []string{},
		Live:        !p.Hidden,
		InDrop:      p.DropID != nil,
		TryOnReady:  p.FlatLay != nil,
	}
	for _, c := range p.Colors {
		out.Colors = append(out.Colors, c.Name)
	}
	for _, z := range p.Sizes {
		out.Sizes = append(out.Sizes, z.Label)
		out.SizeStock[z.Label] = z.Stock
	}
	for _, img := range p.Images {
		out.Images = append(out.Images, img.URL)
	}
	if p.FlatLay != nil {
		out.FlatLay = &p.FlatLay.URL
	}
	return out, nil
}

// cleanedProduct is validated input ready to store.
type cleanedProduct struct {
	domain.ProductInput
	colors  []domain.ColorOption
	images  []domain.Photo
	flatLay *domain.Photo
	dropID  *string
}

func (s *Service) cleanProduct(ctx context.Context, q db.Q, who *httpx.Principal, in domain.ProductInput, existing *views.ProductRow) (cleanedProduct, error) {
	c := cleanedProduct{ProductInput: in}
	c.Name = strings.TrimSpace(in.Name)
	c.Description = strings.TrimSpace(in.Description)
	switch {
	case c.Name == "" || len([]rune(c.Name)) > 120:
		return c, httpx.BadRequest("Name the product")
	case in.Price <= 0 || in.Price > 100_000_000:
		return c, httpx.BadRequest("Add a price")
	case len([]rune(c.Description)) > 2000:
		return c, httpx.BadRequest("Keep the description under 2000 characters")
	}
	if c.Category == "" {
		c.Category = "Tops"
	}
	if !slices.Contains(domain.Categories, c.Category) {
		return c, httpx.BadRequest("Choose a category")
	}

	names := in.Colors
	if len(names) == 0 {
		names = []string{"Black"}
	}
	c.colors = make([]domain.ColorOption, 0, len(names))
	for _, n := range names {
		hex, ok := domain.ColorHex[n]
		if !ok {
			return c, httpx.BadRequest("Choose colours from the list")
		}
		c.colors = append(c.colors, domain.ColorOption{Name: n, Hex: hex})
	}

	if len(in.Sizes) == 0 {
		return c, httpx.BadRequest("Pick at least one size")
	}
	for _, size := range in.Sizes {
		if strings.TrimSpace(size) == "" || len(size) > 12 {
			return c, httpx.BadRequest("Check the sizes")
		}
		if in.SizeStock[size] < 0 {
			return c, httpx.BadRequest("Stock can’t be negative")
		}
	}

	if len(in.Images) > maxProductImages {
		return c, httpx.BadRequest("Use up to 8 photos")
	}
	// images already on the product keep their presentation settings (cutout tint, fit)
	known := map[string]domain.Photo{}
	if existing != nil {
		for _, img := range existing.Images {
			known[img.URL] = img
		}
	}
	c.images = make([]domain.Photo, 0, len(in.Images))
	for _, url := range in.Images {
		if img, ok := known[url]; ok {
			c.images = append(c.images, img)
			continue
		}
		photo, err := media.Photo(ctx, q, who, url)
		if err != nil {
			return c, err
		}
		c.images = append(c.images, *photo)
	}
	if in.FlatLay != nil && *in.FlatLay != "" {
		if existing != nil && existing.FlatLay != nil && existing.FlatLay.URL == *in.FlatLay {
			c.flatLay = existing.FlatLay
		} else {
			photo, err := media.Photo(ctx, q, who, *in.FlatLay)
			if err != nil {
				return c, err
			}
			c.flatLay = photo
		}
	}

	if in.InDrop {
		if existing != nil && existing.DropID != nil {
			c.dropID = existing.DropID
		} else {
			next, err := s.nextDrop(ctx, q, who.BrandID)
			if err != nil {
				return c, err
			}
			if next == nil {
				return c, httpx.BadRequest("Set a date for your next drop first")
			}
			c.dropID = &next.ID
		}
	}
	return c, nil
}

// insertProduct creates a product and adds its photos to the brand's media library.
func (s *Service) insertProduct(ctx context.Context, tx pgx.Tx, who *httpx.Principal, in domain.ProductInput) (string, error) {
	c, err := s.cleanProduct(ctx, tx, who, in, nil)
	if err != nil {
		return "", err
	}
	id := secure.ID("p")
	color := ""
	if len(c.colors) > 0 {
		color = c.colors[0].Name
	}
	now := s.Now()
	if _, err := tx.Exec(ctx, `INSERT INTO products (id, brand_id, name, price, category, description, color, colors, images, flat_lay, placeholder, hidden, drop_id, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$14)`,
		id, who.BrandID, c.Name, c.Price, c.Category, c.Description, color, c.colors, c.images, c.flatLay, c.Name, !c.Live, c.dropID, now); err != nil {
		return "", err
	}
	if err := saveSizes(ctx, tx, id, c.Sizes, c.SizeStock); err != nil {
		return "", err
	}
	return id, addToLibrary(ctx, tx, who.BrandID, c.images)
}

func saveSizes(ctx context.Context, tx pgx.Tx, id string, sizes []string, stock map[string]int) error {
	for i, size := range sizes {
		if _, err := tx.Exec(ctx, `INSERT INTO product_sizes (product_id, label, stock, position) VALUES ($1,$2,$3,$4)
			ON CONFLICT (product_id, label) DO UPDATE SET stock = EXCLUDED.stock, position = EXCLUDED.position`, id, size, stock[size], i); err != nil {
			return err
		}
	}
	_, err := tx.Exec(ctx, `DELETE FROM product_sizes WHERE product_id = $1 AND NOT (label = ANY($2))`, id, sizes)
	return err
}

// addToLibrary makes newly uploaded product photos available to the post composer.
func addToLibrary(ctx context.Context, q db.Q, brandID string, images []domain.Photo) error {
	for _, img := range images {
		if !strings.HasPrefix(img.URL, "/uploads/") {
			continue
		}
		if _, err := q.Exec(ctx, `INSERT INTO brand_media (id, brand_id, media)
			SELECT $1,$2,$3 WHERE NOT EXISTS (SELECT 1 FROM brand_media WHERE brand_id = $2 AND media ->> 'url' = $4)`,
			secure.ID("md"), brandID, img, img.URL); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) createProduct(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	var in domain.ProductInput
	if err := httpx.Decode(r, &in); err != nil {
		return nil, err
	}
	var id string
	err := db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		var e error
		id, e = s.insertProduct(ctx, tx, who, in)
		return e
	})
	if err != nil {
		return nil, err
	}
	return map[string]string{"id": id}, nil
}

func (s *Service) updateProduct(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	id := r.PathValue("id")
	var in domain.ProductInput
	if err := httpx.Decode(r, &in); err != nil {
		return nil, err
	}
	err := db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		existing, err := ownProduct(ctx, tx, who, id)
		if err != nil {
			return err
		}
		c, err := s.cleanProduct(ctx, tx, who, in, &existing)
		if err != nil {
			return err
		}
		color := ""
		if len(c.colors) > 0 {
			color = c.colors[0].Name
		}
		if _, err := tx.Exec(ctx, `UPDATE products SET name = $2, price = $3, category = $4, description = $5, color = $6, colors = $7, images = $8,
			flat_lay = $9, placeholder = $10, hidden = $11, drop_id = $12, updated_at = $13 WHERE id = $1`,
			id, c.Name, c.Price, c.Category, c.Description, color, c.colors, c.images, c.flatLay, c.Name, !c.Live, c.dropID, s.Now()); err != nil {
			return err
		}
		restocked := []string{}
		for _, z := range existing.Sizes {
			if z.Stock == 0 && c.SizeStock[z.Label] > 0 {
				restocked = append(restocked, z.Label)
			}
		}
		if err := saveSizes(ctx, tx, id, c.Sizes, c.SizeStock); err != nil {
			return err
		}
		for _, size := range restocked {
			if err := s.notifyRestock(ctx, tx, id, size); err != nil {
				return err
			}
		}
		return addToLibrary(ctx, tx, who.BrandID, c.images)
	})
	if err != nil {
		return nil, err
	}
	return map[string]string{"id": id}, nil
}

/* ---------- drops ---------- */

// nextDrop is the brand's soonest unreleased drop, or nil.
func (s *Service) nextDrop(ctx context.Context, q db.Q, brandID string) (*domain.NextDrop, error) {
	var d domain.NextDrop
	var at time.Time
	err := q.QueryRow(ctx, `SELECT d.id, d.title, d.releases_at, (SELECT count(*) FROM products p WHERE p.drop_id = d.id),
			(SELECT followers FROM brands b WHERE b.id = d.brand_id)
		FROM drops d WHERE d.brand_id = $1 AND d.released_at IS NULL ORDER BY d.releases_at LIMIT 1`, brandID).
		Scan(&d.ID, &d.Name, &at, &d.Pieces, &d.NotifyCount)
	if db.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d.Title = domain.DropLabel(d.Name, at)
	d.ReleasesAt = at.UTC().Format(time.RFC3339)
	return &d, nil
}

func (s *Service) getNextDrop(_ http.ResponseWriter, r *http.Request) (any, error) {
	d, err := s.nextDrop(r.Context(), s.DB, httpx.Who(r).BrandID)
	if err != nil || d == nil {
		return nil, err // no drop scheduled answers 204
	}
	return d, nil
}

func (s *Service) putNextDrop(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	var in struct {
		Name       string `json:"name"`
		ReleasesAt string `json:"releasesAt"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || len([]rune(name)) > 40 {
		return nil, httpx.BadRequest("Name the drop")
	}
	at, err := time.Parse(time.RFC3339, in.ReleasesAt)
	if err != nil {
		return nil, httpx.BadRequest("Pick a release date and time")
	}
	if !at.After(s.Now()) {
		return nil, httpx.BadRequest("Pick a date in the future")
	}
	current, err := s.nextDrop(ctx, s.DB, who.BrandID)
	if err != nil {
		return nil, err
	}
	if current != nil {
		_, err = s.DB.Exec(ctx, `UPDATE drops SET title = $2, releases_at = $3 WHERE id = $1`, current.ID, name, at)
	} else {
		_, err = s.DB.Exec(ctx, `INSERT INTO drops (id, brand_id, title, releases_at) VALUES ($1,$2,$3,$4)`, secure.ID("dr"), who.BrandID, name, at)
	}
	if err != nil {
		return nil, err
	}
	// scheduled "with next drop" posts follow the drop's new date
	if current != nil {
		if _, err := s.DB.Exec(ctx, `UPDATE posts SET publish_at = $2 WHERE brand_id = $1 AND status = 'scheduled' AND with_next_drop`, who.BrandID, at); err != nil {
			return nil, err
		}
	}
	return s.nextDrop(ctx, s.DB, who.BrandID)
}

// ReleaseDueDrops is run by the background job: reveal the pieces, publish the posts
// waiting on the drop, and tell followers.
func (s *Service) ReleaseDueDrops(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, `SELECT id FROM drops WHERE released_at IS NULL AND releases_at <= $1 ORDER BY releases_at LIMIT 50`, s.Now())
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range ids {
		err := db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
			var brandID, title string
			now := s.Now()
			// the conditional update makes a release happen once even with several servers
			err := tx.QueryRow(ctx, `UPDATE drops SET released_at = $2 WHERE id = $1 AND released_at IS NULL RETURNING brand_id, title`, id, now).Scan(&brandID, &title)
			if db.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE posts SET status = 'published', published_at = $2, with_next_drop = false,
				expires_at = CASE WHEN type = 'Story' THEN $2::timestamptz + interval '24 hours' ELSE NULL END
				WHERE brand_id = $1 AND status = 'scheduled' AND with_next_drop`, brandID, now); err != nil {
				return err
			}
			var handle, name string
			var avatar *domain.Photo
			var verified bool
			if err := tx.QueryRow(ctx, `SELECT handle, name, avatar, status = 'verified' FROM brands WHERE id = $1`, brandID).Scan(&handle, &name, &avatar, &verified); err != nil {
				return err
			}
			if !verified {
				return nil
			}
			var pieces int
			var thumb *domain.Photo
			err = tx.QueryRow(ctx, `SELECT count(*), (array_agg(images -> 0 ORDER BY created_at DESC))[1] FROM products WHERE drop_id = $1 AND NOT hidden`, id).Scan(&pieces, &thumb)
			if err != nil {
				return err
			}
			return inbox.NotifyFollowers(ctx, tx, brandID, inbox.Note{
				Who: name, Text: dropText(title, pieces), Kind: "Drops", Avatar: avatar, Thumb: thumb, Href: "/b/" + handle, Pref: "drops",
			})
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func dropText(title string, pieces int) string {
	switch pieces {
	case 0:
		return "dropped " + title + "."
	case 1:
		return "dropped " + title + " — 1 piece."
	}
	return "dropped " + title + " — " + itoa(pieces) + " pieces."
}
