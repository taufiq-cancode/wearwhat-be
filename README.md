# wearwhat-be

The Go API behind [wearwhat](../wearwhat): a shoppable social feed with try-on, and a studio for brands to run orders, catalog and promotions.

The full API is documented in [`../wearwhat/docs/API.md`](../wearwhat/docs/API.md).

Go · PostgreSQL · standard-library routing · `pgx` with hand-written SQL · `goose` migrations embedded in the binary

## Run it locally

You need Go 1.27+ and Docker.

```bash
cp .env.example .env
docker compose up -d       # Postgres on port 5433
go run ./cmd/server        # http://localhost:8080
```

The server applies migrations on start. With `SEED_DEMO=true` (the default in `.env.example`), it loads demo data the first time it sees an empty database.

Then run the frontend from `../wearwhat` with `npm run dev`, and open <http://localhost:5173>. It proxies `/api` and `/uploads` to this server.

### Demo logins

All use the password `wearwhat-demo`. They only exist when demo data is loaded, which production refuses to do.

| Email | Who | Use it to try |
|---|---|---|
| `amara@kalu.co` | Amara, a shopper who also owns HIGHFASHION | The shopper side, and `/studio` with 90 days of history |
| `tobi@wearwhat.test` | Tobi, a shopper and HIGHFASHION customer | The other side of a conversation or order |
| `debit@wearwhat.test` | Owner of DEBIT | A second brand |
| `seyi@wearwhat.test` | HIGHFASHION staff, Orders role | Limited studio access |
| `kemi@wearwhat.test` | HIGHFASHION staff, Content role | Limited studio access |

Promo codes to try: `DROP02` (15% off Drop 02, on Paint-splatter set, Acid-wash denim set and Indigo wide-leg jean), `FREESHIPLAG`, `DEBIT20`.

To start over: `go run ./cmd/wearwhat seed --reset`. This **wipes the database**, and only runs in development.

## Tests

```bash
go test ./...              # needs `docker compose up -d`
```

Each test gets its own Postgres schema and drives the real API over HTTP. Nothing in the database is mocked. Set `TEST_DATABASE_URL` to use a different database.

The browser tests live in `../wearwhat/e2e`. Start this server with demo data, start the frontend, then run `npx playwright test` there.

Resend and Cloudinary also have tests that use your real accounts. They're skipped unless you turn them on, and they need your keys in `.env`:

```bash
LIVE_TESTS=1 go test ./internal/notify ./internal/media -run Live -v
```

The email test sends one message to `LIVE_TEST_EMAIL`. The Cloudinary tests upload an image, a video and a private file, check them, then delete them.

## What works without any keys

Everything runs with no third-party accounts. Each service switches on when its keys are set:

| Service | With keys | Without keys (development) |
|---|---|---|
| Payments | `PAYSTACK_SECRET_KEY`: Paystack's hosted checkout, signed webhooks, saved cards | Test mode, clearly labelled in the app; every payment succeeds and no money moves |
| Email | `RESEND_API_KEY` and `MAIL_FROM`: delivery through Resend | Printed to the console. Sign-up codes also show on screen, and recent messages are at `GET /api/v1/dev/outbox` |
| Uploads | `CLOUDINARY_URL`: images, videos and encrypted documents on Cloudinary | Saved in `UPLOAD_DIR` and served from `/uploads/:id` |
| SMS | `TERMII_API_KEY`: phone sign-up | Phone sign-up is hidden |
| Apple sign-in | `APPLE_CLIENT_ID`: verified against Apple's keys | The button is hidden |

`.env.example` describes every setting.

**Paystack, Termii and Apple have only been tested against local fake servers,** not the real services. Run the live tests above for Resend and Cloudinary, and before going live make a real test transaction with Paystack test keys.

## Going to production

This describes hosting on Render, using `render.yaml`, with DNS at Namecheap. In production mode the server refuses to start without `DATA_KEY`, `APP_ORIGIN`, `PAYSTACK_SECRET_KEY`, `RESEND_API_KEY`, `MAIL_FROM` and `CLOUDINARY_URL`, and refuses `SEED_DEMO`. Test payments, on-screen codes and `/dev/outbox` can't run in production. Leaving `APP_ENV` unset also counts as production.

The frontend is the Render static site `wearwhat-fe`, on `https://www.wearwhat.site`. The API goes on `https://api.wearwhat.site`.

**`APP_ORIGIN` must be the `www` address.** `wearwhat.site` redirects there, so it's the address browsers report, and the API refuses requests from any other. `render.yaml` already sets this.

**The two must share a domain.** The login cookie isn't sent between different sites, and Render's `onrender.com` addresses count as different sites from each other, so login fails on those addresses.

### 1. Set up the services

1. **Resend:** add the domain `wearwhat.site` and create an API key. Resend then shows the DNS records you'll add in step 3. Until the domain is verified, Resend won't deliver to anyone except you.
2. **Cloudinary:** copy the `CLOUDINARY_URL` value from the dashboard's API Keys page.
3. **Paystack:** copy your secret key. Start with the test key (`sk_test_...`).

### 2. Create the API on Render

In Render, choose New > Blueprint and pick this repo. Render creates the API and its database, adds the `api.wearwhat.site` domain, and asks for these values:

| Setting | Value |
|---|---|
| `SUPPORT_EMAIL` | An inbox you read. It's shown to people who need help |
| `RESEND_API_KEY` | From Resend |
| `CLOUDINARY_URL` | From Cloudinary |
| `PAYSTACK_SECRET_KEY` | From Paystack |
| `PAYMENT_CURRENCY`, `FX_RATE` | `USD` and `1`, or for example `NGN` and `1500` |

**Prices are stored in USD.** If your Paystack account can't charge USD, set the currency and rate. The rate stays fixed until you change it, so decide how you'll keep it current. The server refuses to start if the two don't make sense together.

**After the first deploy, copy `DATA_KEY` from the service's Environment page into a password manager.** It encrypts documents, bank accounts, national ID numbers and saved-card tokens. Losing it makes them unreadable, and there's no rotation command yet.

### 3. Add the DNS records

In Namecheap, open Domain List > Manage > Advanced DNS and add these:

| Type | Host | Value | For |
|---|---|---|---|
| CNAME | `api` | The `onrender.com` address of `wearwhat-api`, shown in Render | The API |
| MX | `send` | From Resend, priority 10 | Resend |
| TXT | `send` | From Resend | Resend |
| TXT | `resend._domainkey` | From Resend | Resend |
| A | `@` | `216.24.57.1` | Replaces the CNAME on `@`. Render and Namecheap both say not to use one there, because it can interfere with mail to the domain |

The MX record goes under Mail Settings, after you choose **Custom MX**. The domain was set to Email Forwarding when this was written, and neither Namecheap nor Resend documents a way to keep both. **Expect any forwarding from `@wearwhat.site` addresses to stop,** so check your forwarding rules first and send a test message afterwards.

### 4. Point the frontend at it

On the `wearwhat-fe` service in Render:

1. Set `VITE_API_URL=https://api.wearwhat.site`.
2. Under Redirects/Rewrites, add a rule with source `/*`, destination `/index.html` and action Rewrite. Without it, every address except the home page shows "Not Found", including links in emails and the return from Paystack.
3. Redeploy, because `VITE_API_URL` is built into the app.

### 5. Finish setup

**Paystack webhook:** set it to `https://api.wearwhat.site/api/v1/webhooks/paystack`. Test and live mode each have their own.

**Find the proxy setting.** Rate limits need each visitor's real address, and that depends on how many proxies sit in front of the server. Run this from your own computer:

```bash
curl -H "X-Forwarded-For: 1.2.3.4" https://api.wearwhat.site/healthz
```

Then find that request in the service's logs in Render and read its `ip=` value. Add `TRUSTED_PROXY_HOPS` in the dashboard, starting at `1` and going up until `ip=` shows your real address (a search for "what is my IP" will tell you). It must never show `1.2.3.4`: that would mean anyone can fake their address.

With the default of `0`, every visitor looks like the same address, so they all share one rate limit.

### 6. Check it, then go live

```bash
curl https://api.wearwhat.site/healthz        # {"status":"ok"}
curl https://api.wearwhat.site/api/v1/config  # "devMode":false and "testPayments":false
```

Using the test Paystack key, sign up with a real email address, create a brand and approve it, add a product with a photo, buy it with a Paystack test card, then ship and deliver it.

For launch, switch to a fresh database and live Paystack keys. Otherwise your test orders will show up in `payouts-due`.

### Limits to watch

- **Resend's free plan** allows 100 emails a day, and sign-up codes share that with order emails and the weekly digest. A digest to more than about 100 people could block sign-ups until the next day, so upgrade before you reach that size.
- **Apple Pay** also needs your domain registered through Paystack.

## Operating it

Day to day, use the **admin panel** at `/admin` (for example `https://www.wearwhat.site/admin`). It covers brand review, shopper reports and money, and every change is logged with who made it. Anyone who isn't an admin gets "not found" there.

### Make yourself an admin

Admin is a flag on an ordinary account, granted with the admin tool. Sign up on the site first, then run the tool where the API runs. On Railway, install the [Railway CLI](https://docs.railway.com/guides/cli) (`npm i -g @railway/cli`), run `railway login` and `railway link` once (pick the API service), then `railway ssh` and `cd /app`. The container already has the production variables. Locally, use `go run ./cmd/wearwhat` in place of `./wearwhat`, which reads `.env`.

```bash
./wearwhat make-admin you@example.com
```

Removing someone's admin takes effect on their very next request. Treat admin accounts with care: they can read brands' private documents and record payouts, so give them strong passwords.

### The admin tool

Everything the panel does also works from the command line, using the same code:

```bash
./wearwhat                          # lists every command
./wearwhat make-admin <email>
./wearwhat remove-admin <email>
./wearwhat admins                   # who can use the admin panel
./wearwhat brands-in-review
./wearwhat brand-document <handle>  # saves a readable copy of their private document in this folder
./wearwhat verify-brand <handle>    # they stay hidden from shoppers until this
./wearwhat reject-brand <handle>
./wearwhat reports                  # what shoppers have reported
./wearwhat resolve-report <id>
./wearwhat payouts-due              # what you owe brands, refunds due, payments to check
./wearwhat mark-payout-paid <handle>
./wearwhat mark-refund-done <order>
```

**Reviewing a brand's document.** Documents are encrypted before they reach Cloudinary, so downloading one from Cloudinary's dashboard only gets you scrambled data. Open it from the brand's page in the admin panel instead. The server decrypts it and your browser shows it in a new tab without saving it anywhere. `brand-document` saves a copy on whichever machine runs the tool. On Railway that's the server, where you can't open it, so use the panel there.

**Money movement is deliberately manual.** The system calculates and records what's owed, but you send payouts and refunds yourself, then record them in the panel's Money screen. To mark a payout as paid, the panel sends the amount you saw. If more orders have come due since then, it refuses and asks you to refresh, so you never record more than you sent.

## Layout

```
cmd/server        the HTTP server
cmd/wearwhat      the admin CLI
migrations/       SQL migrations, embedded in the binary
internal/
  server/         wires everything together; the API integration tests live here
  httpx/          routing with auth levels, JSON errors, middleware, rate limits
  domain/         JSON types and pure rules: pricing, order flow, validation
  views/          shared queries, and the SQL rules for what's visible to whom
  auth/           sign-up, codes, sessions, password reset, Apple
  account/        profiles, addresses, saved cards, follows, blocks
  catalog/        brands, products, explore, trends, stock alerts
  social/         feed, posts, comments, wears, reports, analytics events
  commerce/       bag, promos, checkout, payment, orders, returns, reviews
  inbox/          conversations and notifications
  studio/         the brand side: onboarding, orders, catalog, drops, metrics, team
  media/          uploads: Cloudinary, or local disk in development
  payments/       Paystack and the test provider
  notify/         email (Resend) and SMS (Termii)
  jobs/           scheduled posts, drop releases, expiring checkouts, digest, cleanup
  seed/           demo data
```

Background jobs run inside the server every 30 seconds, and are safe to run from several instances at once.
