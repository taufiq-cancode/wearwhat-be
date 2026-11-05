// Package domain holds the API's JSON shapes and pure business rules.
// Field names are the JSON field names in wearwhat/src/types/index.ts.
package domain

type Photo struct {
	URL string `json:"url"`
	Fit string `json:"fit,omitempty"`
	BG  string `json:"bg,omitempty"`
	// Kind is "video" for reel media; images leave it empty.
	Kind   string  `json:"kind,omitempty"`
	Aspect float64 `json:"aspect,omitempty"`
}

type BrandRef struct {
	ID     string `json:"id"`
	Handle string `json:"handle"`
	Name   string `json:"name"`
}

type ProductSummary struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Brand        BrandRef `json:"brand"`
	Price        int      `json:"price"`
	Currency     string   `json:"currency"`
	Image        *Photo   `json:"image"`
	Placeholder  string   `json:"placeholder,omitempty"`
	Category     string   `json:"category"`
	TryOnReady   bool     `json:"tryOnReady"`
	DefaultSize  string   `json:"defaultSize"`
	DefaultColor string   `json:"defaultColor"`
}

type SizeOption struct {
	Label     string `json:"label"`
	Available bool   `json:"available"`
}

type ColorOption struct {
	Name string `json:"name"`
	Hex  string `json:"hex"`
}

type Review struct {
	ID     string `json:"id"`
	Author string `json:"author"`
	Meta   string `json:"meta"`
	Text   string `json:"text"`
	Rating int    `json:"rating"`
}

type WornBy struct {
	Handle string `json:"handle"`
	Fit    string `json:"fit"`
	Image  *Photo `json:"image"`
	PostID string `json:"postId"`
}

type FitSummary struct {
	Summary  string `json:"summary"`
	Wears    int    `json:"wears"`
	Position int    `json:"position"`
}

type RatingSummary struct {
	Average float64 `json:"average"`
	Count   int     `json:"count"`
}

type Product struct {
	ProductSummary
	Description     string        `json:"description"`
	Images          []Photo       `json:"images"`
	FlatLay         *Photo        `json:"flatLay"`
	Colors          []ColorOption `json:"colors"`
	Sizes           []SizeOption  `json:"sizes"`
	RecommendedSize string        `json:"recommendedSize"`
	Fit             FitSummary    `json:"fit"`
	Rating          RatingSummary `json:"rating"`
	Reviews         []Review      `json:"reviews"`
	WornBy          []WornBy      `json:"wornBy"`
	Saved           bool          `json:"saved"`
	ShipsFrom       string        `json:"shipsFrom"`
	Processing      string        `json:"processing"`
	Returns         string        `json:"returns"`
	CanReview       string        `json:"canReviewOrderId,omitempty"`
}

/* ---------- feed & social ---------- */

type Author struct {
	Kind     string `json:"kind"`
	Handle   string `json:"handle"`
	Name     string `json:"name"`
	Avatar   *Photo `json:"avatar"`
	Verified bool   `json:"verified"`
}

type PostTag struct {
	Label     string  `json:"label"`
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	ProductID string  `json:"productId"`
}

type PostWear struct {
	Product ProductSummary `json:"product"`
	Meta    string         `json:"meta"`
}

type Post struct {
	ID            string           `json:"id"`
	Author        Author           `json:"author"`
	Subtitle      string           `json:"subtitle"`
	Image         *Photo           `json:"image"`
	Media         []Photo          `json:"media"`
	Aspect        float64          `json:"aspect"`
	Caption       string           `json:"caption"`
	Likes         int              `json:"likes"`
	Liked         bool             `json:"liked"`
	Saved         bool             `json:"saved"`
	CommentsCount int              `json:"commentsCount"`
	Wear          *PostWear        `json:"wear,omitempty"`
	Tags          []PostTag        `json:"tags"`
	Shop          []ProductSummary `json:"shop"`
	Mine          bool             `json:"mine"`
}

type DropBubble struct {
	Brand BrandRef `json:"brand"`
	Image *Photo   `json:"image"`
	IsNew bool     `json:"isNew"`
}

type Feed struct {
	Drops []DropBubble `json:"drops"`
	Posts []Post       `json:"posts"`
}

type ExploreItem struct {
	ID      string         `json:"id"`
	Product ProductSummary `json:"product"`
	Image   *Photo         `json:"image"`
	Chip    string         `json:"chip"`
	Aspect  float64        `json:"aspect"`
	PostID  string         `json:"postId,omitempty"`
}

type Trend struct {
	Tag   string `json:"tag"`
	Count string `json:"count"`
	Image *Photo `json:"image"`
}

type GridTile struct {
	ID    string `json:"id"`
	Image *Photo `json:"image"`
	Badge string `json:"badge,omitempty"`
}

type BrandCard struct {
	BrandRef
	Initials    string `json:"initials"`
	Avatar      *Photo `json:"avatar"`
	Description string `json:"description"`
}

type BrandShopItem struct {
	Product ProductSummary `json:"product"`
	Badge   string         `json:"badge,omitempty"`
}

type Brand struct {
	BrandRef
	Initials   string          `json:"initials"`
	Verified   bool            `json:"verified"`
	Avatar     *Photo          `json:"avatar"`
	Followers  int             `json:"followers"`
	Following  bool            `json:"following"`
	PostsCount int             `json:"postsCount"`
	WearsCount int             `json:"wearsCount"`
	Meta       string          `json:"meta"`
	Bio        string          `json:"bio"`
	Banner     string          `json:"banner,omitempty"`
	CanMessage bool            `json:"canMessage"`
	Shop       []BrandShopItem `json:"shop"`
	Posts      []GridTile      `json:"posts"`
}

type Comment struct {
	ID        string `json:"id"`
	Author    Author `json:"author"`
	Text      string `json:"text"`
	Time      string `json:"time"`
	CanDelete bool   `json:"canDelete"`
}

type UserProfile struct {
	ID         string     `json:"id"`
	Handle     string     `json:"handle"`
	Name       string     `json:"name"`
	Bio        string     `json:"bio"`
	Avatar     *Photo     `json:"avatar"`
	Followers  int        `json:"followers"`
	Following  int        `json:"following"`
	WearsCount int        `json:"wearsCount"`
	IsFollowed bool       `json:"isFollowed"`
	Requested  bool       `json:"requested"`
	FollowsYou bool       `json:"followsYou"`
	Locked     bool       `json:"locked"`
	IsMe       bool       `json:"isMe"`
	Wears      []GridTile `json:"wears"`
}

type BlockedUser struct {
	ID     string `json:"id"`
	Handle string `json:"handle"`
	Name   string `json:"name"`
	Avatar *Photo `json:"avatar"`
}

/* ---------- account ---------- */

type Address struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name"`
	Line1     string `json:"line1"`
	City      string `json:"city"`
	Phone     string `json:"phone,omitempty"`
	IsDefault bool   `json:"isDefault,omitempty"`
}

type Prefs map[string]bool

type FitProfile struct {
	Height *string `json:"height"`
	Top    *string `json:"top"`
	Bottom *string `json:"bottom"`
	Shoe   *string `json:"shoe"`
}

type MeBrand struct {
	BrandRef
	Initials string `json:"initials"`
	Role     string `json:"role"`
	Status   string `json:"status"`
}

type Me struct {
	ID           string     `json:"id"`
	Handle       string     `json:"handle"`
	Name         string     `json:"name"`
	Email        string     `json:"email"`
	Phone        string     `json:"phone"`
	Bio          string     `json:"bio"`
	Avatar       *Photo     `json:"avatar"`
	Followers    int        `json:"followers"`
	Following    int        `json:"following"`
	WearsCount   int        `json:"wearsCount"`
	SavedCount   int        `json:"savedCount"`
	OrdersCount  int        `json:"ordersCount"`
	Brand        *MeBrand   `json:"brand"`
	Prefs        Prefs      `json:"prefs"`
	Fit          FitProfile `json:"fit"`
	Styles       []string   `json:"styles"`
	ShopFor      string     `json:"shopFor"`
	Address      *Address   `json:"address"`
	PaymentLabel string     `json:"paymentLabel"`
}

type PaymentMethod struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Detail    string `json:"detail"`
	IsDefault bool   `json:"isDefault"`
}

/* ---------- commerce ---------- */

type CartLine struct {
	ID      string         `json:"id"`
	Product ProductSummary `json:"product"`
	Size    string         `json:"size"`
	Color   string         `json:"color"`
	Qty     int            `json:"qty"`
}

type CartPromo struct {
	Code         string `json:"code"`
	Label        string `json:"label"`
	Discount     int    `json:"discount"`
	FreeShipping bool   `json:"freeShipping"`
}

type Cart struct {
	Lines []CartLine `json:"lines"`
	Promo *CartPromo `json:"promo"`
}

type Checkout struct {
	Number      string   `json:"number"`
	Total       int      `json:"total"`
	Email       string   `json:"email"`
	OrderIDs    []string `json:"orderIds"`
	Status      string   `json:"status"`
	RedirectURL string   `json:"redirectUrl,omitempty"`
	TestMode    bool     `json:"testMode,omitempty"`
}

type ShopperOrder struct {
	ID           string         `json:"id"`
	Product      ProductSummary `json:"product"`
	Size         string         `json:"size"`
	Color        string         `json:"color"`
	Qty          int            `json:"qty"`
	Status       string         `json:"status"`
	When         string         `json:"when"`
	ReturnStatus string         `json:"returnStatus,omitempty"`
	CanReturn    bool           `json:"canReturn"`
	CanReview    bool           `json:"canReview"`
}

type TrackingEvent struct {
	Title   string `json:"title"`
	Detail  string `json:"detail"`
	Time    string `json:"time"`
	Done    bool   `json:"done"`
	Current bool   `json:"current"`
}

type ShopperOrderDetail struct {
	ShopperOrder
	Stage        int             `json:"stage"`
	Headline     string          `json:"headline"`
	Subline      string          `json:"subline"`
	Carrier      string          `json:"carrier"`
	TrackingCode *string         `json:"trackingCode"`
	Events       []TrackingEvent `json:"events"`
	ShipTo       Address         `json:"shipTo"`
	Total        int             `json:"total"`
}

/* ---------- messages & notifications ---------- */

type Message struct {
	ID     string `json:"id"`
	FromMe bool   `json:"fromMe"`
	Text   string `json:"text"`
	Image  *Photo `json:"image,omitempty"`
	Time   string `json:"time"`
}

type ThreadSummary struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Kind     string          `json:"kind"`
	Verified bool            `json:"verified"`
	Avatar   *Photo          `json:"avatar"`
	Unread   int             `json:"unread"`
	Product  *ProductSummary `json:"product"`
	Status   string          `json:"status"`
	Last     *Message        `json:"last"`
	Handle   string          `json:"handle,omitempty"`
}

type Thread struct {
	ThreadSummary
	Messages []Message `json:"messages"`
}

type NotificationAction struct {
	Type      string `json:"type"`
	Label     string `json:"label"`
	DoneLabel string `json:"doneLabel,omitempty"`
	Done      bool   `json:"done"`
	Target    string `json:"target,omitempty"`
}

type AppNotification struct {
	ID     string              `json:"id"`
	Group  string              `json:"group"`
	Who    string              `json:"who"`
	Text   string              `json:"text"`
	Time   string              `json:"time"`
	Kind   string              `json:"kind"`
	Avatar *Photo              `json:"avatar"`
	Thumb  *Photo              `json:"thumb"`
	Href   string              `json:"href,omitempty"`
	Action *NotificationAction `json:"action,omitempty"`
	Unread bool                `json:"unread"`
}

/* ---------- studio ---------- */

type Customer struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Handle string `json:"handle"`
}

type StudioOrder struct {
	ID        string         `json:"id"`
	Number    string         `json:"number"`
	Customer  Customer       `json:"customer"`
	Product   ProductSummary `json:"product"`
	Size      string         `json:"size"`
	Color     string         `json:"color"`
	Qty       int            `json:"qty"`
	Status    string         `json:"status"`
	PlacedAgo string         `json:"placedAgo"`
	FromTryOn bool           `json:"fromTryOn"`
}

type TimelineStep struct {
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Done   bool   `json:"done"`
}

type StudioOrderDetail struct {
	StudioOrder
	Subtotal       int            `json:"subtotal"`
	Discount       int            `json:"discount"`
	Shipping       int            `json:"shipping"`
	Fee            int            `json:"fee"`
	Payout         int            `json:"payout"`
	ShipTo         Address        `json:"shipTo"`
	ShippingSpeed  string         `json:"shippingSpeed"`
	Carrier        string         `json:"carrier"`
	TrackingCode   string         `json:"trackingCode"`
	ReturnNote     string         `json:"returnNote,omitempty"`
	ReturnAge      string         `json:"returnAge,omitempty"`
	CustomerOrders int            `json:"customerOrders"`
	Timeline       []TimelineStep `json:"timeline"`
	BrandName      string         `json:"brandName"`
	ShipsFrom      string         `json:"shipsFrom"`
}

type Metric struct {
	Value any    `json:"value"`
	Delta string `json:"delta"`
}

type FunnelStep struct {
	Label string  `json:"label"`
	Value int     `json:"value"`
	Pct   float64 `json:"pct"`
}

type TopPost struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Meta  string `json:"meta"`
	Value int    `json:"value"`
	Image *Photo `json:"image"`
}

type StudioMetrics struct {
	Period        string       `json:"period"`
	Range         string       `json:"range"`
	GMV           Metric       `json:"gmv"`
	Orders        Metric       `json:"orders"`
	Conversion    Metric       `json:"conversion"`
	TryOnLift     Metric       `json:"tryOnLift"`
	Bars          []int        `json:"bars"`
	Funnel        []FunnelStep `json:"funnel"`
	MissingAssets int          `json:"missingAssets"`
	TopPosts      []TopPost    `json:"topPosts"`
}

type SizeStock struct {
	Label string `json:"label"`
	Stock int    `json:"stock"`
}

type CatalogItem struct {
	Product ProductSummary `json:"product"`
	Stock   int            `json:"stock"`
	Sizes   []SizeStock    `json:"sizes"`
	Status  string         `json:"status"`
	Hidden  bool           `json:"hidden"`
	Sold30d int            `json:"sold30d"`
}

type ProductInput struct {
	Name        string         `json:"name"`
	Price       int            `json:"price"`
	Description string         `json:"description"`
	Category    string         `json:"category"`
	Colors      []string       `json:"colors"`
	Sizes       []string       `json:"sizes"`
	SizeStock   map[string]int `json:"sizeStock"`
	Images      []string       `json:"images"`
	FlatLay     *string        `json:"flatLay"`
	Live        bool           `json:"live"`
	InDrop      bool           `json:"inDrop"`
	TryOnReady  bool           `json:"tryOnReady"`
}

type Promotion struct {
	Code   string `json:"code"`
	Type   string `json:"type"`
	Value  string `json:"value"`
	Scope  string `json:"scope"`
	Length string `json:"length"`
	Uses   int    `json:"uses"`
	Live   bool   `json:"live"`
}

type NextDrop struct {
	ID string `json:"id"`
	// Name is the drop's own name; Title adds when it releases ("Drop 03 · Fri 18:00").
	Name        string `json:"name"`
	Title       string `json:"title"`
	ReleasesAt  string `json:"releasesAt"`
	Pieces      int    `json:"pieces"`
	NotifyCount int    `json:"notifyCount"`
}

type PromotionsOverview struct {
	NextDrop   *NextDrop   `json:"nextDrop"`
	Promotions []Promotion `json:"promotions"`
	Scopes     []string    `json:"scopes"`
}

type StoreSettings struct {
	Name           string   `json:"name"`
	Handle         string   `json:"handle"`
	Categories     []string `json:"categories"`
	Location       string   `json:"location"`
	Bio            string   `json:"bio"`
	LogoURL        *string  `json:"logoUrl"`
	ShowWornBy     bool     `json:"showWornBy"`
	AcceptMessages bool     `json:"acceptMessages"`
}

type PayoutSettings struct {
	Provider    string  `json:"provider"`
	Bank        string  `json:"bank"`
	Account     string  `json:"account"`
	AccountName *string `json:"accountName"`
	Schedule    string  `json:"schedule"`
	Currency    string  `json:"currency"`
}

type ShippingZones struct {
	Lagos         bool `json:"lagos"`
	Nigeria       bool `json:"nigeria"`
	Africa        bool `json:"africa"`
	International bool `json:"international"`
}

type ShippingSettings struct {
	ShipsFrom  string        `json:"shipsFrom"`
	Zones      ShippingZones `json:"zones"`
	Processing string        `json:"processing"`
	Returns    string        `json:"returns"`
}

type TeamMember struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Detail string `json:"detail"`
	Role   string `json:"role"`
}

type StudioSettings struct {
	Store      StoreSettings    `json:"store"`
	Payouts    PayoutSettings   `json:"payouts"`
	Shipping   ShippingSettings `json:"shipping"`
	Team       []TeamMember     `json:"team"`
	Verified   bool             `json:"verified"`
	VerifiedAt string           `json:"verifiedAt,omitempty"`
}
