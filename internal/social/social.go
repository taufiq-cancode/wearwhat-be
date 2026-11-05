// Package social serves the feed, post details, likes, saves, comments, wears, reports and events.
package social

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"wearwhat-be/internal/app"
	"wearwhat-be/internal/db"
	"wearwhat-be/internal/domain"
	"wearwhat-be/internal/httpx"
	"wearwhat-be/internal/inbox"
	"wearwhat-be/internal/media"
	"wearwhat-be/internal/secure"
	"wearwhat-be/internal/views"
)

type Service struct{ *app.Deps }

func (s *Service) Routes(r *httpx.Router) {
	r.User("GET /feed", s.feed)
	r.User("GET /posts/{id}", s.getPost)
	r.User("PUT /posts/{id}/like", s.like(true))
	r.User("DELETE /posts/{id}/like", s.like(false))
	r.User("PUT /posts/{id}/save", s.save(true))
	r.User("DELETE /posts/{id}/save", s.save(false))
	r.User("DELETE /posts/{id}", s.deletePost)
	r.User("GET /posts/{id}/comments", s.listComments)
	r.User("POST /posts/{id}/comments", s.addComment)
	r.User("DELETE /comments/{id}", s.deleteComment)

	r.User("GET /me/wears", s.myWears)
	r.User("GET /me/saved", s.mySaved)
	r.User("GET /me/closet", s.myCloset)
	r.User("POST /wears", s.createWear)

	r.User("POST /reports", s.report)
	r.User("POST /events", s.event)
}

/* ---------- loading posts ---------- */

const postCols = `SELECT p.id, p.brand_id, p.user_id, p.media, p.aspect, p.subtitle, p.caption, p.location, p.likes, p.comments_count,
	p.worn_product_id, p.worn_size, p.worn_fit,
	b.handle, b.name, b.avatar, b.status, u.handle, u.name, u.avatar,
	EXISTS (SELECT 1 FROM post_likes l WHERE l.post_id = p.id AND l.user_id = $1),
	EXISTS (SELECT 1 FROM post_saves v WHERE v.post_id = p.id AND v.user_id = $1)`

// loadPosts runs a post query (first parameter = viewer id) and resolves tags and shop items.
func (s *Service) loadPosts(ctx context.Context, who *httpx.Principal, cond string, args ...any) ([]domain.Post, error) {
	rows, err := s.DB.Query(ctx, postCols+views.PostFrom+cond, args...)
	if err != nil {
		return nil, err
	}
	type wearRef struct {
		product *string
		size    string
		fit     string
	}
	posts := []domain.Post{}
	wears := map[string]wearRef{}
	for rows.Next() {
		var p domain.Post
		var brandID, userID, wornID *string
		var bHandle, bName, bStatus, uHandle, uName *string
		var bAvatar, uAvatar *domain.Photo
		var location, wornSize, wornFit string
		if err := rows.Scan(&p.ID, &brandID, &userID, &p.Media, &p.Aspect, &p.Subtitle, &p.Caption, &location, &p.Likes, &p.CommentsCount,
			&wornID, &wornSize, &wornFit, &bHandle, &bName, &bAvatar, &bStatus, &uHandle, &uName, &uAvatar, &p.Liked, &p.Saved); err != nil {
			rows.Close()
			return nil, err
		}
		if brandID != nil {
			p.Author = domain.Author{Kind: "brand", Handle: *bHandle, Name: *bName, Avatar: bAvatar, Verified: bStatus != nil && *bStatus == "verified"}
			p.Mine = *brandID == who.BrandID
		} else {
			p.Author = domain.Author{Kind: "user", Handle: *uHandle, Name: *uHandle, Avatar: uAvatar}
			p.Mine = userID != nil && *userID == who.UserID
		}
		if p.Media == nil {
			p.Media = []domain.Photo{}
		}
		if len(p.Media) > 0 {
			img := p.Media[0]
			p.Image = &img
		}
		if location != "" {
			if p.Subtitle != "" {
				p.Subtitle += " · "
			}
			p.Subtitle += location
		}
		p.Tags = []domain.PostTag{}
		p.Shop = []domain.ProductSummary{}
		if wornID != nil {
			wears[p.ID] = wearRef{product: wornID, size: wornSize, fit: wornFit}
		}
		posts = append(posts, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(posts) == 0 {
		return posts, nil
	}

	ids := make([]string, len(posts))
	index := make(map[string]int, len(posts))
	for i, p := range posts {
		ids[i] = p.ID
		index[p.ID] = i
	}

	// a tag or shop entry for a piece that's since been hidden simply disappears
	tagRows, err := s.DB.Query(ctx, `SELECT t.post_id, t.label, t.x, t.y, t.product_id FROM post_tags t
		JOIN products p ON p.id = t.product_id JOIN brands b ON b.id = p.brand_id
		WHERE t.post_id = ANY($1) AND `+views.ProductVisible+` ORDER BY t.post_id, t.position`, ids)
	if err != nil {
		return nil, err
	}
	for tagRows.Next() {
		var postID string
		var t domain.PostTag
		if err := tagRows.Scan(&postID, &t.Label, &t.X, &t.Y, &t.ProductID); err != nil {
			tagRows.Close()
			return nil, err
		}
		posts[index[postID]].Tags = append(posts[index[postID]].Tags, t)
	}
	tagRows.Close()
	if err := tagRows.Err(); err != nil {
		return nil, err
	}

	shopRows, err := s.DB.Query(ctx, `SELECT t.post_id, t.product_id FROM post_shop t
		JOIN products p ON p.id = t.product_id JOIN brands b ON b.id = p.brand_id
		WHERE t.post_id = ANY($1) AND `+views.ProductVisible+` ORDER BY t.post_id, t.position`, ids)
	if err != nil {
		return nil, err
	}
	type shopRef struct{ post, product string }
	var refs []shopRef
	wanted := []string{}
	for shopRows.Next() {
		var r shopRef
		if err := shopRows.Scan(&r.post, &r.product); err != nil {
			shopRows.Close()
			return nil, err
		}
		refs = append(refs, r)
		wanted = append(wanted, r.product)
	}
	shopRows.Close()
	if err := shopRows.Err(); err != nil {
		return nil, err
	}
	for _, w := range wears {
		wanted = append(wanted, *w.product)
	}
	summaries, err := views.Summaries(ctx, s.DB, wanted)
	if err != nil {
		return nil, err
	}
	for _, r := range refs {
		if sum, ok := summaries[r.product]; ok {
			posts[index[r.post]].Shop = append(posts[index[r.post]].Shop, sum)
		}
	}
	for postID, w := range wears {
		if sum, ok := summaries[*w.product]; ok {
			posts[index[postID]].Wear = &domain.PostWear{Product: sum, Meta: wearMeta(sum.Brand.Name, w.size, w.fit)}
		}
	}
	return posts, nil
}

func wearMeta(brand, size, fit string) string {
	parts := []string{brand}
	if size != "" {
		parts = append(parts, "SIZE "+size)
	}
	switch fit {
	case "Runs small":
		parts = append(parts, "RUNS SMALL")
	case "Runs large":
		parts = append(parts, "RUNS LARGE")
	case "True to size":
		parts = append(parts, "FITS TRUE")
	}
	return strings.Join(parts, " · ")
}

/* ---------- feed ---------- */

func (s *Service) feed(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	posts, err := s.loadPosts(ctx, who, `WHERE `+views.PostVisible+`
		ORDER BY (p.user_id = $1
			OR EXISTS (SELECT 1 FROM follows_brand fb WHERE fb.user_id = $1 AND fb.brand_id = p.brand_id)
			OR EXISTS (SELECT 1 FROM follows_user fu WHERE fu.follower_id = $1 AND fu.followee_id = p.user_id AND fu.approved)
			OR EXISTS (SELECT 1 FROM post_amplifications pa JOIN follows_brand fa ON fa.brand_id = pa.brand_id WHERE pa.post_id = p.id AND fa.user_id = $1)
		) IS TRUE DESC, p.published_at DESC LIMIT 50`, who.UserID)
	if err != nil {
		return nil, err
	}

	// the drops rail: brands with a fresh drop first, then the ones you follow
	rows, err := s.DB.Query(ctx, `SELECT b.id, b.handle, b.name, b.avatar,
			EXISTS (SELECT 1 FROM drops d WHERE d.brand_id = b.id AND d.released_at > now() - interval '14 days') AS fresh
		FROM brands b WHERE b.status = 'verified' AND EXISTS (SELECT 1 FROM products x WHERE x.brand_id = b.id AND NOT x.hidden)
		ORDER BY fresh DESC, EXISTS (SELECT 1 FROM follows_brand f WHERE f.user_id = $1 AND f.brand_id = b.id) DESC, b.followers DESC LIMIT 12`, who.UserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	drops := []domain.DropBubble{}
	for rows.Next() {
		var d domain.DropBubble
		if err := rows.Scan(&d.Brand.ID, &d.Brand.Handle, &d.Brand.Name, &d.Image, &d.IsNew); err != nil {
			return nil, err
		}
		drops = append(drops, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return domain.Feed{Drops: drops, Posts: posts}, nil
}

func (s *Service) visiblePost(ctx context.Context, who *httpx.Principal, id string) (domain.Post, error) {
	posts, err := s.loadPosts(ctx, who, `WHERE p.id = $2 AND `+views.PostVisible, who.UserID, id)
	if err != nil {
		return domain.Post{}, err
	}
	if len(posts) == 0 {
		return domain.Post{}, httpx.NotFound("Post not found")
	}
	return posts[0], nil
}

func (s *Service) getPost(_ http.ResponseWriter, r *http.Request) (any, error) {
	return s.visiblePost(r.Context(), httpx.Who(r), r.PathValue("id"))
}

func (s *Service) deletePost(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	tag, err := s.DB.Exec(ctx, `DELETE FROM posts WHERE id = $1 AND (user_id = $2 OR ($3 <> '' AND brand_id = $3))`, r.PathValue("id"), who.UserID, contentBrand(who))
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, httpx.NotFound("Post not found")
	}
	return nil, nil
}

// contentBrand is the brand this person may manage posts for, or "".
func contentBrand(who *httpx.Principal) string {
	if who.Can(httpx.CapContent) {
		return who.BrandID
	}
	return ""
}

func (s *Service) like(on bool) httpx.Handler {
	return func(_ http.ResponseWriter, r *http.Request) (any, error) {
		who := httpx.Who(r)
		ctx := r.Context()
		id := r.PathValue("id")
		if _, err := s.visiblePost(ctx, who, id); err != nil {
			return nil, err
		}
		return nil, db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
			if !on {
				tag, err := tx.Exec(ctx, `DELETE FROM post_likes WHERE user_id = $1 AND post_id = $2`, who.UserID, id)
				if err != nil || tag.RowsAffected() == 0 {
					return err
				}
				_, err = tx.Exec(ctx, `UPDATE posts SET likes = GREATEST(likes - 1, 0) WHERE id = $1`, id)
				return err
			}
			tag, err := tx.Exec(ctx, `INSERT INTO post_likes (user_id, post_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, who.UserID, id)
			if err != nil || tag.RowsAffected() == 0 {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE posts SET likes = likes + 1 WHERE id = $1`, id)
			return err
		})
	}
}

func (s *Service) save(on bool) httpx.Handler {
	return func(_ http.ResponseWriter, r *http.Request) (any, error) {
		who := httpx.Who(r)
		ctx := r.Context()
		id := r.PathValue("id")
		if !on {
			_, err := s.DB.Exec(ctx, `DELETE FROM post_saves WHERE user_id = $1 AND post_id = $2`, who.UserID, id)
			return nil, err
		}
		if _, err := s.visiblePost(ctx, who, id); err != nil {
			return nil, err
		}
		_, err := s.DB.Exec(ctx, `INSERT INTO post_saves (user_id, post_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, who.UserID, id)
		return nil, err
	}
}

/* ---------- comments ---------- */

func (s *Service) listComments(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	post, err := s.visiblePost(ctx, who, r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT c.id, c.text, c.created_at, c.user_id, u.handle, u.name, u.avatar
		FROM comments c JOIN users u ON u.id = c.user_id
		WHERE c.post_id = $2 AND `+views.NotBlocked("c.user_id")+` ORDER BY c.created_at LIMIT 200`, who.UserID, post.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := s.Now()
	out := []domain.Comment{}
	for rows.Next() {
		var c domain.Comment
		var at time.Time
		var userID string
		if err := rows.Scan(&c.ID, &c.Text, &at, &userID, &c.Author.Handle, &c.Author.Name, &c.Author.Avatar); err != nil {
			return nil, err
		}
		c.Author.Kind = "user"
		c.Author.Name = c.Author.Handle
		c.Time = domain.Ago(at, now)
		c.CanDelete = userID == who.UserID || post.Mine
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Service) addComment(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	post, err := s.visiblePost(ctx, who, r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	var in struct {
		Text string `json:"text"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return nil, err
	}
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return nil, httpx.BadRequest("Write something first")
	}
	if len([]rune(text)) > 500 {
		return nil, httpx.BadRequest("Keep comments under 500 characters")
	}
	if err := httpx.Limit(ctx, s.DB, "comment:"+who.UserID, 30, time.Minute, "You’re commenting too quickly"); err != nil {
		return nil, err
	}
	id := secure.ID("c")
	var avatar *domain.Photo
	err = db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO comments (id, post_id, user_id, text, created_at) VALUES ($1,$2,$3,$4,$5)`, id, post.ID, who.UserID, text, s.Now()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE posts SET comments_count = comments_count + 1 WHERE id = $1`, post.ID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT avatar FROM users WHERE id = $1`, who.UserID).Scan(&avatar); err != nil {
			return err
		}
		if post.Mine {
			return nil
		}
		note := inbox.Note{Who: who.Handle, Text: fmt.Sprintf("commented: “%s”", truncate(text, 80)), Kind: "Social", Avatar: avatar, Thumb: post.Image, Href: "/post/" + post.ID}
		var brandID, userID *string
		if err := tx.QueryRow(ctx, `SELECT brand_id, user_id FROM posts WHERE id = $1`, post.ID).Scan(&brandID, &userID); err != nil {
			return err
		}
		if brandID != nil {
			note.BrandID = *brandID
		} else {
			note.UserID = *userID
		}
		return inbox.Notify(ctx, tx, note)
	})
	if err != nil {
		return nil, err
	}
	return domain.Comment{ID: id, Text: text, Time: "now", CanDelete: true,
		Author: domain.Author{Kind: "user", Handle: who.Handle, Name: who.Handle, Avatar: avatar}}, nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func (s *Service) deleteComment(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	return nil, db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		var postID string
		// the comment's author, or whoever owns the post it's on
		err := tx.QueryRow(ctx, `DELETE FROM comments c USING posts p WHERE c.id = $1 AND p.id = c.post_id
			AND (c.user_id = $2 OR p.user_id = $2 OR ($3 <> '' AND p.brand_id = $3)) RETURNING c.post_id`,
			r.PathValue("id"), who.UserID, contentBrand(who)).Scan(&postID)
		if db.IsNotFound(err) {
			return httpx.NotFound("Comment not found")
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE posts SET comments_count = GREATEST(comments_count - 1, 0) WHERE id = $1`, postID)
		return err
	})
}

/* ---------- my grids ---------- */

func tiles(rows pgx.Rows, now time.Time, badgeNew bool) ([]domain.GridTile, error) {
	defer rows.Close()
	out := []domain.GridTile{}
	for rows.Next() {
		var t domain.GridTile
		var at *time.Time
		if err := rows.Scan(&t.ID, &t.Image, &at); err != nil {
			return nil, err
		}
		if badgeNew && at != nil && now.Sub(*at) < 24*time.Hour {
			t.Badge = "NEW"
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Service) myWears(_ http.ResponseWriter, r *http.Request) (any, error) {
	rows, err := s.DB.Query(r.Context(), `SELECT id, media -> 0, published_at FROM posts WHERE user_id = $1 AND status = 'published' ORDER BY published_at DESC LIMIT 200`, httpx.Who(r).UserID)
	if err != nil {
		return nil, err
	}
	return tiles(rows, s.Now(), true)
}

func (s *Service) mySaved(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	rows, err := s.DB.Query(r.Context(), `SELECT p.id, p.media -> 0, p.published_at`+views.PostFrom+`
		JOIN post_saves v ON v.post_id = p.id AND v.user_id = $1
		WHERE `+views.PostVisible+` ORDER BY v.created_at DESC LIMIT 200`, who.UserID)
	if err != nil {
		return nil, err
	}
	return tiles(rows, s.Now(), false)
}

func (s *Service) myCloset(_ http.ResponseWriter, r *http.Request) (any, error) {
	rows, err := views.ProductsWhere(r.Context(), s.DB, `JOIN closet_items c ON c.product_id = p.id WHERE c.user_id = $1 ORDER BY c.added_at DESC, p.id`, httpx.Who(r).UserID)
	if err != nil {
		return nil, err
	}
	out := make([]domain.ProductSummary, 0, len(rows))
	for _, p := range rows {
		out = append(out, p.Summary)
	}
	return out, nil
}

/* ---------- posting a wear ---------- */

var fitOptions = map[string]bool{"Runs small": true, "True to size": true, "Runs large": true}

type wearInput struct {
	ImageURL  string   `json:"imageUrl"`
	ProductID string   `json:"productId"`
	Size      string   `json:"size"`
	Fit       string   `json:"fit"`
	Caption   string   `json:"caption"`
	Hashtags  []string `json:"hashtags"`
	Location  string   `json:"location"`
}

func (s *Service) createWear(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	var in wearInput
	if err := httpx.Decode(r, &in); err != nil {
		return nil, err
	}
	if err := httpx.Limit(ctx, s.DB, "wear:"+who.UserID, 30, time.Hour, "You’re posting too quickly — try again later"); err != nil {
		return nil, err
	}
	photo, err := media.Photo(ctx, s.DB, who, in.ImageURL)
	if err != nil {
		return nil, err
	}
	product, err := views.ProductByID(ctx, s.DB, in.ProductID)
	if db.IsNotFound(err) {
		return nil, httpx.BadRequest("Tag the piece you’re wearing")
	}
	if err != nil {
		return nil, err
	}
	if !fitOptions[in.Fit] {
		return nil, httpx.BadRequest("Say how it fits")
	}
	caption := strings.TrimSpace(in.Caption)
	location := strings.TrimSpace(in.Location)
	if len([]rune(caption)) > 2200 {
		return nil, httpx.BadRequest("That caption is too long")
	}
	if len([]rune(location)) > 80 {
		return nil, httpx.BadRequest("That location is too long")
	}
	tags := []string{}
	for _, h := range in.Hashtags {
		if !domain.ValidHashtag(h) {
			return nil, httpx.BadRequest("Hashtags use letters, numbers and underscores")
		}
		tags = append(tags, h)
	}
	if len(tags) > 12 {
		return nil, httpx.BadRequest("Use 12 hashtags or fewer")
	}

	aspect := 390.0 / 480.0
	if photo.Aspect > 0 {
		aspect = clampAspect(photo.Aspect)
	}
	id := secure.ID("wr")
	brand := product.Summary.Brand
	subtitle := "Wearing " + brand.Name
	now := s.Now()
	var reward string
	err = db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO posts (id, user_id, type, status, media, aspect, subtitle, caption, location, worn_product_id, worn_size, worn_fit, created_at, published_at)
			VALUES ($1,$2,'Wear','published',$3,$4,$5,$6,$7,$8,$9,$10,$11,$11)`,
			id, who.UserID, []domain.Photo{*photo}, aspect, subtitle, caption, location, product.Summary.ID, in.Size, in.Fit, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO post_tags (post_id, position, product_id, label, x, y) VALUES ($1,0,$2,$3,18,62)`, id, product.Summary.ID, product.Summary.Name); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO post_shop (post_id, position, product_id) VALUES ($1,0,$2)`, id, product.Summary.ID); err != nil {
			return err
		}
		for _, t := range tags {
			if _, err := tx.Exec(ctx, `INSERT INTO post_hashtags (post_id, tag) VALUES ($1,$2) ON CONFLICT DO NOTHING`, id, t); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO closet_items (user_id, product_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, who.UserID, product.Summary.ID); err != nil {
			return err
		}
		var avatar *domain.Photo
		if err := tx.QueryRow(ctx, `SELECT avatar FROM users WHERE id = $1`, who.UserID).Scan(&avatar); err != nil {
			return err
		}
		if err := inbox.Notify(ctx, tx, inbox.Note{
			BrandID: brand.ID, Who: who.Handle, Text: "posted a wear of your " + product.Summary.Name + ".", Kind: "Social",
			Avatar: avatar, Thumb: photo, Href: "/post/" + id, Pref: "brandWears",
			Action: &domain.NotificationAction{Type: "amplify", Label: "AMPLIFY", DoneLabel: "AMPLIFIED", Target: id},
		}); err != nil {
			return err
		}
		reward, err = s.issueWearReward(ctx, tx, who.UserID, product.Summary.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "image": photo, "badge": "NEW", "rewardCode": reward}, nil
}

func clampAspect(a float64) float64 {
	switch {
	case a < 0.6:
		return 0.6
	case a > 1.91:
		return 1.91
	}
	return a
}

// issueWearReward keeps the promise on the order confirmation screen: the first wear of a
// delivered order earns a single-use 10% code, funded by wearwhat rather than the brand.
func (s *Service) issueWearReward(ctx context.Context, tx pgx.Tx, userID, productID string) (string, error) {
	var seq int64
	err := tx.QueryRow(ctx, `SELECT o.seq FROM orders o LEFT JOIN wear_rewards w ON w.order_seq = o.seq
		WHERE o.user_id = $1 AND o.product_id = $2 AND o.delivered_at IS NOT NULL AND w.order_seq IS NULL
		ORDER BY o.delivered_at DESC LIMIT 1`, userID, productID).Scan(&seq)
	if db.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	code := "WEAR10-" + strings.ToUpper(secure.ID("x")[2:8])
	ends := s.Now().Add(30 * 24 * time.Hour)
	if _, err := tx.Exec(ctx, `INSERT INTO promotions (code, user_id, type, value, scope, length, ends_at, max_uses, live)
		VALUES ($1,$2,'% off','10','Everything','30 days',$3,1,true)`, code, userID, ends); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO wear_rewards (order_seq, code) VALUES ($1,$2)`, seq, code); err != nil {
		return "", err
	}
	return code, inbox.Notify(ctx, tx, inbox.Note{UserID: userID, Who: "wearwhat", Text: "Thanks for posting your wear. Here’s 10% off your next order: " + code, Kind: "Orders", Href: "/bag"})
}

/* ---------- reports ---------- */

func (s *Service) report(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	var in struct {
		Kind   string `json:"kind"`
		ID     string `json:"id"`
		Reason string `json:"reason"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return nil, err
	}
	switch in.Kind {
	case "post", "comment", "user", "brand", "thread":
	default:
		return nil, httpx.BadRequest("Choose what to report")
	}
	if in.ID == "" {
		return nil, httpx.BadRequest("Choose what to report")
	}
	if err := httpx.Limit(ctx, s.DB, "report:"+who.UserID, 30, time.Hour, "Too many reports — try again later"); err != nil {
		return nil, err
	}
	_, err := s.DB.Exec(ctx, `INSERT INTO reports (id, reporter_id, target_kind, target_id, reason) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (reporter_id, target_kind, target_id) DO UPDATE SET reason = EXCLUDED.reason`,
		secure.ID("r"), who.UserID, in.Kind, in.ID, truncate(strings.TrimSpace(in.Reason), 500))
	return nil, err
}

/* ---------- analytics events ---------- */

// event records a view or try-on for the brand's dashboard. Repeats within half an hour count once.
func (s *Service) event(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	var in struct {
		Type      string  `json:"type"`
		ProductID *string `json:"productId"`
		PostID    *string `json:"postId"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return nil, err
	}
	var brandID *string
	var err error
	switch {
	case in.Type == "product_view" || in.Type == "try_on":
		if in.ProductID == nil {
			return nil, httpx.BadRequest("Missing product")
		}
		err = s.DB.QueryRow(ctx, `SELECT brand_id FROM products WHERE id = $1`, *in.ProductID).Scan(&brandID)
		in.PostID = nil
	case in.Type == "post_view":
		if in.PostID == nil {
			return nil, httpx.BadRequest("Missing post")
		}
		// a wear's view counts for the brand of the piece it features
		err = s.DB.QueryRow(ctx, `SELECT COALESCE(p.brand_id, x.brand_id) FROM posts p LEFT JOIN products x ON x.id = p.worn_product_id WHERE p.id = $1`, *in.PostID).Scan(&brandID)
		in.ProductID = nil
	default:
		return nil, httpx.BadRequest("Unknown event")
	}
	if db.IsNotFound(err) {
		return nil, nil // the thing was removed; analytics aren't worth an error
	}
	if err != nil {
		return nil, err
	}
	_, err = s.DB.Exec(ctx, `INSERT INTO analytics_events (type, brand_id, product_id, post_id, user_id, at)
		SELECT $1,$2,$3,$4,$5,$6 WHERE NOT EXISTS (
			SELECT 1 FROM analytics_events a WHERE a.user_id = $5 AND a.type = $1
			AND a.product_id IS NOT DISTINCT FROM $3 AND a.post_id IS NOT DISTINCT FROM $4
			AND a.at > $6::timestamptz - interval '30 minutes')`,
		in.Type, brandID, in.ProductID, in.PostID, who.UserID, s.Now())
	return nil, err
}
