// Package inbox implements conversations and notifications.
package inbox

import (
	"context"
	"net/http"
	"time"

	"wearwhat-be/internal/app"
	"wearwhat-be/internal/db"
	"wearwhat-be/internal/domain"
	"wearwhat-be/internal/httpx"
	"wearwhat-be/internal/secure"
)

// Note is a notification to create. Pref names the preference that can silence it.
type Note struct {
	UserID  string // shopper-scope recipient
	BrandID string // studio-scope recipient
	Who     string
	Text    string
	Kind    string // Drops | Social | Orders
	Avatar  *domain.Photo
	Thumb   *domain.Photo
	Href    string
	Action  *domain.NotificationAction
	Pref    string
}

// Notify stores a notification unless the recipient has switched that kind off.
func Notify(ctx context.Context, q db.Q, n Note) error {
	scope := "shopper"
	if n.BrandID != "" {
		scope = "studio"
	}
	if n.Pref != "" {
		var enabled bool
		var err error
		if scope == "studio" {
			// a brand's alerts follow its owner's preferences; ownerless demo brands stay on
			err = q.QueryRow(ctx, `SELECT COALESCE((SELECT COALESCE((u.prefs ->> $2)::boolean, true)
				FROM brands b JOIN users u ON u.id = b.owner_id WHERE b.id = $1), true)`, n.BrandID, n.Pref).Scan(&enabled)
		} else {
			err = q.QueryRow(ctx, `SELECT COALESCE((prefs ->> $2)::boolean, true) FROM users WHERE id = $1`, n.UserID, n.Pref).Scan(&enabled)
		}
		if err != nil {
			return err
		}
		if !enabled {
			return nil
		}
	}
	_, err := q.Exec(ctx, `INSERT INTO notifications (id, scope, user_id, brand_id, who, text, kind, avatar, thumb, href, action)
		VALUES ($1,$2,NULLIF($3,''),NULLIF($4,''),$5,$6,$7,$8,$9,$10,$11)`,
		secure.ID("n"), scope, n.UserID, n.BrandID, n.Who, n.Text, n.Kind, n.Avatar, n.Thumb, n.Href, n.Action)
	return err
}

// NotifyFollowers fans a notification out to a brand's followers who allow that kind.
func NotifyFollowers(ctx context.Context, q db.Q, brandID string, n Note) error {
	_, err := q.Exec(ctx, `INSERT INTO notifications (id, scope, user_id, who, text, kind, avatar, thumb, href, action)
		SELECT 'n_' || replace(gen_random_uuid()::text, '-', ''), 'shopper', f.user_id, $2, $3, $4, $5, $6, $7, $8
		FROM follows_brand f JOIN users u ON u.id = f.user_id
		WHERE f.brand_id = $1 AND COALESCE((u.prefs ->> $9)::boolean, true)`,
		brandID, n.Who, n.Text, n.Kind, n.Avatar, n.Thumb, n.Href, n.Action, n.Pref)
	return err
}

/* ---------- routes ---------- */

// ActionHandler runs the real effect of a notification button; done is the state after the toggle.
type ActionHandler func(ctx context.Context, who *httpx.Principal, action *domain.NotificationAction, done bool) (any, error)

type Service struct {
	*app.Deps
	// Actions handle "follow" and "amplify"; they live with the features they change.
	Actions map[string]ActionHandler
}

func (s *Service) Routes(r *httpx.Router) {
	r.User("GET /notifications", s.list("shopper"))
	r.User("GET /notifications/unread-count", s.unread("shopper"))
	r.User("POST /notifications/read", s.markRead("shopper"))
	r.User("POST /notifications/{id}/action", s.action("shopper"))

	r.Brand("GET /studio/notifications", httpx.CapAny, s.list("studio"))
	r.Brand("GET /studio/notifications/unread-count", httpx.CapAny, s.unread("studio"))
	r.Brand("POST /studio/notifications/read", httpx.CapAny, s.markRead("studio"))
	r.Brand("POST /studio/notifications/{id}/action", httpx.CapAny, s.action("studio"))

	s.threadRoutes(r)
}

// owner returns the SQL condition and its argument limiting notifications to the caller.
func owner(scope string, who *httpx.Principal) (string, string) {
	if scope == "studio" {
		return `scope = 'studio' AND brand_id = $1`, who.BrandID
	}
	return `scope = 'shopper' AND user_id = $1`, who.UserID
}

func (s *Service) list(scope string) httpx.Handler {
	return func(_ http.ResponseWriter, r *http.Request) (any, error) {
		cond, arg := owner(scope, httpx.Who(r))
		rows, err := s.DB.Query(r.Context(), `SELECT id, who, text, kind, avatar, thumb, href, action, read_at, created_at
			FROM notifications WHERE `+cond+` ORDER BY created_at DESC LIMIT 100`, arg)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		now := s.Now()
		out := []domain.AppNotification{}
		for rows.Next() {
			var n domain.AppNotification
			var readAt *time.Time
			var created time.Time
			if err := rows.Scan(&n.ID, &n.Who, &n.Text, &n.Kind, &n.Avatar, &n.Thumb, &n.Href, &n.Action, &readAt, &created); err != nil {
				return nil, err
			}
			n.Group = domain.NotificationGroup(created, now)
			n.Time = domain.Ago(created, now)
			n.Unread = readAt == nil
			out = append(out, n)
		}
		return out, rows.Err()
	}
}

func (s *Service) unread(scope string) httpx.Handler {
	return func(_ http.ResponseWriter, r *http.Request) (any, error) {
		cond, arg := owner(scope, httpx.Who(r))
		var n int
		err := s.DB.QueryRow(r.Context(), `SELECT count(*) FROM notifications WHERE read_at IS NULL AND `+cond, arg).Scan(&n)
		return map[string]int{"count": n}, err
	}
}

func (s *Service) markRead(scope string) httpx.Handler {
	return func(_ http.ResponseWriter, r *http.Request) (any, error) {
		cond, arg := owner(scope, httpx.Who(r))
		_, err := s.DB.Exec(r.Context(), `UPDATE notifications SET read_at = now() WHERE read_at IS NULL AND `+cond, arg)
		return nil, err
	}
}

func (s *Service) action(scope string) httpx.Handler {
	return func(_ http.ResponseWriter, r *http.Request) (any, error) {
		who := httpx.Who(r)
		cond, arg := owner(scope, who)
		var action *domain.NotificationAction
		err := s.DB.QueryRow(r.Context(), `SELECT action FROM notifications WHERE id = $2 AND `+cond, arg, r.PathValue("id")).Scan(&action)
		if db.IsNotFound(err) || (err == nil && action == nil) {
			return nil, httpx.NotFound("Notification not found")
		}
		if err != nil {
			return nil, err
		}
		done := !action.Done
		var result any
		if h := s.Actions[action.Type]; h != nil {
			result, err = h(r.Context(), who, action, done)
			if err != nil {
				return nil, err
			}
		}
		action.Done = done
		if _, err := s.DB.Exec(r.Context(), `UPDATE notifications SET action = $2 WHERE id = $1`, r.PathValue("id"), action); err != nil {
			return nil, err
		}
		return result, nil
	}
}
