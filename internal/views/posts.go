package views

// PostVisible is the SQL rule for which posts a viewer can see ($1 = viewer id; aliases p =
// posts, b = brands, u = users): published and unexpired, from a verified brand or a person
// who hasn't blocked the viewer (or been blocked by them), respecting private accounts.
const PostVisible = `(p.status = 'published' AND (p.expires_at IS NULL OR p.expires_at > now())
	AND (p.brand_id IS NULL OR b.status = 'verified')
	AND (p.user_id IS NULL OR p.user_id = $1 OR (
		(COALESCE((u.prefs ->> 'private')::boolean, false) = false
			OR EXISTS (SELECT 1 FROM follows_user fv WHERE fv.follower_id = $1 AND fv.followee_id = p.user_id AND fv.approved))
		AND NOT EXISTS (SELECT 1 FROM blocks bl WHERE (bl.blocker_id = $1 AND bl.blocked_id = p.user_id)
			OR (bl.blocker_id = p.user_id AND bl.blocked_id = $1)))))`

// PostFrom joins the tables PostVisible needs.
const PostFrom = ` FROM posts p LEFT JOIN brands b ON b.id = p.brand_id LEFT JOIN users u ON u.id = p.user_id `
