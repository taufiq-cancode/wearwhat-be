// Package account holds profiles, addresses, payment methods, follows and blocks.
package account

import (
	"context"
	"net/http"
	"strings"

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
	r.User("GET /me", s.getMe)
	r.User("PATCH /me", s.patchMe)

	r.User("GET /me/addresses", s.listAddresses)
	r.User("POST /me/addresses", s.createAddress)
	r.User("PATCH /me/addresses/{id}", s.updateAddress)
	r.User("DELETE /me/addresses/{id}", s.deleteAddress)

	r.User("GET /me/payment-methods", s.listPaymentMethods)
	r.User("POST /me/payment-methods/{id}/default", s.defaultPaymentMethod)
	r.User("DELETE /me/payment-methods/{id}", s.deletePaymentMethod)

	r.User("GET /me/blocks", s.listBlocks)
	r.User("GET /users/{handle}", s.getUser)
	r.User("PUT /users/{handle}/follow", s.follow(true))
	r.User("DELETE /users/{handle}/follow", s.follow(false))
	r.User("PUT /users/{handle}/block", s.block(true))
	r.User("DELETE /users/{handle}/block", s.block(false))
}

/* ---------- me ---------- */

// Me builds the signed-in account.
func (s *Service) Me(ctx context.Context, userID string) (domain.Me, error) {
	var m domain.Me
	err := s.DB.QueryRow(ctx, `SELECT u.id, u.handle, u.name, COALESCE(u.email, ''), COALESCE(u.phone, ''), u.bio, u.avatar,
			u.followers, u.following, u.prefs, u.fit, u.styles, u.shop_for,
			(SELECT count(*) FROM posts p WHERE p.user_id = u.id AND p.status = 'published'),
			(SELECT count(*) FROM post_saves ps WHERE ps.user_id = u.id),
			(SELECT count(*) FROM orders o WHERE o.user_id = u.id AND `+views.PaidOrder+`),
			COALESCE((SELECT pm.label || CASE WHEN pm.detail <> '' THEN ' · ' || pm.detail ELSE '' END FROM payment_methods pm WHERE pm.user_id = u.id AND pm.is_default), ''),
			u.is_admin
		FROM users u WHERE u.id = $1`, userID).Scan(
		&m.ID, &m.Handle, &m.Name, &m.Email, &m.Phone, &m.Bio, &m.Avatar,
		&m.Followers, &m.Following, &m.Prefs, &m.Fit, &m.Styles, &m.ShopFor,
		&m.WearsCount, &m.SavedCount, &m.OrdersCount, &m.PaymentLabel, &m.Admin)
	if err != nil {
		return m, err
	}
	if m.Styles == nil {
		m.Styles = []string{}
	}

	var b domain.MeBrand
	err = s.DB.QueryRow(ctx, `SELECT b.id, b.handle, b.name, b.initials, t.role, b.status
		FROM team_members t JOIN brands b ON b.id = t.brand_id WHERE t.user_id = $1 AND t.status = 'active'`, userID).
		Scan(&b.ID, &b.Handle, &b.Name, &b.Initials, &b.Role, &b.Status)
	switch {
	case err == nil:
		m.Brand = &b
	case !db.IsNotFound(err):
		return m, err
	}

	var a domain.Address
	err = s.DB.QueryRow(ctx, `SELECT id, name, line1, city, phone, is_default FROM addresses WHERE user_id = $1 AND is_default`, userID).
		Scan(&a.ID, &a.Name, &a.Line1, &a.City, &a.Phone, &a.IsDefault)
	switch {
	case err == nil:
		m.Address = &a
	case !db.IsNotFound(err):
		return m, err
	}
	return m, nil
}

func (s *Service) getMe(_ http.ResponseWriter, r *http.Request) (any, error) {
	return s.Me(r.Context(), httpx.Who(r).UserID)
}

type profileInput struct {
	Name      *string            `json:"name"`
	Handle    *string            `json:"handle"`
	Bio       *string            `json:"bio"`
	AvatarURL *string            `json:"avatarUrl"`
	ShopFor   *string            `json:"shopFor"`
	Styles    *[]string          `json:"styles"`
	Fit       *domain.FitProfile `json:"fit"`
	Follows   *[]string          `json:"follows"`
	Consents  map[string]bool    `json:"consents"`
	Prefs     map[string]bool    `json:"prefs"`
}

var prefKeys = map[string]bool{"drops": true, "orders": true, "digest": true, "private": true, "brandOrders": true, "brandReturns": true, "brandWears": true}

func (s *Service) patchMe(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	var in profileInput
	if err := httpx.Decode(r, &in); err != nil {
		return nil, err
	}
	err := db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		if in.Name != nil {
			name := strings.TrimSpace(*in.Name)
			if name == "" || len([]rune(name)) > 80 {
				return httpx.BadRequest("Add your name")
			}
			if _, err := tx.Exec(ctx, `UPDATE users SET name = $2 WHERE id = $1`, who.UserID, name); err != nil {
				return err
			}
		}
		if in.Handle != nil {
			h := domain.NormalizeHandle(*in.Handle)
			if !domain.ValidHandle(h) {
				return httpx.BadRequest("Usernames use 3–30 letters, numbers, dots or underscores")
			}
			if _, err := tx.Exec(ctx, `UPDATE users SET handle = $2 WHERE id = $1`, who.UserID, h); err != nil {
				if db.IsUnique(err) {
					return httpx.Conflict("@" + h + " is taken")
				}
				return err
			}
		}
		if in.Bio != nil {
			if len([]rune(*in.Bio)) > 300 {
				return httpx.BadRequest("Keep your bio under 300 characters")
			}
			if _, err := tx.Exec(ctx, `UPDATE users SET bio = $2 WHERE id = $1`, who.UserID, strings.TrimSpace(*in.Bio)); err != nil {
				return err
			}
		}
		if in.AvatarURL != nil {
			photo, err := media.OptionalPhoto(ctx, tx, who, in.AvatarURL)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE users SET avatar = $2 WHERE id = $1`, who.UserID, photo); err != nil {
				return err
			}
		}
		if in.ShopFor != nil {
			if _, err := tx.Exec(ctx, `UPDATE users SET shop_for = $2 WHERE id = $1`, who.UserID, *in.ShopFor); err != nil {
				return err
			}
		}
		if in.Styles != nil {
			if len(*in.Styles) > 30 {
				return httpx.BadRequest("Too many styles")
			}
			if _, err := tx.Exec(ctx, `UPDATE users SET styles = $2 WHERE id = $1`, who.UserID, *in.Styles); err != nil {
				return err
			}
		}
		if in.Fit != nil {
			if _, err := tx.Exec(ctx, `UPDATE users SET fit = $2 WHERE id = $1`, who.UserID, in.Fit); err != nil {
				return err
			}
		}
		if in.Consents != nil {
			if _, err := tx.Exec(ctx, `UPDATE users SET consents = consents || $2 WHERE id = $1`, who.UserID, in.Consents); err != nil {
				return err
			}
		}
		if in.Prefs != nil {
			clean := map[string]bool{}
			for k, v := range in.Prefs {
				if prefKeys[k] {
					clean[k] = v
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE users SET prefs = prefs || $2 WHERE id = $1`, who.UserID, clean); err != nil {
				return err
			}
			// going public: anyone waiting on approval becomes a follower
			if priv, ok := clean["private"]; ok && !priv {
				if err := approveAllRequests(ctx, tx, who.UserID); err != nil {
					return err
				}
			}
		}
		if in.Follows != nil {
			return s.replaceBrandFollows(ctx, tx, who.UserID, *in.Follows)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.Me(ctx, who.UserID)
}

func approveAllRequests(ctx context.Context, q db.Q, userID string) error {
	_, err := q.Exec(ctx, `WITH approved AS (
			UPDATE follows_user SET approved = true WHERE followee_id = $1 AND NOT approved RETURNING follower_id
		), followers_side AS (
			UPDATE users SET following = following + 1 WHERE id IN (SELECT follower_id FROM approved)
		)
		UPDATE users SET followers = followers + (SELECT count(*) FROM approved) WHERE id = $1`, userID)
	return err
}

// replaceBrandFollows makes the follow set exactly ids, adjusting counters for what changed.
func (s *Service) replaceBrandFollows(ctx context.Context, q db.Q, userID string, ids []string) error {
	rows, err := q.Query(ctx, `SELECT brand_id FROM follows_brand WHERE user_id = $1`, userID)
	if err != nil {
		return err
	}
	current, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
		if err := SetBrandFollow(ctx, q, userID, id, true); err != nil {
			return err
		}
	}
	for _, id := range current {
		if !want[id] {
			if err := SetBrandFollow(ctx, q, userID, id, false); err != nil {
				return err
			}
		}
	}
	return nil
}

// SetBrandFollow follows or unfollows a brand and keeps both counters correct.
func SetBrandFollow(ctx context.Context, q db.Q, userID, brandID string, follow bool) error {
	if follow {
		var ok bool
		if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM brands WHERE id = $1)`, brandID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return httpx.NotFound("Brand not found")
		}
		tag, err := q.Exec(ctx, `INSERT INTO follows_brand (user_id, brand_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, userID, brandID)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		if _, err := q.Exec(ctx, `UPDATE brands SET followers = followers + 1 WHERE id = $1`, brandID); err != nil {
			return err
		}
		_, err = q.Exec(ctx, `UPDATE users SET following = following + 1 WHERE id = $1`, userID)
		return err
	}
	tag, err := q.Exec(ctx, `DELETE FROM follows_brand WHERE user_id = $1 AND brand_id = $2`, userID, brandID)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	if _, err := q.Exec(ctx, `UPDATE brands SET followers = GREATEST(followers - 1, 0) WHERE id = $1`, brandID); err != nil {
		return err
	}
	_, err = q.Exec(ctx, `UPDATE users SET following = GREATEST(following - 1, 0) WHERE id = $1`, userID)
	return err
}

/* ---------- people ---------- */

// userByHandle finds a verified account the viewer is allowed to know about.
func (s *Service) userByHandle(ctx context.Context, viewerID, handle string) (id string, err error) {
	err = s.DB.QueryRow(ctx, `SELECT u.id FROM users u WHERE u.handle = $2 AND u.verified_at IS NOT NULL AND `+views.NotBlocked("u.id"),
		viewerID, domain.NormalizeHandle(handle)).Scan(&id)
	if db.IsNotFound(err) {
		return "", httpx.NotFound("Account not found")
	}
	return id, err
}

func (s *Service) getUser(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	id, err := s.userByHandle(ctx, who.UserID, r.PathValue("handle"))
	if err != nil {
		return nil, err
	}
	var p domain.UserProfile
	var private bool
	err = s.DB.QueryRow(ctx, `SELECT u.id, u.handle, u.name, u.bio, u.avatar, u.followers, u.following,
			COALESCE((u.prefs ->> 'private')::boolean, false),
			(SELECT count(*) FROM posts x WHERE x.user_id = u.id AND x.status = 'published'),
			EXISTS (SELECT 1 FROM follows_user f WHERE f.follower_id = $2 AND f.followee_id = u.id AND f.approved),
			EXISTS (SELECT 1 FROM follows_user f WHERE f.follower_id = $2 AND f.followee_id = u.id AND NOT f.approved),
			EXISTS (SELECT 1 FROM follows_user f WHERE f.follower_id = u.id AND f.followee_id = $2 AND f.approved)
		FROM users u WHERE u.id = $1`, id, who.UserID).Scan(
		&p.ID, &p.Handle, &p.Name, &p.Bio, &p.Avatar, &p.Followers, &p.Following, &private,
		&p.WearsCount, &p.IsFollowed, &p.Requested, &p.FollowsYou)
	if err != nil {
		return nil, err
	}
	p.IsMe = id == who.UserID
	p.Locked = private && !p.IsFollowed && !p.IsMe
	p.Wears = []domain.GridTile{}
	if !p.Locked {
		rows, err := s.DB.Query(ctx, `SELECT id, media -> 0 FROM posts WHERE user_id = $1 AND status = 'published' ORDER BY published_at DESC LIMIT 60`, id)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var t domain.GridTile
			if err := rows.Scan(&t.ID, &t.Image); err != nil {
				return nil, err
			}
			p.Wears = append(p.Wears, t)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func (s *Service) follow(on bool) httpx.Handler {
	return func(_ http.ResponseWriter, r *http.Request) (any, error) {
		who := httpx.Who(r)
		ctx := r.Context()
		id, err := s.userByHandle(ctx, who.UserID, r.PathValue("handle"))
		if err != nil {
			return nil, err
		}
		if id == who.UserID {
			return nil, httpx.BadRequest("You can’t follow yourself")
		}
		var state string
		err = db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
			var e error
			state, e = SetUserFollow(ctx, tx, who, id, on)
			return e
		})
		if err != nil {
			return nil, err
		}
		return map[string]string{"state": state}, nil
	}
}

// SetUserFollow follows or unfollows a person. Private accounts receive a request instead,
// and only count as a follower once approved. It returns following | requested | none.
func SetUserFollow(ctx context.Context, q db.Q, who *httpx.Principal, targetID string, on bool) (string, error) {
	if !on {
		var approved bool
		err := q.QueryRow(ctx, `DELETE FROM follows_user WHERE follower_id = $1 AND followee_id = $2 RETURNING approved`, who.UserID, targetID).Scan(&approved)
		if db.IsNotFound(err) {
			return "none", nil
		}
		if err != nil {
			return "", err
		}
		if approved {
			if err := bumpFollowCounts(ctx, q, who.UserID, targetID, -1); err != nil {
				return "", err
			}
		}
		return "none", nil
	}

	var private bool
	var avatar *domain.Photo
	if err := q.QueryRow(ctx, `SELECT COALESCE((prefs ->> 'private')::boolean, false) FROM users WHERE id = $1`, targetID).Scan(&private); err != nil {
		return "", err
	}
	if err := q.QueryRow(ctx, `SELECT avatar FROM users WHERE id = $1`, who.UserID).Scan(&avatar); err != nil {
		return "", err
	}
	approved := !private
	tag, err := q.Exec(ctx, `INSERT INTO follows_user (follower_id, followee_id, approved) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, who.UserID, targetID, approved)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		// already following or already requested: report the existing state
		if err := q.QueryRow(ctx, `SELECT approved FROM follows_user WHERE follower_id = $1 AND followee_id = $2`, who.UserID, targetID).Scan(&approved); err != nil {
			return "", err
		}
		return followState(approved), nil
	}
	if approved {
		if err := bumpFollowCounts(ctx, q, who.UserID, targetID, 1); err != nil {
			return "", err
		}
	}
	note := inbox.Note{UserID: targetID, Who: who.Handle, Kind: "Social", Avatar: avatar, Href: "/u/" + who.Handle}
	if approved {
		note.Text = "started following you."
		var followingBack bool
		if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM follows_user WHERE follower_id = $1 AND followee_id = $2)`, targetID, who.UserID).Scan(&followingBack); err != nil {
			return "", err
		}
		note.Action = &domain.NotificationAction{Type: "follow", Label: "FOLLOW", DoneLabel: "FOLLOWING", Done: followingBack, Target: who.Handle}
	} else {
		note.Text = "wants to follow you."
		note.Action = &domain.NotificationAction{Type: "approve", Label: "APPROVE", DoneLabel: "APPROVED", Target: who.Handle}
	}
	if err := inbox.Notify(ctx, q, note); err != nil {
		return "", err
	}
	return followState(approved), nil
}

func followState(approved bool) string {
	if approved {
		return "following"
	}
	return "requested"
}

func bumpFollowCounts(ctx context.Context, q db.Q, follower, followee string, by int) error {
	if _, err := q.Exec(ctx, `UPDATE users SET following = GREATEST(following + $2, 0) WHERE id = $1`, follower, by); err != nil {
		return err
	}
	_, err := q.Exec(ctx, `UPDATE users SET followers = GREATEST(followers + $2, 0) WHERE id = $1`, followee, by)
	return err
}

// FollowAction is the notification button for following someone back.
func (s *Service) FollowAction(ctx context.Context, who *httpx.Principal, a *domain.NotificationAction, done bool) (any, error) {
	id, err := s.userByHandle(ctx, who.UserID, a.Target)
	if err != nil {
		return nil, err
	}
	err = db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		_, e := SetUserFollow(ctx, tx, who, id, done)
		return e
	})
	return nil, err
}

// ApproveAction is the notification button on a private account's follow request.
func (s *Service) ApproveAction(ctx context.Context, who *httpx.Principal, a *domain.NotificationAction, done bool) (any, error) {
	id, err := s.userByHandle(ctx, who.UserID, a.Target)
	if err != nil {
		return nil, err
	}
	err = db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		if done {
			tag, err := tx.Exec(ctx, `UPDATE follows_user SET approved = true WHERE follower_id = $1 AND followee_id = $2 AND NOT approved`, id, who.UserID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return httpx.NotFound("That request has been withdrawn")
			}
			return bumpFollowCounts(ctx, tx, id, who.UserID, 1)
		}
		// un-approving removes the follower again
		var approved bool
		err := tx.QueryRow(ctx, `DELETE FROM follows_user WHERE follower_id = $1 AND followee_id = $2 RETURNING approved`, id, who.UserID).Scan(&approved)
		if db.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if approved {
			return bumpFollowCounts(ctx, tx, id, who.UserID, -1)
		}
		return nil
	})
	return nil, err
}

/* ---------- blocks ---------- */

func (s *Service) listBlocks(_ http.ResponseWriter, r *http.Request) (any, error) {
	rows, err := s.DB.Query(r.Context(), `SELECT u.id, u.handle, u.name, u.avatar FROM blocks b JOIN users u ON u.id = b.blocked_id
		WHERE b.blocker_id = $1 ORDER BY b.created_at DESC`, httpx.Who(r).UserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.BlockedUser{}
	for rows.Next() {
		var b domain.BlockedUser
		if err := rows.Scan(&b.ID, &b.Handle, &b.Name, &b.Avatar); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Service) block(on bool) httpx.Handler {
	return func(_ http.ResponseWriter, r *http.Request) (any, error) {
		who := httpx.Who(r)
		ctx := r.Context()
		// unblocking must work even though the blocked account is hidden from normal lookups
		var id string
		err := s.DB.QueryRow(ctx, `SELECT id FROM users WHERE handle = $1`, domain.NormalizeHandle(r.PathValue("handle"))).Scan(&id)
		if db.IsNotFound(err) {
			return nil, httpx.NotFound("Account not found")
		}
		if err != nil {
			return nil, err
		}
		if id == who.UserID {
			return nil, httpx.BadRequest("You can’t block yourself")
		}
		err = db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
			if !on {
				_, err := tx.Exec(ctx, `DELETE FROM blocks WHERE blocker_id = $1 AND blocked_id = $2`, who.UserID, id)
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO blocks (blocker_id, blocked_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, who.UserID, id); err != nil {
				return err
			}
			// a block also ends any follow in either direction
			if _, err := SetUserFollow(ctx, tx, who, id, false); err != nil {
				return err
			}
			other := &httpx.Principal{UserID: id}
			_, err := SetUserFollow(ctx, tx, other, who.UserID, false)
			return err
		})
		return nil, err
	}
}

/* ---------- addresses ---------- */

type addressInput struct {
	Name      string `json:"name"`
	Line1     string `json:"line1"`
	City      string `json:"city"`
	Phone     string `json:"phone"`
	IsDefault bool   `json:"isDefault"`
}

func (in *addressInput) clean() error {
	in.Name, in.Line1, in.City, in.Phone = strings.TrimSpace(in.Name), strings.TrimSpace(in.Line1), strings.TrimSpace(in.City), strings.TrimSpace(in.Phone)
	switch {
	case in.Name == "":
		return httpx.BadRequest("Add who it’s for")
	case in.Line1 == "":
		return httpx.BadRequest("Add the street address")
	case in.City == "":
		return httpx.BadRequest("Add the city")
	case len(in.Name) > 120 || len(in.Line1) > 300 || len(in.City) > 120 || len(in.Phone) > 40:
		return httpx.BadRequest("That address is too long")
	}
	return nil
}

func (s *Service) listAddresses(_ http.ResponseWriter, r *http.Request) (any, error) {
	rows, err := s.DB.Query(r.Context(), `SELECT id, name, line1, city, phone, is_default FROM addresses WHERE user_id = $1 ORDER BY is_default DESC, created_at DESC`, httpx.Who(r).UserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Address{}
	for rows.Next() {
		var a domain.Address
		if err := rows.Scan(&a.ID, &a.Name, &a.Line1, &a.City, &a.Phone, &a.IsDefault); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Service) createAddress(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	var in addressInput
	if err := httpx.Decode(r, &in); err != nil {
		return nil, err
	}
	if err := in.clean(); err != nil {
		return nil, err
	}
	a := domain.Address{ID: secure.ID("adr"), Name: in.Name, Line1: in.Line1, City: in.City, Phone: in.Phone}
	err := db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM addresses WHERE user_id = $1`, who.UserID).Scan(&count); err != nil {
			return err
		}
		if count >= 20 {
			return httpx.BadRequest("You’ve saved the maximum number of addresses")
		}
		a.IsDefault = in.IsDefault || count == 0
		if a.IsDefault {
			if _, err := tx.Exec(ctx, `UPDATE addresses SET is_default = false WHERE user_id = $1 AND is_default`, who.UserID); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO addresses (id, user_id, name, line1, city, phone, is_default) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			a.ID, who.UserID, a.Name, a.Line1, a.City, a.Phone, a.IsDefault)
		return err
	})
	if err != nil {
		return nil, err
	}
	return a, nil
}

func (s *Service) updateAddress(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	var in addressInput
	if err := httpx.Decode(r, &in); err != nil {
		return nil, err
	}
	if err := in.clean(); err != nil {
		return nil, err
	}
	a := domain.Address{ID: r.PathValue("id"), Name: in.Name, Line1: in.Line1, City: in.City, Phone: in.Phone}
	err := db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		if in.IsDefault {
			if _, err := tx.Exec(ctx, `UPDATE addresses SET is_default = false WHERE user_id = $1 AND is_default AND id <> $2`, who.UserID, a.ID); err != nil {
				return err
			}
		}
		err := tx.QueryRow(ctx, `UPDATE addresses SET name = $3, line1 = $4, city = $5, phone = $6, is_default = is_default OR $7
			WHERE id = $1 AND user_id = $2 RETURNING is_default`, a.ID, who.UserID, a.Name, a.Line1, a.City, a.Phone, in.IsDefault).Scan(&a.IsDefault)
		if db.IsNotFound(err) {
			return httpx.NotFound("Address not found")
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return a, nil
}

func (s *Service) deleteAddress(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	err := db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		var wasDefault bool
		err := tx.QueryRow(ctx, `DELETE FROM addresses WHERE id = $1 AND user_id = $2 RETURNING is_default`, r.PathValue("id"), who.UserID).Scan(&wasDefault)
		if db.IsNotFound(err) {
			return httpx.NotFound("Address not found")
		}
		if err != nil {
			return err
		}
		if wasDefault {
			_, err = tx.Exec(ctx, `UPDATE addresses SET is_default = true WHERE id = (SELECT id FROM addresses WHERE user_id = $1 ORDER BY created_at DESC LIMIT 1)`, who.UserID)
		}
		return err
	})
	return nil, err
}

/* ---------- payment methods ---------- */

func (s *Service) listPaymentMethods(_ http.ResponseWriter, r *http.Request) (any, error) {
	rows, err := s.DB.Query(r.Context(), `SELECT id, label, detail, is_default FROM payment_methods WHERE user_id = $1 AND provider = $2 ORDER BY is_default DESC, created_at DESC`,
		httpx.Who(r).UserID, s.Payments.Name())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.PaymentMethod{}
	for rows.Next() {
		var m domain.PaymentMethod
		if err := rows.Scan(&m.ID, &m.Label, &m.Detail, &m.IsDefault); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Service) defaultPaymentMethod(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	err := db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE payment_methods SET is_default = false WHERE user_id = $1 AND is_default`, who.UserID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE payment_methods SET is_default = true WHERE id = $1 AND user_id = $2`, r.PathValue("id"), who.UserID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return httpx.NotFound("Payment method not found")
		}
		return nil
	})
	return nil, err
}

func (s *Service) deletePaymentMethod(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	err := db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		var wasDefault bool
		err := tx.QueryRow(ctx, `DELETE FROM payment_methods WHERE id = $1 AND user_id = $2 RETURNING is_default`, r.PathValue("id"), who.UserID).Scan(&wasDefault)
		if db.IsNotFound(err) {
			return httpx.NotFound("Payment method not found")
		}
		if err != nil {
			return err
		}
		if wasDefault {
			_, err = tx.Exec(ctx, `UPDATE payment_methods SET is_default = true WHERE id = (SELECT id FROM payment_methods WHERE user_id = $1 ORDER BY created_at DESC LIMIT 1)`, who.UserID)
		}
		return err
	})
	return nil, err
}
