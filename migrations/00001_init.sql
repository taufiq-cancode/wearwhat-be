-- +goose Up

-- ---------- accounts ----------

CREATE TABLE users (
  id             text PRIMARY KEY,
  handle         text NOT NULL UNIQUE,
  name           text NOT NULL DEFAULT '',
  email          text UNIQUE,
  phone          text UNIQUE,
  password_hash  text,
  apple_sub      text UNIQUE,
  signup_kind    text NOT NULL DEFAULT 'shopper',
  verified_at    timestamptz,
  bio            text NOT NULL DEFAULT '',
  avatar         jsonb,
  followers      integer NOT NULL DEFAULT 0,
  following      integer NOT NULL DEFAULT 0,
  prefs          jsonb NOT NULL DEFAULT '{"drops":true,"orders":true,"digest":false,"private":false,"brandOrders":true,"brandReturns":true,"brandWears":true}',
  fit            jsonb NOT NULL DEFAULT '{}',
  styles         text[] NOT NULL DEFAULT '{}',
  shop_for       text NOT NULL DEFAULT '',
  consents       jsonb NOT NULL DEFAULT '{}',
  last_digest_at timestamptz,
  created_at     timestamptz NOT NULL DEFAULT now()
);

-- only the hash of a session token is stored; pending sessions can only verify or resend
CREATE TABLE sessions (
  token_hash bytea PRIMARY KEY,
  user_id    text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  pending    boolean NOT NULL DEFAULT false,
  expires_at timestamptz NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sessions_user_idx ON sessions(user_id);

CREATE TABLE verification_codes (
  user_id     text PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  code_hash   bytea NOT NULL,
  attempts    integer NOT NULL DEFAULT 0,
  expires_at  timestamptz NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE password_resets (
  token_hash bytea PRIMARY KEY,
  user_id    text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at timestamptz NOT NULL,
  used_at    timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE rate_limits (
  key          text PRIMARY KEY,
  window_start timestamptz NOT NULL,
  hits         integer NOT NULL
);

CREATE TABLE addresses (
  id         text PRIMARY KEY,
  user_id    text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name       text NOT NULL,
  line1      text NOT NULL,
  city       text NOT NULL,
  phone      text NOT NULL DEFAULT '',
  is_default boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX addresses_user_idx ON addresses(user_id);
CREATE UNIQUE INDEX addresses_one_default ON addresses(user_id) WHERE is_default;

-- reusable provider authorisations; never card numbers
CREATE TABLE payment_methods (
  id         text PRIMARY KEY,
  user_id    text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  provider   text NOT NULL,
  label      text NOT NULL,
  detail     text NOT NULL DEFAULT '',
  auth_enc   text NOT NULL DEFAULT '',
  fingerprint text NOT NULL DEFAULT '',
  is_default boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, provider, fingerprint)
);
CREATE UNIQUE INDEX payment_methods_one_default ON payment_methods(user_id) WHERE is_default;

CREATE TABLE follows_user (
  follower_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  followee_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  approved    boolean NOT NULL DEFAULT true, -- false = waiting on a private account's approval
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (follower_id, followee_id)
);
CREATE INDEX follows_user_followee_idx ON follows_user(followee_id);

CREATE TABLE blocks (
  blocker_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  blocked_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (blocker_id, blocked_id)
);
CREATE INDEX blocks_blocked_idx ON blocks(blocked_id);

-- ---------- uploads ----------

CREATE TABLE uploads (
  id           text PRIMARY KEY,
  user_id      text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  kind         text NOT NULL,            -- image | video | document
  content_type text NOT NULL,
  size_bytes   bigint NOT NULL,
  path         text NOT NULL,
  is_public    boolean NOT NULL,
  aspect       double precision,
  created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX uploads_user_idx ON uploads(user_id);

-- ---------- brands ----------

CREATE TABLE brands (
  id              text PRIMARY KEY,
  owner_id        text REFERENCES users(id) ON DELETE SET NULL,
  handle          text NOT NULL UNIQUE,
  name            text NOT NULL,
  initials        text NOT NULL,
  status          text NOT NULL DEFAULT 'in_review',  -- in_review | verified | rejected
  verified_at     timestamptz,
  avatar          jsonb,
  followers       integer NOT NULL DEFAULT 0,
  meta            text NOT NULL DEFAULT '',
  description     text NOT NULL DEFAULT '',
  bio             text NOT NULL DEFAULT '',
  location        text NOT NULL DEFAULT '',
  categories      text[] NOT NULL DEFAULT '{}',
  show_worn_by    boolean NOT NULL DEFAULT true,
  accept_messages boolean NOT NULL DEFAULT true,
  -- provider, bank, schedule, currency, legalName, accountEnc
  payouts         jsonb NOT NULL DEFAULT '{}',
  -- shipsFrom, zones, processing, returns
  shipping        jsonb NOT NULL DEFAULT '{}',
  -- type, legalName, registrationEnc, documentId
  business        jsonb NOT NULL DEFAULT '{}',
  created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE team_members (
  id                text PRIMARY KEY,
  brand_id          text NOT NULL REFERENCES brands(id) ON DELETE CASCADE,
  user_id           text REFERENCES users(id) ON DELETE CASCADE,
  email             text NOT NULL DEFAULT '',
  name              text NOT NULL DEFAULT '',
  role              text NOT NULL,        -- OWNER | Admin | Orders | Content
  status            text NOT NULL,        -- active | invited
  invite_token_hash bytea UNIQUE,
  invite_expires_at timestamptz,
  created_at        timestamptz NOT NULL DEFAULT now(),
  UNIQUE (brand_id, email)
);
CREATE UNIQUE INDEX team_one_brand_per_user ON team_members(user_id) WHERE status = 'active';

CREATE TABLE follows_brand (
  user_id    text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  brand_id   text NOT NULL REFERENCES brands(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, brand_id)
);
CREATE INDEX follows_brand_brand_idx ON follows_brand(brand_id);

CREATE TABLE brand_media (
  id         text PRIMARY KEY,
  brand_id   text NOT NULL REFERENCES brands(id) ON DELETE CASCADE,
  media      jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX brand_media_brand_idx ON brand_media(brand_id, created_at DESC);

CREATE TABLE drops (
  id          text PRIMARY KEY,
  brand_id    text NOT NULL REFERENCES brands(id) ON DELETE CASCADE,
  title       text NOT NULL,
  releases_at timestamptz NOT NULL,
  released_at timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX drops_brand_idx ON drops(brand_id, releases_at DESC);
CREATE INDEX drops_due_idx ON drops(releases_at) WHERE released_at IS NULL;

-- ---------- products ----------

CREATE TABLE products (
  id           text PRIMARY KEY,
  brand_id     text NOT NULL REFERENCES brands(id) ON DELETE CASCADE,
  name         text NOT NULL,
  price        integer NOT NULL CHECK (price >= 0),
  category     text NOT NULL,
  description  text NOT NULL DEFAULT '',
  color        text NOT NULL DEFAULT '',    -- explore filter colour
  colors       jsonb NOT NULL DEFAULT '[]', -- [{name, hex}]
  images       jsonb NOT NULL DEFAULT '[]', -- [Photo]
  flat_lay     jsonb,
  placeholder  text NOT NULL DEFAULT '',
  hidden       boolean NOT NULL DEFAULT false,
  drop_id      text REFERENCES drops(id) ON DELETE SET NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX products_brand_idx ON products(brand_id);

CREATE TABLE product_sizes (
  product_id text NOT NULL REFERENCES products(id) ON DELETE CASCADE,
  label      text NOT NULL,
  stock      integer NOT NULL CHECK (stock >= 0),
  position   integer NOT NULL DEFAULT 0,
  PRIMARY KEY (product_id, label)
);

CREATE TABLE product_saves (
  user_id    text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  product_id text NOT NULL REFERENCES products(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, product_id)
);

CREATE TABLE stock_alerts (
  user_id    text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  product_id text NOT NULL REFERENCES products(id) ON DELETE CASCADE,
  size       text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, product_id, size)
);

CREATE TABLE closet_items (
  user_id    text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  product_id text NOT NULL REFERENCES products(id) ON DELETE CASCADE,
  added_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, product_id)
);

-- ---------- posts ----------

CREATE TABLE posts (
  id              text PRIMARY KEY,
  brand_id        text REFERENCES brands(id) ON DELETE CASCADE,
  user_id         text REFERENCES users(id) ON DELETE CASCADE,
  type            text NOT NULL DEFAULT 'Post',  -- Post | Reel | Story | Wear
  status          text NOT NULL DEFAULT 'published', -- published | scheduled
  publish_at      timestamptz,
  with_next_drop  boolean NOT NULL DEFAULT false,
  expires_at      timestamptz,
  media           jsonb NOT NULL DEFAULT '[]', -- [Photo]
  aspect          double precision NOT NULL DEFAULT 0.8125,
  subtitle        text NOT NULL DEFAULT '',
  caption         text NOT NULL DEFAULT '',
  location        text NOT NULL DEFAULT '',
  likes           integer NOT NULL DEFAULT 0,
  comments_count  integer NOT NULL DEFAULT 0,
  worn_product_id text REFERENCES products(id) ON DELETE SET NULL,
  worn_size       text NOT NULL DEFAULT '',
  worn_fit        text NOT NULL DEFAULT '',
  created_at      timestamptz NOT NULL DEFAULT now(),
  published_at    timestamptz,
  CHECK ((brand_id IS NULL) <> (user_id IS NULL))
);
CREATE INDEX posts_feed_idx ON posts(published_at DESC) WHERE status = 'published';
CREATE INDEX posts_brand_idx ON posts(brand_id, published_at DESC);
CREATE INDEX posts_user_idx ON posts(user_id, published_at DESC);
CREATE INDEX posts_worn_idx ON posts(worn_product_id);
CREATE INDEX posts_scheduled_idx ON posts(publish_at) WHERE status = 'scheduled';

CREATE TABLE post_tags (
  post_id    text NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
  position   integer NOT NULL,
  product_id text NOT NULL REFERENCES products(id) ON DELETE CASCADE,
  label      text NOT NULL,
  x          double precision NOT NULL,
  y          double precision NOT NULL,
  PRIMARY KEY (post_id, position)
);

CREATE TABLE post_shop (
  post_id    text NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
  position   integer NOT NULL,
  product_id text NOT NULL REFERENCES products(id) ON DELETE CASCADE,
  PRIMARY KEY (post_id, position)
);

CREATE TABLE post_hashtags (
  post_id text NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
  tag     text NOT NULL,
  PRIMARY KEY (post_id, tag)
);
CREATE INDEX post_hashtags_tag_idx ON post_hashtags(tag);

CREATE TABLE post_likes (
  user_id    text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  post_id    text NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, post_id)
);

CREATE TABLE post_saves (
  user_id    text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  post_id    text NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, post_id)
);

CREATE TABLE post_amplifications (
  post_id    text NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
  brand_id   text NOT NULL REFERENCES brands(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (post_id, brand_id)
);
CREATE INDEX post_amplifications_brand_idx ON post_amplifications(brand_id);

CREATE TABLE comments (
  id         text PRIMARY KEY,
  post_id    text NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
  user_id    text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  text       text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX comments_post_idx ON comments(post_id, created_at);

CREATE TABLE reports (
  id          text PRIMARY KEY,
  reporter_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  target_kind text NOT NULL,
  target_id   text NOT NULL,
  reason      text NOT NULL DEFAULT '',
  created_at  timestamptz NOT NULL DEFAULT now(),
  resolved_at timestamptz,
  UNIQUE (reporter_id, target_kind, target_id)
);

-- ---------- commerce ----------

CREATE TABLE promotions (
  code       text PRIMARY KEY,
  brand_id   text REFERENCES brands(id) ON DELETE CASCADE, -- null = funded by wearwhat
  user_id    text REFERENCES users(id) ON DELETE CASCADE,  -- set = single-use reward for this person
  type       text NOT NULL,   -- % off | $ off | Free ship
  value      text NOT NULL DEFAULT '',
  scope      text NOT NULL DEFAULT 'Everything',
  length     text NOT NULL DEFAULT '',
  ends_at    timestamptz,
  uses       integer NOT NULL DEFAULT 0,
  max_uses   integer,
  live       boolean NOT NULL DEFAULT true,
  banner     boolean NOT NULL DEFAULT false,
  notify     boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX promotions_brand_idx ON promotions(brand_id, created_at DESC);

CREATE TABLE cart_lines (
  id             text PRIMARY KEY,
  user_id        text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  product_id     text NOT NULL REFERENCES products(id) ON DELETE CASCADE,
  size           text NOT NULL,
  color          text NOT NULL,
  qty            integer NOT NULL CHECK (qty >= 1),
  source_post_id text REFERENCES posts(id) ON DELETE SET NULL,
  created_at     timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, product_id, size, color)
);

CREATE TABLE cart_promos (
  user_id text PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  code    text NOT NULL REFERENCES promotions(code) ON DELETE CASCADE
);

CREATE SEQUENCE checkout_seq START 20501;
CREATE SEQUENCE order_seq START 20501;

CREATE TABLE checkouts (
  number         text PRIMARY KEY,
  user_id        text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  subtotal       integer NOT NULL,
  discount       integer NOT NULL DEFAULT 0,
  shipping       integer NOT NULL,
  total          integer NOT NULL,
  currency       text NOT NULL DEFAULT 'USD',
  charge_amount  bigint NOT NULL,        -- what the provider must collect, in minor units
  charge_currency text NOT NULL,
  status         text NOT NULL,          -- pending | paid | failed | expired | paid_after_expiry | amount_mismatch
  shipping_speed text NOT NULL,
  payment_method text NOT NULL,
  provider       text NOT NULL,
  provider_ref   text UNIQUE,
  redirect_url   text NOT NULL DEFAULT '',
  promo_code     text,
  ship_to        jsonb NOT NULL,
  email          text NOT NULL DEFAULT '',
  created_at     timestamptz NOT NULL DEFAULT now(),
  paid_at        timestamptz,
  expires_at     timestamptz NOT NULL
);
CREATE INDEX checkouts_user_idx ON checkouts(user_id);
CREATE INDEX checkouts_expiring_idx ON checkouts(expires_at) WHERE status = 'pending';

CREATE TABLE orders (
  seq             bigint PRIMARY KEY DEFAULT nextval('order_seq'),
  checkout_number text REFERENCES checkouts(number) ON DELETE SET NULL,
  user_id         text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  brand_id        text NOT NULL REFERENCES brands(id) ON DELETE CASCADE,
  product_id      text NOT NULL REFERENCES products(id) ON DELETE CASCADE,
  size            text NOT NULL,
  color           text NOT NULL,
  qty             integer NOT NULL DEFAULT 1,
  unit_price      integer NOT NULL,
  subtotal        integer NOT NULL,
  discount        integer NOT NULL DEFAULT 0,  -- all discounts on this line
  brand_discount  integer NOT NULL DEFAULT 0,  -- the part the brand funds
  shipping        integer NOT NULL DEFAULT 0,
  fee             integer NOT NULL DEFAULT 0,
  payout          integer NOT NULL DEFAULT 0,
  -- PENDING | TO FULFIL | SHIPPED | OUT FOR DELIVERY | DELIVERED |
  -- RETURN REQUESTED | RETURN APPROVED | RETURN DECLINED | CANCELLED
  status          text NOT NULL,
  carrier         text NOT NULL DEFAULT '',
  tracking_code   text NOT NULL DEFAULT '',
  return_note     text NOT NULL DEFAULT '',
  from_try_on     boolean NOT NULL DEFAULT false,
  source_post_id  text REFERENCES posts(id) ON DELETE SET NULL,
  ship_to         jsonb NOT NULL DEFAULT '{}',
  shipping_speed  text NOT NULL DEFAULT 'standard',
  refund_status   text NOT NULL DEFAULT 'none',  -- none | due | done
  placed_at       timestamptz NOT NULL DEFAULT now(),
  shipped_at      timestamptz,
  delivered_at    timestamptz,
  return_requested_at timestamptz,
  payout_paid_at  timestamptz
);
CREATE INDEX orders_user_idx ON orders(user_id, placed_at DESC);
CREATE INDEX orders_brand_idx ON orders(brand_id, placed_at DESC);
CREATE INDEX orders_checkout_idx ON orders(checkout_number);
CREATE INDEX orders_post_idx ON orders(source_post_id);

CREATE TABLE order_events (
  id         bigserial PRIMARY KEY,
  order_seq  bigint NOT NULL REFERENCES orders(seq) ON DELETE CASCADE,
  stage      text NOT NULL,  -- placed | paid | shipped | out_for_delivery | delivered | return_requested | return_approved | return_declined
  detail     text NOT NULL DEFAULT '',
  at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX order_events_order_idx ON order_events(order_seq, at);

CREATE TABLE reviews (
  id         text PRIMARY KEY,
  product_id text NOT NULL REFERENCES products(id) ON DELETE CASCADE,
  user_id    text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  order_seq  bigint REFERENCES orders(seq) ON DELETE SET NULL UNIQUE,
  rating     integer NOT NULL CHECK (rating BETWEEN 1 AND 5),
  text       text NOT NULL DEFAULT '',
  size       text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX reviews_product_idx ON reviews(product_id, created_at DESC);

CREATE TABLE wear_rewards (
  order_seq bigint PRIMARY KEY REFERENCES orders(seq) ON DELETE CASCADE,
  code      text NOT NULL
);

-- ---------- messages & notifications ----------

-- side a = the shopper (or first person); side b = the brand (or second person)
CREATE TABLE threads (
  id              text PRIMARY KEY,
  kind            text NOT NULL,   -- brand | person
  a_user_id       text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  b_user_id       text REFERENCES users(id) ON DELETE CASCADE,
  brand_id        text REFERENCES brands(id) ON DELETE CASCADE,
  product_id      text REFERENCES products(id) ON DELETE SET NULL,
  a_unread        integer NOT NULL DEFAULT 0,
  b_unread        integer NOT NULL DEFAULT 0,
  last_message_at timestamptz NOT NULL DEFAULT now(),
  created_at      timestamptz NOT NULL DEFAULT now(),
  CHECK ((kind = 'brand' AND brand_id IS NOT NULL AND b_user_id IS NULL)
      OR (kind = 'person' AND brand_id IS NULL AND b_user_id IS NOT NULL))
);
CREATE UNIQUE INDEX threads_shopper_brand ON threads(a_user_id, brand_id) WHERE kind = 'brand';
CREATE UNIQUE INDEX threads_people ON threads(LEAST(a_user_id, b_user_id), GREATEST(a_user_id, b_user_id)) WHERE kind = 'person';
CREATE INDEX threads_b_user_idx ON threads(b_user_id);
CREATE INDEX threads_brand_idx ON threads(brand_id, last_message_at DESC);

CREATE TABLE messages (
  id             text PRIMARY KEY,
  thread_id      text NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
  side           text NOT NULL CHECK (side IN ('a', 'b')),
  sender_user_id text REFERENCES users(id) ON DELETE SET NULL,
  text           text NOT NULL DEFAULT '',
  image          jsonb,
  created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX messages_thread_idx ON messages(thread_id, created_at);

CREATE TABLE notifications (
  id         text PRIMARY KEY,
  scope      text NOT NULL,  -- shopper | studio
  user_id    text REFERENCES users(id) ON DELETE CASCADE,
  brand_id   text REFERENCES brands(id) ON DELETE CASCADE,
  who        text NOT NULL,
  text       text NOT NULL,
  kind       text NOT NULL,  -- Drops | Social | Orders
  avatar     jsonb,
  thumb      jsonb,
  href       text NOT NULL DEFAULT '',
  action     jsonb,
  read_at    timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  CHECK ((scope = 'shopper' AND user_id IS NOT NULL) OR (scope = 'studio' AND brand_id IS NOT NULL))
);
CREATE INDEX notifications_user_idx ON notifications(user_id, created_at DESC);
CREATE INDEX notifications_brand_idx ON notifications(brand_id, created_at DESC);

-- ---------- analytics ----------

CREATE TABLE analytics_events (
  id          bigserial PRIMARY KEY,
  type        text NOT NULL,  -- product_view | try_on | add_to_bag | post_view
  brand_id    text REFERENCES brands(id) ON DELETE CASCADE,
  product_id  text REFERENCES products(id) ON DELETE CASCADE,
  post_id     text REFERENCES posts(id) ON DELETE CASCADE,
  user_id     text REFERENCES users(id) ON DELETE SET NULL,
  from_try_on boolean NOT NULL DEFAULT false,
  at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX analytics_brand_idx ON analytics_events(brand_id, type, at);
CREATE INDEX analytics_user_idx ON analytics_events(user_id, type, product_id, at);
CREATE INDEX analytics_post_idx ON analytics_events(post_id) WHERE post_id IS NOT NULL;

-- +goose Down
DROP SCHEMA public CASCADE;
CREATE SCHEMA public;
