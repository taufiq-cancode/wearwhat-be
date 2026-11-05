package inbox

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"wearwhat-be/internal/db"
	"wearwhat-be/internal/domain"
	"wearwhat-be/internal/httpx"
	"wearwhat-be/internal/media"
	"wearwhat-be/internal/secure"
	"wearwhat-be/internal/views"
)

const maxMessage = 2000

func (s *Service) threadRoutes(r *httpx.Router) {
	r.User("GET /threads", s.listThreads("shopper"))
	r.User("POST /threads", s.startShopperThread)
	r.User("GET /threads/{id}", s.getThread("shopper"))
	r.User("POST /threads/{id}/read", s.readThread("shopper"))
	r.User("POST /threads/{id}/messages", s.sendMessage("shopper"))

	r.Brand("GET /studio/threads", httpx.CapOrders, s.listThreads("studio"))
	r.Brand("POST /studio/threads", httpx.CapOrders, s.startStudioThread)
	r.Brand("GET /studio/threads/{id}", httpx.CapOrders, s.getThread("studio"))
	r.Brand("POST /studio/threads/{id}/read", httpx.CapOrders, s.readThread("studio"))
	r.Brand("POST /studio/threads/{id}/messages", httpx.CapOrders, s.sendMessage("studio"))
}

// threadRow is one conversation with both participants loaded.
type threadRow struct {
	ID, Kind, AUser       string
	BUser, BrandID, ProdID *string
	AUnread, BUnread      int
	BrandName, BrandLoc   *string
	BrandStatus           *string
	BrandAvatar           *domain.Photo
	AName, AHandle        string
	AAvatar               *domain.Photo
	BName, BHandle        *string
	BAvatar               *domain.Photo
	LastID                *string
	LastSide              *string
	LastText              *string
	LastImage             *domain.Photo
	LastAt                *time.Time
}

const threadSelect = `SELECT t.id, t.kind, t.a_user_id, t.b_user_id, t.brand_id, t.product_id, t.a_unread, t.b_unread,
	b.name, b.location, b.status, b.avatar, ua.name, ua.handle, ua.avatar, ub.name, ub.handle, ub.avatar,
	lm.id, lm.side, lm.text, lm.image, lm.created_at
	FROM threads t
	JOIN users ua ON ua.id = t.a_user_id
	LEFT JOIN users ub ON ub.id = t.b_user_id
	LEFT JOIN brands b ON b.id = t.brand_id
	LEFT JOIN LATERAL (SELECT m.id, m.side, m.text, m.image, m.created_at FROM messages m
		WHERE m.thread_id = t.id ORDER BY m.created_at DESC, m.id DESC LIMIT 1) lm ON true `

func scanThread(row pgx.Row) (threadRow, error) {
	var t threadRow
	err := row.Scan(&t.ID, &t.Kind, &t.AUser, &t.BUser, &t.BrandID, &t.ProdID, &t.AUnread, &t.BUnread,
		&t.BrandName, &t.BrandLoc, &t.BrandStatus, &t.BrandAvatar, &t.AName, &t.AHandle, &t.AAvatar, &t.BName, &t.BHandle, &t.BAvatar,
		&t.LastID, &t.LastSide, &t.LastText, &t.LastImage, &t.LastAt)
	return t, err
}

// side says which participant the caller is in this thread ("" when they aren't one).
func (t threadRow) side(scope string, who *httpx.Principal) string {
	switch {
	case scope == "studio":
		if t.BrandID != nil && *t.BrandID == who.BrandID {
			return "b"
		}
	case t.AUser == who.UserID:
		return "a"
	case t.BUser != nil && *t.BUser == who.UserID:
		return "b"
	}
	return ""
}

func (s *Service) summary(ctx context.Context, t threadRow, scope, side string) (domain.ThreadSummary, error) {
	now := s.Now()
	out := domain.ThreadSummary{ID: t.ID}
	switch {
	case scope == "studio":
		out.Kind, out.Name, out.Avatar, out.Handle = "customer", t.AName, t.AAvatar, t.AHandle
		var orders int
		var city *string
		err := s.DB.QueryRow(ctx, `SELECT (SELECT count(*) FROM orders o WHERE o.user_id = $1 AND o.brand_id = $2 AND `+views.PaidOrder+`),
			(SELECT a.city FROM addresses a WHERE a.user_id = $1 AND a.is_default)`, t.AUser, *t.BrandID).Scan(&orders, &city)
		if err != nil {
			return out, err
		}
		out.Status = customerStatus(orders, city)
	case t.Kind == "brand":
		out.Kind, out.Name, out.Avatar = "brand", *t.BrandName, t.BrandAvatar
		out.Verified = t.BrandStatus != nil && *t.BrandStatus == "verified"
		out.Status = "Brand"
		if t.BrandLoc != nil && *t.BrandLoc != "" {
			out.Status = "Brand · " + *t.BrandLoc
		}
	case side == "a":
		out.Kind, out.Name, out.Avatar, out.Handle = "person", *t.BHandle, t.BAvatar, *t.BHandle
		out.Status = "@" + *t.BHandle
	default:
		out.Kind, out.Name, out.Avatar, out.Handle = "person", t.AHandle, t.AAvatar, t.AHandle
		out.Status = "@" + t.AHandle
	}
	if side == "a" {
		out.Unread = t.AUnread
	} else {
		out.Unread = t.BUnread
	}
	product, err := views.SummaryPtr(ctx, s.DB, t.ProdID)
	if err != nil {
		return out, err
	}
	out.Product = product
	if t.LastID != nil {
		out.Last = &domain.Message{ID: *t.LastID, FromMe: *t.LastSide == side, Text: *t.LastText, Image: t.LastImage, Time: domain.ChatTime(*t.LastAt, now)}
	}
	return out, nil
}

func customerStatus(orders int, city *string) string {
	parts := []string{"Customer"}
	switch orders {
	case 0:
	case 1:
		parts = append(parts, "1 order")
	default:
		parts = append(parts, fmt.Sprintf("%d orders", orders))
	}
	if city != nil && *city != "" {
		parts = append(parts, *city)
	}
	return strings.Join(parts, " · ")
}

func (s *Service) listThreads(scope string) httpx.Handler {
	return func(_ http.ResponseWriter, r *http.Request) (any, error) {
		who := httpx.Who(r)
		var rows pgx.Rows
		var err error
		if scope == "studio" {
			// a brand sees a conversation once a message exists
			rows, err = s.DB.Query(r.Context(), threadSelect+`WHERE t.kind = 'brand' AND t.brand_id = $1 AND lm.id IS NOT NULL
				ORDER BY t.last_message_at DESC LIMIT 200`, who.BrandID)
		} else {
			// the other person only sees a thread once a message exists; the starter sees it immediately
			rows, err = s.DB.Query(r.Context(), threadSelect+`WHERE (t.a_user_id = $1 OR (t.b_user_id = $1 AND lm.id IS NOT NULL))
				AND `+views.NotBlocked(`CASE WHEN t.a_user_id = $1 THEN COALESCE(t.b_user_id, '') ELSE t.a_user_id END`)+`
				ORDER BY t.last_message_at DESC LIMIT 200`, who.UserID)
		}
		if err != nil {
			return nil, err
		}
		list := []threadRow{}
		for rows.Next() {
			t, err := scanThread(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			list = append(list, t)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		out := make([]domain.ThreadSummary, 0, len(list))
		for _, t := range list {
			sum, err := s.summary(r.Context(), t, scope, t.side(scope, who))
			if err != nil {
				return nil, err
			}
			out = append(out, sum)
		}
		return out, nil
	}
}

// load finds a thread only if the caller participates in it; anything else looks missing.
func (s *Service) load(ctx context.Context, id, scope string, who *httpx.Principal) (threadRow, string, error) {
	t, err := scanThread(s.DB.QueryRow(ctx, threadSelect+`WHERE t.id = $1`, id))
	if db.IsNotFound(err) {
		return t, "", httpx.NotFound("Conversation not found")
	}
	if err != nil {
		return t, "", err
	}
	side := t.side(scope, who)
	if side == "" {
		return t, "", httpx.NotFound("Conversation not found")
	}
	return t, side, nil
}

func (s *Service) getThread(scope string) httpx.Handler {
	return func(_ http.ResponseWriter, r *http.Request) (any, error) {
		who := httpx.Who(r)
		t, side, err := s.load(r.Context(), r.PathValue("id"), scope, who)
		if err != nil {
			return nil, err
		}
		sum, err := s.summary(r.Context(), t, scope, side)
		if err != nil {
			return nil, err
		}
		rows, err := s.DB.Query(r.Context(), `SELECT id, side, text, image, created_at FROM messages WHERE thread_id = $1 ORDER BY created_at, id`, t.ID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		now := s.Now()
		msgs := []domain.Message{}
		for rows.Next() {
			var m domain.Message
			var msgSide string
			var at time.Time
			if err := rows.Scan(&m.ID, &msgSide, &m.Text, &m.Image, &at); err != nil {
				return nil, err
			}
			m.FromMe = msgSide == side
			m.Time = domain.ChatTime(at, now)
			msgs = append(msgs, m)
		}
		return domain.Thread{ThreadSummary: sum, Messages: msgs}, rows.Err()
	}
}

func (s *Service) readThread(scope string) httpx.Handler {
	return func(_ http.ResponseWriter, r *http.Request) (any, error) {
		t, side, err := s.load(r.Context(), r.PathValue("id"), scope, httpx.Who(r))
		if err != nil {
			return nil, err
		}
		_, err = s.DB.Exec(r.Context(), `UPDATE threads SET `+side+`_unread = 0 WHERE id = $1`, t.ID)
		return nil, err
	}
}

func (s *Service) sendMessage(scope string) httpx.Handler {
	return func(_ http.ResponseWriter, r *http.Request) (any, error) {
		who := httpx.Who(r)
		ctx := r.Context()
		t, side, err := s.load(ctx, r.PathValue("id"), scope, who)
		if err != nil {
			return nil, err
		}
		var body struct {
			Text     string `json:"text"`
			ImageURL string `json:"imageUrl"`
		}
		if err := httpx.Decode(r, &body); err != nil {
			return nil, err
		}
		text := strings.TrimSpace(body.Text)
		var image *domain.Photo
		if body.ImageURL != "" {
			if image, err = media.Photo(ctx, s.DB, who, body.ImageURL); err != nil {
				return nil, err
			}
		}
		if text == "" && image == nil {
			return nil, httpx.BadRequest("Message is empty")
		}
		if len([]rune(text)) > maxMessage {
			return nil, httpx.BadRequest("That message is too long")
		}
		if t.Kind == "person" {
			other := t.AUser
			if side == "a" {
				other = *t.BUser
			}
			if blocked, err := blockedBetween(ctx, s.DB, who.UserID, other); err != nil {
				return nil, err
			} else if blocked {
				return nil, httpx.Forbidden("You can’t message this account")
			}
		}
		if err := httpx.Limit(ctx, s.DB, "msg:"+who.UserID, 120, time.Minute, "You’re sending messages too quickly"); err != nil {
			return nil, err
		}
		id := secure.ID("m")
		otherUnread := "b_unread"
		if side == "b" {
			otherUnread = "a_unread"
		}
		err = db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `INSERT INTO messages (id, thread_id, side, sender_user_id, text, image, created_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7)`, id, t.ID, side, who.UserID, text, image, s.Now()); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE threads SET last_message_at = $2, `+otherUnread+` = `+otherUnread+` + 1 WHERE id = $1`, t.ID, s.Now())
			return err
		})
		if err != nil {
			return nil, err
		}
		return domain.Message{ID: id, FromMe: true, Text: text, Image: image, Time: "Now"}, nil
	}
}

func blockedBetween(ctx context.Context, q db.Q, a, b string) (bool, error) {
	var blocked bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM blocks WHERE (blocker_id = $1 AND blocked_id = $2) OR (blocker_id = $2 AND blocked_id = $1))`, a, b).Scan(&blocked)
	return blocked, err
}

// startShopperThread gets or creates a conversation with a brand or another person.
func (s *Service) startShopperThread(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	var body struct {
		BrandID   string  `json:"brandId"`
		Handle    string  `json:"handle"`
		ProductID *string `json:"productId"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		return nil, err
	}
	var threadID string
	switch {
	case body.BrandID != "":
		var accepts bool
		var status string
		err := s.DB.QueryRow(ctx, `SELECT accept_messages, status FROM brands WHERE id = $1`, body.BrandID).Scan(&accepts, &status)
		if db.IsNotFound(err) {
			return nil, httpx.NotFound("Brand not found")
		}
		if err != nil {
			return nil, err
		}
		if who.BrandID == body.BrandID {
			return nil, httpx.BadRequest("This is your own brand — use the studio inbox")
		}
		err = s.DB.QueryRow(ctx, `SELECT id FROM threads WHERE kind = 'brand' AND a_user_id = $1 AND brand_id = $2`, who.UserID, body.BrandID).Scan(&threadID)
		if db.IsNotFound(err) {
			if !accepts || status != "verified" {
				return nil, httpx.Forbidden("This brand isn’t taking messages right now")
			}
			threadID = secure.ID("t")
			_, err = s.DB.Exec(ctx, `INSERT INTO threads (id, kind, a_user_id, brand_id, product_id, last_message_at)
				VALUES ($1,'brand',$2,$3,$4,$5)
				ON CONFLICT (a_user_id, brand_id) WHERE kind = 'brand' DO NOTHING`, threadID, who.UserID, body.BrandID, body.ProductID, s.Now())
			if err != nil {
				return nil, err
			}
			err = s.DB.QueryRow(ctx, `SELECT id FROM threads WHERE kind = 'brand' AND a_user_id = $1 AND brand_id = $2`, who.UserID, body.BrandID).Scan(&threadID)
		}
		if err != nil {
			return nil, err
		}
	case body.Handle != "":
		var other string
		err := s.DB.QueryRow(ctx, `SELECT id FROM users WHERE handle = $1 AND verified_at IS NOT NULL`, domain.NormalizeHandle(body.Handle)).Scan(&other)
		if db.IsNotFound(err) {
			return nil, httpx.NotFound("Account not found")
		}
		if err != nil {
			return nil, err
		}
		if other == who.UserID {
			return nil, httpx.BadRequest("You can’t message yourself")
		}
		if blocked, err := blockedBetween(ctx, s.DB, who.UserID, other); err != nil {
			return nil, err
		} else if blocked {
			return nil, httpx.Forbidden("You can’t message this account")
		}
		err = s.DB.QueryRow(ctx, `SELECT id FROM threads WHERE kind = 'person' AND
			((a_user_id = $1 AND b_user_id = $2) OR (a_user_id = $2 AND b_user_id = $1))`, who.UserID, other).Scan(&threadID)
		if db.IsNotFound(err) {
			threadID = secure.ID("t")
			_, err = s.DB.Exec(ctx, `INSERT INTO threads (id, kind, a_user_id, b_user_id, last_message_at) VALUES ($1,'person',$2,$3,$4)`,
				threadID, who.UserID, other, s.Now())
		}
		if err != nil {
			return nil, err
		}
	default:
		return nil, httpx.BadRequest("Choose who to message")
	}
	return s.summaryByID(ctx, threadID, "shopper", who)
}

// startStudioThread opens a conversation with someone who has bought from this brand or
// already messaged it. Brands can't cold-message arbitrary accounts.
func (s *Service) startStudioThread(_ http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	ctx := r.Context()
	var body struct {
		CustomerID string `json:"customerId"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		return nil, err
	}
	if body.CustomerID == "" {
		return nil, httpx.BadRequest("Customer is required")
	}
	var threadID string
	err := s.DB.QueryRow(ctx, `SELECT id FROM threads WHERE kind = 'brand' AND a_user_id = $1 AND brand_id = $2`, body.CustomerID, who.BrandID).Scan(&threadID)
	if db.IsNotFound(err) {
		var bought bool
		if err := s.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM orders o WHERE o.user_id = $1 AND o.brand_id = $2 AND `+views.PaidOrder+`)`,
			body.CustomerID, who.BrandID).Scan(&bought); err != nil {
			return nil, err
		}
		if !bought {
			return nil, httpx.Forbidden("You can message people who’ve ordered from you")
		}
		threadID = secure.ID("t")
		_, err = s.DB.Exec(ctx, `INSERT INTO threads (id, kind, a_user_id, brand_id, last_message_at) VALUES ($1,'brand',$2,$3,$4)`,
			threadID, body.CustomerID, who.BrandID, s.Now())
	}
	if err != nil {
		return nil, err
	}
	return s.summaryByID(ctx, threadID, "studio", who)
}

func (s *Service) summaryByID(ctx context.Context, id, scope string, who *httpx.Principal) (any, error) {
	t, side, err := s.load(ctx, id, scope, who)
	if err != nil {
		return nil, err
	}
	return s.summary(ctx, t, scope, side)
}
