package server_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"wearwhat-be/internal/seed"
)

func TestConversationSeenFromBothSides(t *testing.T) {
	e := newEnv(t)
	owner := e.brandOwner()
	c := e.shopper()

	type summary struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Kind   string `json:"kind"`
		Unread int    `json:"unread"`
		Last   *struct {
			Text   string `json:"text"`
			FromMe bool   `json:"fromMe"`
		} `json:"last"`
	}
	var th summary
	c.ok("POST", "/threads", m{"brandId": owner.brandID}, &th)
	if th.Kind != "brand" {
		t.Errorf("shopper sees kind %q", th.Kind)
	}
	// get-or-create really gets
	var again summary
	c.ok("POST", "/threads", m{"brandId": owner.brandID}, &again)
	if again.ID != th.ID {
		t.Error("a second thread was created")
	}
	// the brand doesn't see an empty thread
	var list []summary
	owner.ok("GET", "/studio/threads", nil, &list)
	if len(list) != 0 {
		t.Errorf("brand sees %d empty threads", len(list))
	}

	c.status(400, "POST", "/threads/"+th.ID+"/messages", m{"text": "   "})
	c.ok("POST", "/threads/"+th.ID+"/messages", m{"text": "Does this run big?"}, nil)

	owner.ok("GET", "/studio/threads", nil, &list)
	if len(list) != 1 || list[0].Kind != "customer" || list[0].Unread != 1 || list[0].Last.Text != "Does this run big?" || list[0].Last.FromMe {
		t.Fatalf("brand's list: %+v %+v", list, list[0].Last)
	}

	type thread struct {
		Messages []struct {
			Text   string `json:"text"`
			FromMe bool   `json:"fromMe"`
		} `json:"messages"`
	}
	owner.ok("POST", "/studio/threads/"+th.ID+"/read", nil, nil)
	owner.ok("GET", "/studio/threads", nil, &list)
	if list[0].Unread != 0 {
		t.Error("reading didn't clear the brand's unread count")
	}
	owner.ok("POST", "/studio/threads/"+th.ID+"/messages", m{"text": "It's boxy by design."}, nil)

	var shopperList []summary
	c.ok("GET", "/threads", nil, &shopperList)
	if shopperList[0].Unread != 1 || shopperList[0].Last.FromMe {
		t.Errorf("shopper's list: %+v", shopperList[0])
	}
	var view thread
	c.ok("GET", "/threads/"+th.ID, nil, &view)
	if len(view.Messages) != 2 || !view.Messages[0].FromMe || view.Messages[1].FromMe {
		t.Errorf("shopper's view: %+v", view.Messages)
	}
	owner.ok("GET", "/studio/threads/"+th.ID, nil, &view)
	if view.Messages[0].FromMe || !view.Messages[1].FromMe {
		t.Errorf("brand's view: %+v", view.Messages)
	}

	t.Run("photo messages", func(t *testing.T) {
		u := c.upload("image", pngBytes(t))
		var msg struct {
			Image *struct {
				URL string `json:"url"`
			} `json:"image"`
		}
		c.ok("POST", "/threads/"+th.ID+"/messages", m{"text": "", "imageUrl": u.URL}, &msg)
		if msg.Image == nil || msg.Image.URL != u.URL {
			t.Errorf("message: %+v", msg)
		}
		c.status(400, "POST", "/threads/"+th.ID+"/messages", m{"imageUrl": "https://evil.example/x.png"})
	})

	t.Run("a brand can open a thread with its own customer", func(t *testing.T) {
		buyer := e.shopper().withAddress()
		pid := owner.addProduct(5000, map[string]int{"M": 5})
		buyer.buy(pid, "M")
		var fresh summary
		owner.ok("POST", "/studio/threads", m{"customerId": buyer.id}, &fresh)
		if fresh.Kind != "customer" {
			t.Errorf("kind %q", fresh.Kind)
		}
	})

	t.Run("a brand that doesn't take messages can't be cold-messaged", func(t *testing.T) {
		quiet := e.brandOwner()
		var s struct {
			Store m `json:"store"`
		}
		quiet.ok("GET", "/studio/settings", nil, &s)
		s.Store["acceptMessages"] = false
		quiet.ok("PUT", "/studio/settings/store", s.Store, nil)
		e.shopper().status(403, "POST", "/threads", m{"brandId": quiet.brandID})
	})

	t.Run("you can't message your own brand as a shopper", func(t *testing.T) {
		owner.status(400, "POST", "/threads", m{"brandId": owner.brandID})
	})
}

func TestPersonToPersonMessages(t *testing.T) {
	e := newEnv(t)
	a, b := e.shopper(), e.shopper()
	var th struct {
		ID, Kind, Name string
	}
	a.ok("POST", "/threads", m{"handle": b.handle}, &th)
	if th.Kind != "person" || th.Name != b.handle {
		t.Errorf("thread: %+v", th)
	}
	// starting it from the other side finds the same thread
	var same struct{ ID string }
	b.ok("POST", "/threads", m{"handle": a.handle}, &same)
	if same.ID != th.ID {
		t.Error("two threads between the same two people")
	}
	a.ok("POST", "/threads/"+th.ID+"/messages", m{"text": "hi"}, nil)
	var list []struct {
		Name   string `json:"name"`
		Unread int    `json:"unread"`
	}
	b.ok("GET", "/threads", nil, &list)
	if len(list) != 1 || list[0].Unread != 1 || list[0].Name != a.handle {
		t.Errorf("B's list: %+v", list)
	}
	a.status(400, "POST", "/threads", m{"handle": a.handle})

	t.Run("blocking stops messages both ways and hides the thread", func(t *testing.T) {
		b.ok("PUT", "/users/"+a.handle+"/block", nil, nil)
		a.status(403, "POST", "/threads/"+th.ID+"/messages", m{"text": "still here?"})
		b.status(403, "POST", "/threads/"+th.ID+"/messages", m{"text": "no"})
		a.ok("GET", "/threads", nil, &list)
		if len(list) != 0 {
			t.Errorf("thread still listed: %+v", list)
		}
		a.status(403, "POST", "/threads", m{"handle": b.handle})
	})
}

func TestFollowsAndPrivateAccounts(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.shopper(), e.shopper()
	type profile struct {
		Followers  int  `json:"followers"`
		IsFollowed bool `json:"isFollowed"`
		Requested  bool `json:"requested"`
		FollowsYou bool `json:"followsYou"`
		Locked     bool `json:"locked"`
		WearsCount int  `json:"wearsCount"`
		Wears      []struct {
			ID string `json:"id"`
		} `json:"wears"`
	}

	bob.ok("PUT", "/users/"+alice.handle+"/follow", nil, nil)
	var p profile
	bob.ok("GET", "/users/"+alice.handle, nil, &p)
	if !p.IsFollowed || p.Followers != 1 {
		t.Fatalf("after follow: %+v", p)
	}
	// following twice doesn't double-count
	bob.ok("PUT", "/users/"+alice.handle+"/follow", nil, nil)
	bob.ok("GET", "/users/"+alice.handle, nil, &p)
	if p.Followers != 1 {
		t.Errorf("followers %d", p.Followers)
	}
	var me struct {
		Following int `json:"following"`
	}
	bob.ok("GET", "/me", nil, &me)
	if me.Following != 1 {
		t.Errorf("following %d", me.Following)
	}

	// Alice was told, with a real follow-back button
	var notes []struct {
		ID     string `json:"id"`
		Text   string `json:"text"`
		Action struct {
			Type string `json:"type"`
			Done bool   `json:"done"`
		} `json:"action"`
	}
	alice.ok("GET", "/notifications", nil, &notes)
	if len(notes) != 1 || notes[0].Action.Type != "follow" {
		t.Fatalf("notifications: %+v", notes)
	}
	alice.ok("POST", "/notifications/"+notes[0].ID+"/action", nil, nil)
	alice.ok("GET", "/users/"+bob.handle, nil, &p)
	if !p.IsFollowed || !p.FollowsYou {
		t.Errorf("follow-back didn't happen: %+v", p)
	}
	alice.ok("GET", "/notifications", nil, &notes)
	if !notes[0].Action.Done {
		t.Error("the button didn't record its state")
	}
	alice.ok("POST", "/notifications/"+notes[0].ID+"/action", nil, nil)
	alice.ok("GET", "/users/"+bob.handle, nil, &p)
	if p.IsFollowed {
		t.Error("toggling the button again should unfollow")
	}

	bob.ok("DELETE", "/users/"+alice.handle+"/follow", nil, nil)
	bob.ok("GET", "/users/"+alice.handle, nil, &p)
	if p.IsFollowed || p.Followers != 0 {
		t.Errorf("after unfollow: %+v", p)
	}

	t.Run("private accounts approve followers and hide wears until then", func(t *testing.T) {
		owner := e.brandOwner()
		pid := owner.addProduct(5000, map[string]int{"M": 5})
		private, follower := e.shopper(), e.shopper()
		u := private.upload("image", pngBytes(t))
		var wear struct {
			ID string `json:"id"`
		}
		private.ok("POST", "/wears", m{"imageUrl": u.URL, "productId": pid, "fit": "True to size", "caption": "secret outfit", "hashtags": []string{"#Private"}}, &wear)
		private.ok("PATCH", "/me", m{"prefs": m{"private": true}}, nil)

		var view profile
		follower.ok("GET", "/users/"+private.handle, nil, &view)
		if !view.Locked || len(view.Wears) != 0 {
			t.Fatalf("a stranger can see a private account: %+v", view)
		}
		follower.status(404, "GET", "/posts/"+wear.ID, nil)
		follower.status(404, "PUT", "/posts/"+wear.ID+"/like", nil)
		var feed struct {
			Posts []struct {
				ID string `json:"id"`
			} `json:"posts"`
		}
		follower.ok("GET", "/feed", nil, &feed)
		for _, x := range feed.Posts {
			if x.ID == wear.ID {
				t.Fatal("a private wear is in a stranger's feed")
			}
		}

		// following only requests
		var res struct {
			State string `json:"state"`
		}
		follower.ok("PUT", "/users/"+private.handle+"/follow", nil, &res)
		if res.State != "requested" {
			t.Fatalf("state %q", res.State)
		}
		follower.ok("GET", "/users/"+private.handle, nil, &view)
		if view.IsFollowed || !view.Requested || !view.Locked || view.Followers != 0 {
			t.Fatalf("while requested: %+v", view)
		}

		// approving unlocks it and counts the follower
		var pending []struct {
			ID     string `json:"id"`
			Action struct {
				Type string `json:"type"`
			} `json:"action"`
		}
		private.ok("GET", "/notifications", nil, &pending)
		if len(pending) != 1 || pending[0].Action.Type != "approve" {
			t.Fatalf("notifications: %+v", pending)
		}
		private.ok("POST", "/notifications/"+pending[0].ID+"/action", nil, nil)
		follower.ok("GET", "/users/"+private.handle, nil, &view)
		if !view.IsFollowed || view.Locked || view.Followers != 1 || len(view.Wears) != 1 {
			t.Fatalf("after approval: %+v", view)
		}
		follower.ok("GET", "/posts/"+wear.ID, nil, nil)

		// going public approves anyone still waiting
		waiting := e.shopper()
		waiting.ok("PUT", "/users/"+private.handle+"/follow", nil, &res)
		private.ok("PATCH", "/me", m{"prefs": m{"private": false}}, nil)
		waiting.ok("GET", "/users/"+private.handle, nil, &view)
		if !view.IsFollowed || view.Followers != 2 {
			t.Errorf("after going public: %+v", view)
		}
	})
}

func TestBlocking(t *testing.T) {
	e := newEnv(t)
	owner := e.brandOwner()
	pid := owner.addProduct(5000, map[string]int{"M": 5})
	alice, bob := e.shopper(), e.shopper()
	bob.ok("PUT", "/users/"+alice.handle+"/follow", nil, nil)
	u := alice.upload("image", pngBytes(t))
	var wear struct {
		ID string `json:"id"`
	}
	alice.ok("POST", "/wears", m{"imageUrl": u.URL, "productId": pid, "fit": "True to size", "caption": "x", "hashtags": []string{}}, &wear)
	var comment struct {
		ID string `json:"id"`
	}
	bob.ok("POST", "/posts/"+wear.ID+"/comments", m{"text": "nice"}, &comment)

	alice.ok("PUT", "/users/"+bob.handle+"/block", nil, nil)

	// Bob can't find Alice at all, and the follow is gone
	bob.status(404, "GET", "/users/"+alice.handle, nil)
	bob.status(404, "GET", "/posts/"+wear.ID, nil)
	alice.status(404, "GET", "/users/"+bob.handle, nil)
	var me struct {
		Followers int `json:"followers"`
	}
	alice.ok("GET", "/me", nil, &me)
	if me.Followers != 0 {
		t.Errorf("followers %d", me.Followers)
	}
	// his comment disappears from her view
	var comments []any
	alice.ok("GET", "/posts/"+wear.ID+"/comments", nil, &comments)
	if len(comments) != 0 {
		t.Errorf("%d comments", len(comments))
	}

	var blocked []struct {
		Handle string `json:"handle"`
	}
	alice.ok("GET", "/me/blocks", nil, &blocked)
	if len(blocked) != 1 || blocked[0].Handle != bob.handle {
		t.Fatalf("blocks: %+v", blocked)
	}
	// and it can be undone from the list, even though the account is otherwise hidden
	alice.ok("DELETE", "/users/"+bob.handle+"/block", nil, nil)
	bob.ok("GET", "/users/"+alice.handle, nil, nil)
	alice.ok("GET", "/me/blocks", nil, &blocked)
	if len(blocked) != 0 {
		t.Errorf("still blocked: %+v", blocked)
	}
	alice.status(400, "PUT", "/users/"+alice.handle+"/block", nil)
}

func TestLikesSavesAndComments(t *testing.T) {
	e := newEnv(t)
	owner := e.brandOwner()
	pid := owner.addProduct(5000, map[string]int{"M": 5})
	u := owner.upload("image", pngBytes(t))
	var tile struct {
		ID string `json:"id"`
	}
	owner.ok("POST", "/studio/media", m{"url": u.URL}, &tile)
	var post struct {
		ID string `json:"id"`
	}
	owner.ok("POST", "/studio/posts", m{"type": "Post", "mediaIds": []string{tile.ID}, "productIds": []string{pid}, "caption": "new", "when": "Now"}, &post)

	c := e.shopper()
	type view struct {
		Likes         int  `json:"likes"`
		Liked         bool `json:"liked"`
		Saved         bool `json:"saved"`
		CommentsCount int  `json:"commentsCount"`
		Mine          bool `json:"mine"`
		Shop          []struct {
			ID string `json:"id"`
		} `json:"shop"`
		Tags []struct {
			ProductID string `json:"productId"`
		} `json:"tags"`
	}
	var v view
	c.ok("PUT", "/posts/"+post.ID+"/like", nil, nil)
	c.ok("PUT", "/posts/"+post.ID+"/like", nil, nil) // idempotent
	c.ok("GET", "/posts/"+post.ID, nil, &v)
	if v.Likes != 1 || !v.Liked {
		t.Fatalf("liked: %+v", v)
	}
	if len(v.Shop) != 1 || v.Shop[0].ID != pid || len(v.Tags) != 1 {
		t.Errorf("tagged products: %+v", v)
	}
	c.ok("DELETE", "/posts/"+post.ID+"/like", nil, nil)
	c.ok("DELETE", "/posts/"+post.ID+"/like", nil, nil)
	c.ok("GET", "/posts/"+post.ID, nil, &v)
	if v.Likes != 0 || v.Liked {
		t.Fatalf("unliked: %+v", v)
	}

	c.ok("PUT", "/posts/"+post.ID+"/save", nil, nil)
	var saved []struct {
		ID string `json:"id"`
	}
	c.ok("GET", "/me/saved", nil, &saved)
	if len(saved) != 1 || saved[0].ID != post.ID {
		t.Fatalf("saved: %+v", saved)
	}
	var me struct {
		SavedCount int `json:"savedCount"`
	}
	c.ok("GET", "/me", nil, &me)
	if me.SavedCount != 1 {
		t.Errorf("savedCount %d", me.SavedCount)
	}
	c.ok("DELETE", "/posts/"+post.ID+"/save", nil, nil)

	c.status(404, "PUT", "/posts/missing/like", nil)

	t.Run("comments", func(t *testing.T) {
		c.status(400, "POST", "/posts/"+post.ID+"/comments", m{"text": " "})
		c.status(400, "POST", "/posts/"+post.ID+"/comments", m{"text": strings.Repeat("x", 501)})
		var made struct {
			ID        string `json:"id"`
			Text      string `json:"text"`
			CanDelete bool   `json:"canDelete"`
		}
		c.ok("POST", "/posts/"+post.ID+"/comments", m{"text": "  <b>Love</b> it  "}, &made)
		// stored as written: React escapes on display, so nothing is mangled or rendered as HTML
		if made.Text != "<b>Love</b> it" || !made.CanDelete {
			t.Errorf("comment: %+v", made)
		}
		c.ok("GET", "/posts/"+post.ID, nil, &v)
		if v.CommentsCount != 1 {
			t.Errorf("count %d", v.CommentsCount)
		}
		// the brand is told, and can open the post from it
		var notes []struct {
			Text string `json:"text"`
			Href string `json:"href"`
		}
		owner.ok("GET", "/studio/notifications", nil, &notes)
		if len(notes) == 0 || !strings.Contains(notes[0].Text, "commented") || notes[0].Href != "/post/"+post.ID {
			t.Errorf("notification: %+v", notes)
		}

		stranger := e.shopper()
		stranger.status(404, "DELETE", "/comments/"+made.ID, nil)
		var list []struct {
			CanDelete bool `json:"canDelete"`
		}
		stranger.ok("GET", "/posts/"+post.ID+"/comments", nil, &list)
		if len(list) != 1 || list[0].CanDelete {
			t.Errorf("a stranger's view: %+v", list)
		}
		// the post's owner can moderate their own post
		owner.ok("GET", "/posts/"+post.ID+"/comments", nil, &list)
		if !list[0].CanDelete {
			t.Error("the brand should be able to remove comments on its post")
		}
		owner.ok("DELETE", "/comments/"+made.ID, nil, nil)
		c.ok("GET", "/posts/"+post.ID, nil, &v)
		if v.CommentsCount != 0 {
			t.Errorf("count after delete %d", v.CommentsCount)
		}
	})

	t.Run("reports", func(t *testing.T) {
		c.ok("POST", "/reports", m{"kind": "post", "id": post.ID, "reason": "spam"}, nil)
		c.ok("POST", "/reports", m{"kind": "post", "id": post.ID, "reason": "spam again"}, nil)
		if n := e.count(`SELECT count(*) FROM reports`); n != 1 {
			t.Errorf("%d reports for one thing", n)
		}
		c.status(400, "POST", "/reports", m{"kind": "planet", "id": "x"})
	})

	t.Run("only the author can delete a post", func(t *testing.T) {
		e.shopper().status(404, "DELETE", "/posts/"+post.ID, nil)
		owner.ok("DELETE", "/posts/"+post.ID, nil, nil)
		c.status(404, "GET", "/posts/"+post.ID, nil)
	})
}

func TestWearFlow(t *testing.T) {
	e := newEnv(t)
	owner := e.brandOwner()
	pid := owner.addProduct(10000, map[string]int{"M": 5})
	c := e.shopper().withAddress()
	u := c.upload("image", pngBytes(t))
	post := func(extra m) (string, string) {
		body := m{"imageUrl": u.URL, "productId": pid, "size": "M", "fit": "True to size", "caption": "fits", "hashtags": []string{"#OOTD"}, "location": "Lagos"}
		for k, v := range extra {
			body[k] = v
		}
		var res struct {
			ID         string `json:"id"`
			RewardCode string `json:"rewardCode"`
		}
		c.ok("POST", "/wears", body, &res)
		return res.ID, res.RewardCode
	}
	c.status(400, "POST", "/wears", m{"imageUrl": u.URL, "productId": "missing", "fit": "True to size"})
	c.status(400, "POST", "/wears", m{"imageUrl": u.URL, "productId": pid, "fit": "Vibes"})
	c.status(400, "POST", "/wears", m{"imageUrl": u.URL, "productId": pid, "fit": "True to size", "hashtags": []string{"not a tag"}})

	id, reward := post(nil)
	if reward != "" {
		t.Errorf("no delivered order yet, but got reward %q", reward)
	}

	var view struct {
		Subtitle string `json:"subtitle"`
		Wear     *struct {
			Meta string `json:"meta"`
		} `json:"wear"`
	}
	c.ok("GET", "/posts/"+id, nil, &view)
	if view.Wear == nil || !strings.Contains(view.Wear.Meta, "SIZE M") || !strings.Contains(view.Wear.Meta, "FITS TRUE") {
		t.Errorf("wear: %+v", view.Wear)
	}
	if !strings.Contains(view.Subtitle, "Lagos") {
		t.Errorf("subtitle: %q", view.Subtitle)
	}

	// it counts as a real fit report and "worn by" entry
	var prod struct {
		Fit struct {
			Summary string `json:"summary"`
			Wears   int    `json:"wears"`
		} `json:"fit"`
		WornBy []struct {
			Handle string `json:"handle"`
		} `json:"wornBy"`
	}
	c.ok("GET", "/products/"+pid, nil, &prod)
	if prod.Fit.Wears != 1 || prod.Fit.Summary != "Fits true to size" || len(prod.WornBy) != 1 || prod.WornBy[0].Handle != "@"+c.handle {
		t.Errorf("product: %+v", prod)
	}
	// and a real trend
	var trends []struct {
		Tag   string `json:"tag"`
		Count string `json:"count"`
	}
	c.ok("GET", "/explore/trends", nil, &trends)
	if len(trends) != 1 || trends[0].Tag != "#OOTD" || trends[0].Count != "1 wear" {
		t.Errorf("trends: %+v", trends)
	}

	// the brand can amplify it to its real followers
	fan := e.shopper()
	fan.ok("PUT", "/brands/"+owner.brandHandle+"/follow", nil, nil)
	var notes []struct {
		ID     string `json:"id"`
		Action struct {
			Type   string `json:"type"`
			Target string `json:"target"`
		} `json:"action"`
	}
	owner.ok("GET", "/studio/notifications", nil, &notes)
	if len(notes) == 0 || notes[0].Action.Type != "amplify" || notes[0].Action.Target != id {
		t.Fatalf("brand notification: %+v", notes)
	}
	var amp struct {
		Reach int `json:"reach"`
	}
	owner.ok("POST", "/studio/notifications/"+notes[0].ID+"/action", nil, &amp)
	if amp.Reach != 1 {
		t.Errorf("reach %d: should be the brand's real follower count", amp.Reach)
	}
	var brand struct {
		Posts []struct {
			ID string `json:"id"`
		} `json:"posts"`
		WearsCount int `json:"wearsCount"`
	}
	fan.ok("GET", "/brands/"+owner.brandHandle, nil, &brand)
	if len(brand.Posts) != 1 || brand.Posts[0].ID != id || brand.WearsCount != 1 {
		t.Errorf("brand page after amplify: %+v", brand)
	}
	var wearer []struct {
		Text string `json:"text"`
	}
	c.ok("GET", "/notifications", nil, &wearer)
	if len(wearer) == 0 || !strings.Contains(wearer[0].Text, "featured your wear") {
		t.Errorf("wearer wasn't told: %+v", wearer)
	}
	// following the brand puts the amplified wear ahead of unrelated posts
	var feed struct {
		Posts []struct {
			ID string `json:"id"`
		} `json:"posts"`
	}
	fan.ok("GET", "/feed", nil, &feed)
	if len(feed.Posts) == 0 || feed.Posts[0].ID != id {
		t.Errorf("feed: %+v", feed.Posts)
	}
	// undoing it
	owner.ok("POST", "/studio/notifications/"+notes[0].ID+"/action", nil, nil)
	fan.ok("GET", "/brands/"+owner.brandHandle, nil, &brand)
	if len(brand.Posts) != 0 {
		t.Errorf("after un-amplify: %+v", brand.Posts)
	}

	t.Run("a delivered order's first wear earns the promised single-use 10% code", func(t *testing.T) {
		res := c.buy(pid, "M")
		owner.deliver(res.OrderIDs[0])
		_, code := post(nil)
		if !strings.HasPrefix(code, "WEAR10-") {
			t.Fatalf("code %q", code)
		}
		_, second := post(nil)
		if second != "" {
			t.Errorf("a second wear of the same order earned %q", second)
		}

		other := e.shopper()
		other.ok("POST", "/cart/items", m{"productId": pid, "size": "M", "color": "Black"}, nil)
		other.status(400, "PUT", "/cart/promo", m{"code": code}) // someone else's code

		c.ok("POST", "/cart/items", m{"productId": pid, "size": "M", "color": "Black"}, nil)
		var cart struct {
			Promo struct {
				Discount int `json:"discount"`
			} `json:"promo"`
		}
		c.ok("PUT", "/cart/promo", m{"code": code}, &cart)
		if cart.Promo.Discount != 1000 {
			t.Errorf("discount %d", cart.Promo.Discount)
		}
		var out checkoutResult
		c.ok("POST", "/checkout", m{"shipping": "standard", "payment": "card"}, &out)

		// wearwhat funds it, so the brand's payout is whole
		var payout int
		if err := e.pool.QueryRow(context.Background(), `SELECT payout FROM orders WHERE checkout_number = $1`, out.Number).Scan(&payout); err != nil {
			t.Fatal(err)
		}
		if payout != 10000+800-800 {
			t.Errorf("payout %d: the brand shouldn't fund a wearwhat reward", payout)
		}

		// and it's gone
		c.ok("POST", "/cart/items", m{"productId": pid, "size": "M", "color": "Black"}, nil)
		c.status(400, "PUT", "/cart/promo", m{"code": code})
	})
}

func TestDropsAndScheduledPosts(t *testing.T) {
	e := newEnv(t)
	owner := e.brandOwner()
	fan := e.shopper()
	fan.ok("PUT", "/brands/"+owner.brandHandle+"/follow", nil, nil)

	var overview struct {
		NextDrop *struct {
			Name, Title string
			Pieces      int
			NotifyCount int
		} `json:"nextDrop"`
	}
	owner.ok("GET", "/studio/promotions", nil, &overview)
	if overview.NextDrop != nil {
		t.Fatalf("a new brand has no drop: %+v", overview.NextDrop)
	}
	// pieces can't join a drop that doesn't exist
	owner.status(400, "POST", "/studio/products", m{"name": "x", "price": 1000, "category": "Tops", "colors": []string{"Black"}, "sizes": []string{"M"}, "sizeStock": m{"M": 1}, "live": true, "inDrop": true})
	owner.status(400, "PUT", "/studio/drops/next", m{"name": "Past", "releasesAt": time.Now().Add(-time.Hour).Format(time.RFC3339)})
	owner.status(400, "PUT", "/studio/drops/next", m{"name": "", "releasesAt": time.Now().Add(time.Hour).Format(time.RFC3339)})
	owner.ok("PUT", "/studio/drops/next", m{"name": "Drop 01", "releasesAt": time.Now().Add(2 * time.Hour).Format(time.RFC3339)}, nil)

	var prod product
	owner.ok("POST", "/studio/products", m{"name": "Held back", "price": 1000, "category": "Tops", "colors": []string{"Black"}, "sizes": []string{"M"}, "sizeStock": m{"M": 5}, "live": true, "inDrop": true}, &prod)
	owner.ok("GET", "/studio/promotions", nil, &overview)
	if overview.NextDrop == nil || overview.NextDrop.Pieces != 1 || overview.NextDrop.NotifyCount != 1 || !strings.Contains(overview.NextDrop.Title, "Drop 01 · ") {
		t.Fatalf("next drop: %+v", overview.NextDrop)
	}

	u := owner.upload("image", pngBytes(t))
	var tile struct {
		ID string `json:"id"`
	}
	owner.ok("POST", "/studio/media", m{"url": u.URL}, &tile)
	var teaser, tomorrow struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	owner.ok("POST", "/studio/posts", m{"type": "Post", "mediaIds": []string{tile.ID}, "productIds": []string{prod.ID}, "caption": "teaser", "when": "With next drop"}, &teaser)
	owner.ok("POST", "/studio/posts", m{"type": "Post", "mediaIds": []string{tile.ID}, "productIds": []string{}, "caption": "tomorrow", "when": "Tomorrow 09:00"}, &tomorrow)
	if teaser.Status != "scheduled" || tomorrow.Status != "scheduled" {
		t.Fatalf("%+v %+v", teaser, tomorrow)
	}
	owner.status(400, "POST", "/studio/posts", m{"type": "Post", "mediaIds": []string{tile.ID}, "productIds": []string{}, "when": "Whenever"})
	owner.status(400, "POST", "/studio/posts", m{"type": "Reel", "mediaIds": []string{tile.ID}, "productIds": []string{}, "when": "Now"})

	// before release: invisible to shoppers, visible to the owner
	fan.status(404, "GET", "/products/"+prod.ID, nil)
	fan.status(404, "POST", "/cart/items", m{"productId": prod.ID, "size": "M", "color": "Black"})
	fan.status(404, "GET", "/posts/"+teaser.ID, nil)
	var brand struct {
		Shop []struct {
			Product struct {
				ID string `json:"id"`
			} `json:"product"`
		} `json:"shop"`
		PostsCount int `json:"postsCount"`
	}
	fan.ok("GET", "/brands/"+owner.brandHandle, nil, &brand)
	if len(brand.Shop) != 0 || brand.PostsCount != 0 {
		t.Fatalf("shopper sees the unreleased drop: %+v", brand)
	}
	owner.ok("GET", "/brands/"+owner.brandHandle, nil, &brand)
	if len(brand.Shop) != 1 {
		t.Errorf("owner's preview: %+v", brand.Shop)
	}
	e.server.Jobs.Tick(context.Background())
	fan.status(404, "GET", "/products/"+prod.ID, nil)

	// release time
	e.clock.advance(3 * time.Hour)
	e.server.Jobs.Tick(context.Background())
	fan.ok("GET", "/products/"+prod.ID, nil, nil)
	fan.ok("GET", "/posts/"+teaser.ID, nil, nil)
	fan.status(404, "GET", "/posts/"+tomorrow.ID, nil) // that one is still waiting
	var notes []struct {
		Text string `json:"text"`
		Kind string `json:"kind"`
	}
	fan.ok("GET", "/notifications", nil, &notes)
	if len(notes) != 1 || !strings.Contains(notes[0].Text, "dropped Drop 01") || notes[0].Kind != "Drops" {
		t.Fatalf("follower notifications: %+v", notes)
	}
	// running the job again doesn't re-announce
	e.server.Jobs.Tick(context.Background())
	fan.ok("GET", "/notifications", nil, &notes)
	if len(notes) != 1 {
		t.Errorf("announced %d times", len(notes))
	}

	e.clock.advance(48 * time.Hour)
	e.server.Jobs.Tick(context.Background())
	fan.ok("GET", "/posts/"+tomorrow.ID, nil, nil)

	t.Run("notification preferences are respected", func(t *testing.T) {
		quiet := e.shopper()
		quiet.ok("PUT", "/brands/"+owner.brandHandle+"/follow", nil, nil)
		quiet.ok("PATCH", "/me", m{"prefs": m{"drops": false}}, nil)
		owner.ok("PUT", "/studio/drops/next", m{"name": "Drop 02", "releasesAt": e.clock.now().Add(time.Hour).Format(time.RFC3339)}, nil)
		e.clock.advance(2 * time.Hour)
		e.server.Jobs.Tick(context.Background())
		var got []any
		quiet.ok("GET", "/notifications", nil, &got)
		if len(got) != 0 {
			t.Errorf("drops alerts are off but got %d", len(got))
		}
		fan.ok("GET", "/notifications", nil, &notes)
		if len(notes) != 2 {
			t.Errorf("a follower with alerts on should get it: %d", len(notes))
		}
	})

	t.Run("stories expire after a day", func(t *testing.T) {
		var story struct {
			ID string `json:"id"`
		}
		owner.ok("POST", "/studio/posts", m{"type": "Story", "mediaIds": []string{tile.ID}, "productIds": []string{}, "when": "Now"}, &story)
		// expiry compares against the database clock, so this checks the stored time
		var hours float64
		if err := e.pool.QueryRow(context.Background(), `SELECT EXTRACT(EPOCH FROM expires_at - published_at) / 3600 FROM posts WHERE id = $1`, story.ID).Scan(&hours); err != nil {
			t.Fatal(err)
		}
		if hours != 24 {
			t.Errorf("story lasts %v hours", hours)
		}
	})
}

func TestBrandVerificationGate(t *testing.T) {
	e := newEnv(t, func(o *options) { o.env = "production" })
	// production has no devCode, so read the code from the email
	signUp := func() *client {
		c := e.anon()
		c.email = "owner" + string(rune('a'+e.next())) + "@test.dev"
		c.password = "longenough"
		c.ok("POST", "/auth/signup", m{"method": "Email", "kind": "brand", "email": c.email, "password": c.password}, nil)
		body := e.mailTo(c.email, "code")
		code := body[strings.Index(body, "code is ")+8 : strings.Index(body, "code is ")+14]
		c.ok("POST", "/auth/verify", m{"code": code}, nil)
		return c
	}
	owner := signUp()
	owner.onboard()
	pid := owner.addProduct(5000, map[string]int{"M": 5})
	var me struct {
		Brand struct {
			Status string `json:"status"`
		} `json:"brand"`
	}
	owner.ok("GET", "/me", nil, &me)
	if me.Brand.Status != "in_review" {
		t.Fatalf("status %q: production brands start in review", me.Brand.Status)
	}
	var settings struct {
		Verified bool `json:"verified"`
	}
	owner.ok("GET", "/studio/settings", nil, &settings)
	if settings.Verified {
		t.Fatal("not verified yet")
	}

	shopper := signUp()
	// hidden from shoppers while in review...
	shopper.status(404, "GET", "/brands/"+owner.brandHandle, nil)
	shopper.status(404, "GET", "/products/"+pid, nil)
	shopper.status(404, "POST", "/cart/items", m{"productId": pid, "size": "M", "color": "Black"})
	shopper.status(403, "POST", "/threads", m{"brandId": owner.brandID})
	var brands []any
	shopper.ok("GET", "/brands", nil, &brands)
	if len(brands) != 0 {
		t.Errorf("listed: %d", len(brands))
	}
	// ...but the owner can keep setting up
	owner.ok("GET", "/brands/"+owner.brandHandle, nil, nil)
	owner.ok("GET", "/products/"+pid, nil, nil)

	if _, err := e.pool.Exec(context.Background(), `UPDATE brands SET status = 'verified', verified_at = now() WHERE id = $1`, owner.brandID); err != nil {
		t.Fatal(err)
	}
	shopper.ok("GET", "/brands/"+owner.brandHandle, nil, nil)
	shopper.ok("GET", "/products/"+pid, nil, nil)
	owner.ok("GET", "/studio/settings", nil, &settings)
	if !settings.Verified {
		t.Error("should be verified")
	}
	if msg := owner.status(409, "POST", "/studio/onboarding", m{}); !strings.Contains(msg, "already") {
		t.Errorf("second brand: %q", msg)
	}
}

func TestExploreFilters(t *testing.T) {
	// the rules from wearwhat/src/lib/explore.ts, now applied in SQL
	e := newEnv(t)
	owner := e.brandOwner()
	add := func(name string, price int, category, color string, stock map[string]int, flat bool) string {
		sizes := []string{}
		for z := range stock {
			sizes = append(sizes, z)
		}
		var flatLay any
		if flat {
			flatLay = owner.upload("image", pngBytes(t)).URL
		}
		var p product
		owner.ok("POST", "/studio/products", m{"name": name, "price": price, "category": category, "colors": []string{color}, "sizes": sizes, "sizeStock": stock, "images": []string{}, "flatLay": flatLay, "live": true}, &p)
		return p.ID
	}
	jacket := add("Denim jacket", 15000, "Outerwear", "Denim", map[string]int{"M": 2, "L": 0}, true)
	tee := add("Black tee", 5000, "Tops", "Black", map[string]int{"S": 3}, false)
	coat := add("Wool coat", 25000, "Outerwear", "Grey", map[string]int{"L": 1}, false)

	c := e.shopper()
	ids := func(query string) []string {
		t.Helper()
		var res struct {
			Items []struct {
				Product struct {
					ID string `json:"id"`
				} `json:"product"`
			} `json:"items"`
		}
		c.ok("GET", "/explore"+query, nil, &res)
		out := []string{}
		for _, item := range res.Items {
			out = append(out, item.Product.ID)
		}
		return out
	}
	same := func(got []string, want ...string) bool {
		if len(got) != len(want) {
			return false
		}
		seen := map[string]bool{}
		for _, g := range got {
			seen[g] = true
		}
		for _, w := range want {
			if !seen[w] {
				return false
			}
		}
		return true
	}
	cases := []struct {
		query string
		want  []string
	}{
		{"", []string{jacket, tee, coat}},
		{"?category=Outerwear", []string{jacket, coat}},
		{"?color=Black", []string{tee}},
		{"?price=lt100", []string{tee}},
		{"?price=100to200", []string{jacket}},
		{"?price=gt200", []string{coat}},
		{"?tryOn=ready", []string{jacket}},
		{"?size=M", []string{jacket}},
		{"?size=L", []string{coat}},
		{"?size=S", []string{tee}},
		{"?q=denim", []string{jacket}},
		{"?q=COAT", []string{coat}},
		{"?q=outerwear", []string{jacket, coat}},
		{"?category=Outerwear&price=gt200", []string{coat}},
		{"?q=nothing-matches", []string{}},
		{"?q=%25", []string{}}, // a literal %, not a wildcard
	}
	for _, x := range cases {
		if got := ids(x.query); !same(got, x.want...) {
			t.Errorf("explore%s: got %v, want %v", x.query, got, x.want)
		}
	}
	if got := ids("?sort=price-asc"); got[0] != tee || got[2] != coat {
		t.Errorf("price-asc: %v", got)
	}
	if got := ids("?sort=price-desc"); got[0] != coat || got[2] != tee {
		t.Errorf("price-desc: %v", got)
	}
	// a "#tag" search finds the posts using it, and matches case-insensitively
	u := c.upload("image", pngBytes(t))
	var wear struct {
		ID string `json:"id"`
	}
	c.ok("POST", "/wears", m{"imageUrl": u.URL, "productId": tee, "fit": "True to size", "caption": "x", "hashtags": []string{"#Tagged"}}, &wear)
	var tagged struct {
		Items []struct {
			PostID string `json:"postId"`
			Chip   string `json:"chip"`
		} `json:"items"`
	}
	c.ok("GET", "/explore?q=%23tagged", nil, &tagged)
	if len(tagged.Items) != 1 || tagged.Items[0].PostID != wear.ID || !strings.HasPrefix(tagged.Items[0].Chip, "WEAR · @") {
		t.Errorf("hashtag search: %+v", tagged.Items)
	}
	c.ok("GET", "/explore?q=%23nothing", nil, &tagged)
	if len(tagged.Items) != 0 {
		t.Errorf("unused hashtag: %+v", tagged.Items)
	}

	// injection attempts are just unusual search text
	for _, q := range []string{"?q='; DROP TABLE users; --", "?category=' OR 1=1 --"} {
		status, _ := c.raw("GET", "/explore"+strings.ReplaceAll(q, " ", "%20"), nil)
		if status != 200 {
			t.Errorf("%s: %d", q, status)
		}
	}
	if n := e.count(`SELECT count(*) FROM users`); n == 0 {
		t.Error("users table gone")
	}
}

func TestMetricsComputedFromRealActivity(t *testing.T) {
	e := newEnv(t)
	owner := e.brandOwner()
	pid := owner.addProduct(10000, map[string]int{"M": 50})
	u := owner.upload("image", pngBytes(t))
	var tile struct{ ID string }
	owner.ok("POST", "/studio/media", m{"url": u.URL}, &tile)
	var post struct {
		ID string `json:"id"`
	}
	owner.ok("POST", "/studio/posts", m{"type": "Post", "mediaIds": []string{tile.ID}, "productIds": []string{pid}, "caption": "A campaign post", "when": "Now"}, &post)

	type metrics struct {
		Range string `json:"range"`
		GMV   struct {
			Value int    `json:"value"`
			Delta string `json:"delta"`
		} `json:"gmv"`
		Orders struct {
			Value int `json:"value"`
		} `json:"orders"`
		Conversion struct {
			Value string `json:"value"`
		} `json:"conversion"`
		TryOnLift struct {
			Value string `json:"value"`
		} `json:"tryOnLift"`
		Funnel []struct {
			Label string  `json:"label"`
			Value int     `json:"value"`
			Pct   float64 `json:"pct"`
		} `json:"funnel"`
		MissingAssets int   `json:"missingAssets"`
		Bars          []int `json:"bars"`
		TopPosts      []struct {
			ID    string `json:"id"`
			Value int    `json:"value"`
			Meta  string `json:"meta"`
		} `json:"topPosts"`
	}
	var zero metrics
	owner.ok("GET", "/studio/metrics", nil, &zero)
	if zero.GMV.Value != 0 || zero.Orders.Value != 0 || zero.Conversion.Value != "—" || zero.TryOnLift.Value != "—" || len(zero.Bars) != 14 {
		t.Errorf("a new brand shows invented numbers: %+v", zero)
	}
	if zero.MissingAssets != 1 {
		t.Errorf("missing assets %d", zero.MissingAssets)
	}

	// someone tries it on, then buys via a post
	c := e.shopper().withAddress()
	c.ok("POST", "/events", m{"type": "product_view", "productId": pid}, nil)
	c.ok("POST", "/events", m{"type": "product_view", "productId": pid}, nil) // repeat view isn't double-counted
	c.ok("POST", "/events", m{"type": "try_on", "productId": pid}, nil)
	c.ok("POST", "/events", m{"type": "post_view", "postId": post.ID}, nil)
	c.status(400, "POST", "/events", m{"type": "made_up", "productId": pid})
	c.ok("POST", "/events", m{"type": "try_on", "productId": "missing"}, nil) // quietly ignored
	c.ok("POST", "/cart/items", m{"productId": pid, "size": "M", "color": "Black", "postId": post.ID}, nil)
	var res checkoutResult
	c.ok("POST", "/checkout", m{"shipping": "standard", "payment": "card"}, &res)

	var so struct {
		FromTryOn bool `json:"fromTryOn"`
	}
	owner.ok("GET", "/studio/orders/"+studioID(res.OrderIDs[0]), nil, &so)
	if !so.FromTryOn {
		t.Error("a purchase after a try-on should say so")
	}

	var after metrics
	owner.ok("GET", "/studio/metrics?period=7D", nil, &after)
	if after.GMV.Value != 10000 || after.Orders.Value != 1 {
		t.Errorf("sales: %+v %+v", after.GMV, after.Orders)
	}
	if after.GMV.Delta != "New this period" {
		t.Errorf("delta %q", after.GMV.Delta)
	}
	if after.Conversion.Value != "100.0%" {
		t.Errorf("1 order from 1 view: %q", after.Conversion.Value)
	}
	if after.Funnel[0].Value != 1 || after.Funnel[1].Value != 1 || after.Funnel[2].Value != 1 || after.Funnel[2].Pct != 100 {
		t.Errorf("funnel: %+v", after.Funnel)
	}
	if len(after.TopPosts) != 1 || after.TopPosts[0].ID != post.ID || after.TopPosts[0].Value != 10000 || !strings.Contains(after.TopPosts[0].Meta, "1 views") {
		t.Errorf("top posts: %+v", after.TopPosts)
	}
	tallest := 0
	for _, b := range after.Bars {
		if b > tallest {
			tallest = b
		}
	}
	if tallest != 100 {
		t.Errorf("bars: %v", after.Bars)
	}

	// a returned sale stops counting
	owner.deliver(res.OrderIDs[0])
	c.ok("POST", "/orders/"+res.OrderIDs[0]+"/return", m{"note": "no"}, nil)
	owner.ok("PATCH", "/studio/orders/"+studioID(res.OrderIDs[0]), m{"status": "RETURN APPROVED"}, nil)
	owner.ok("GET", "/studio/metrics?period=7D", nil, &after)
	if after.GMV.Value != 0 {
		t.Errorf("GMV after an approved return: %d", after.GMV.Value)
	}
}

func TestSeedLoadsAndIsIdempotent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := seed.IfEmpty(ctx, e.pool, time.UTC, e.deps.Cfg.DataKey); err != nil {
		t.Fatal(err)
	}
	orders := e.count(`SELECT count(*) FROM orders`)
	if orders < 100 {
		t.Errorf("only %d orders", orders)
	}
	if err := seed.IfEmpty(ctx, e.pool, time.UTC, e.deps.Cfg.DataKey); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*) FROM orders`); n != orders {
		t.Errorf("a second run changed orders: %d → %d", orders, n)
	}

	amara := e.anon()
	amara.ok("POST", "/auth/login", m{"email": "amara@kalu.co", "password": seed.DemoPassword}, nil)
	var me struct {
		Handle string `json:"handle"`
		Brand  struct {
			Handle string `json:"handle"`
			Role   string `json:"role"`
		} `json:"brand"`
		OrdersCount int `json:"ordersCount"`
	}
	amara.ok("GET", "/me", nil, &me)
	if me.Handle != "amara.k" || me.Brand.Handle != "highfashion" || me.Brand.Role != "OWNER" || me.OrdersCount != 3 {
		t.Errorf("Amara: %+v", me)
	}

	var metrics struct {
		GMV struct {
			Value int `json:"value"`
		} `json:"gmv"`
		Conversion struct {
			Value string `json:"value"`
		} `json:"conversion"`
		TryOnLift struct {
			Value string `json:"value"`
		} `json:"tryOnLift"`
		TopPosts []any `json:"topPosts"`
	}
	amara.ok("GET", "/studio/metrics", nil, &metrics)
	if metrics.GMV.Value == 0 || metrics.Conversion.Value == "—" || metrics.TryOnLift.Value == "—" || len(metrics.TopPosts) == 0 {
		t.Errorf("the seeded dashboard should have real numbers: %+v", metrics)
	}

	// the demo really works end to end, from both sides of a conversation
	tobi := e.anon()
	tobi.ok("POST", "/auth/login", m{"email": "tobi@wearwhat.test", "password": seed.DemoPassword}, nil)
	var threads []struct {
		ID, Name string
	}
	tobi.ok("GET", "/threads", nil, &threads)
	var hf string
	for _, th := range threads {
		if th.Name == "HIGHFASHION" {
			hf = th.ID
		}
	}
	if hf == "" {
		t.Fatalf("Tobi's threads: %+v", threads)
	}
	tobi.ok("POST", "/threads/"+hf+"/messages", m{"text": "Quick question"}, nil)
	var view struct {
		Messages []struct {
			Text string `json:"text"`
		} `json:"messages"`
	}
	amara.ok("GET", "/studio/threads/"+hf, nil, &view)
	if view.Messages[len(view.Messages)-1].Text != "Quick question" {
		t.Errorf("Amara's studio inbox: %+v", view.Messages)
	}

	// and a demo checkout goes through against seeded stock
	var cart struct {
		Lines []any `json:"lines"`
	}
	amara.ok("GET", "/cart", nil, &cart)
	if len(cart.Lines) != 2 {
		t.Fatalf("Amara's bag: %+v", cart)
	}
	amara.ok("POST", "/checkout", m{"shipping": "standard", "payment": "apple_pay"}, nil)
}
