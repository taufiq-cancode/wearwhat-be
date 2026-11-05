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

## What works without any keys

Everything runs with no third-party accounts. Each service switches on when its keys are set:

| Service | With keys | Without keys (development) |
|---|---|---|
| Payments | `PAYSTACK_SECRET_KEY`: Paystack's hosted checkout, signed webhooks, saved cards | Test mode, clearly labelled in the app; every payment succeeds and no money moves |
| Email | `SMTP_HOST` and friends: real delivery | Printed to the console. Sign-up codes also show on screen, and recent messages are at `GET /api/v1/dev/outbox` |
| SMS | `TERMII_API_KEY`: phone sign-up | Phone sign-up is hidden |
| Apple sign-in | `APPLE_CLIENT_ID`: verified against Apple's keys | The button is hidden |

`.env.example` describes every setting.

**These integrations have only been tested against local fake servers,** not the real services. Before going live, run a real test transaction with Paystack test keys and send yourself real email and SMS.

## Going to production

`APP_ENV` must be left unset or set to anything other than `development`. The server then refuses to start without `DATA_KEY`, `APP_ORIGIN`, `PAYSTACK_SECRET_KEY` and `SMTP_HOST`, and refuses `SEED_DEMO`. Test payments, on-screen codes and `/dev/outbox` can't run in production.

- **Serve the frontend:** build it with `npm run build`, then set `STATIC_DIR` to its `dist` folder. The server handles the SPA fallback. If the API is on a different origin, set `VITE_API_URL` when building and list the app's origin in `APP_ORIGIN`.
- **Webhooks:** point Paystack at `https://your-domain/api/v1/webhooks/paystack`.
- **Currency:** prices are stored in USD. If your Paystack account can't charge USD, set `PAYMENT_CURRENCY` and `FX_RATE`. The rate is fixed until you change it, so decide how you'll keep it current.
- **Apple Pay** also needs your domain registered through Paystack.
- **Behind a proxy:** set `TRUST_PROXY=true` so rate limits see real client addresses. Only do this if the proxy overwrites `X-Forwarded-For`.
- **Uploads** are stored on local disk in `UPLOAD_DIR`, so back that folder up, or implement the `media.Storage` interface for S3.
- **`DATA_KEY`** encrypts bank accounts, national ID numbers and saved-card tokens. Losing it makes them unreadable, and there's no rotation command yet.

## Operating it

There are no admin screens yet. These tasks use the CLI, which reads the same `.env`:

```bash
go run ./cmd/wearwhat                          # lists every command
go run ./cmd/wearwhat brands-in-review
go run ./cmd/wearwhat brand-document <handle>  # where their private ID document is stored
go run ./cmd/wearwhat verify-brand <handle>    # they stay hidden from shoppers until this
go run ./cmd/wearwhat reports                  # what shoppers have reported
go run ./cmd/wearwhat payouts-due              # what you owe brands, refunds due, payments to check
go run ./cmd/wearwhat mark-payout-paid <handle>
go run ./cmd/wearwhat mark-refund-done <order>
```

**Money movement is deliberately manual.** The system calculates and records what's owed, but you send payouts and refunds yourself, then record them with the last two commands.

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
  media/          uploads
  payments/       Paystack and the test provider
  notify/         email and SMS
  jobs/           scheduled posts, drop releases, expiring checkouts, digest, cleanup
  seed/           demo data
```

Background jobs run inside the server every 30 seconds, and are safe to run from several instances at once.
