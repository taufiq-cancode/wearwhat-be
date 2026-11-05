// Command wearwhat is the admin tool: brand verification, moderation and what's owed to whom.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"wearwhat-be/internal/config"
	"wearwhat-be/internal/db"
	"wearwhat-be/internal/domain"
	"wearwhat-be/internal/seed"
)

const usage = `wearwhat admin

  seed --reset                wipe the database and reload demo data (development only)
  brands-in-review            list brands waiting for verification
  verify-brand <handle>       approve a brand so shoppers can see it
  reject-brand <handle>       reject a brand
  brand-document <handle>     show where a brand's private verification document is stored
  reports                     list unresolved reports from shoppers
  resolve-report <id>         mark a report handled
  payouts-due                 what's owed to brands, refunds due, and payments needing attention
  mark-payout-paid <handle>   record that you've paid a brand everything currently due
  mark-refund-done <order>    record that you've refunded an approved return, e.g. WW-20417
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		return
	}
	if err := run(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(cmd string, args []string) error {
	_ = godotenv.Load()
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, cfg.DatabaseURL, "")
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}
	switch cmd {
	case "seed":
		if !cfg.IsDev() {
			return errors.New("seeding is only allowed with APP_ENV=development")
		}
		if len(args) == 0 || args[0] != "--reset" {
			return errors.New("seed wipes all data: run it as `seed --reset` to confirm")
		}
		if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
			return err
		}
		if err := db.Migrate(ctx, pool); err != nil {
			return err
		}
		if err := seed.Run(ctx, pool, cfg.Location, cfg.DataKey); err != nil {
			return err
		}
		fmt.Println("Demo data loaded. Sign in as amara@kalu.co with password", seed.DemoPassword)
		return nil
	case "brands-in-review":
		return brandsInReview(ctx, pool)
	case "verify-brand":
		return setBrandStatus(ctx, pool, args, "verified")
	case "reject-brand":
		return setBrandStatus(ctx, pool, args, "rejected")
	case "brand-document":
		return brandDocument(ctx, pool, args, cfg.UploadDir)
	case "reports":
		return reports(ctx, pool)
	case "resolve-report":
		return resolveReport(ctx, pool, args)
	case "payouts-due":
		return payoutsDue(ctx, pool)
	case "mark-payout-paid":
		return markPayoutPaid(ctx, pool, args)
	case "mark-refund-done":
		return markRefundDone(ctx, pool, args)
	}
	fmt.Print(usage)
	return fmt.Errorf("unknown command %q", cmd)
}

func needArg(args []string, name string) (string, error) {
	if len(args) != 1 {
		return "", fmt.Errorf("expected <%s>", name)
	}
	return args[0], nil
}

func brandsInReview(ctx context.Context, pool *pgxpool.Pool) error {
	rows, err := pool.Query(ctx, `SELECT handle, name, business ->> 'legalName', business ->> 'type', created_at FROM brands WHERE status = 'in_review' ORDER BY created_at`)
	if err != nil {
		return err
	}
	defer rows.Close()
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "HANDLE\tNAME\tLEGAL NAME\tTYPE\tSUBMITTED")
	for rows.Next() {
		var handle, name string
		var legal, kind *string
		var at time.Time
		if err := rows.Scan(&handle, &name, &legal, &kind, &at); err != nil {
			return err
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", handle, name, deref(legal), deref(kind), at.Format("2006-01-02"))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return w.Flush()
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func setBrandStatus(ctx context.Context, pool *pgxpool.Pool, args []string, status string) error {
	handle, err := needArg(args, "handle")
	if err != nil {
		return err
	}
	var name string
	var brandID string
	err = pool.QueryRow(ctx, `UPDATE brands SET status = $2, verified_at = CASE WHEN $2 = 'verified' THEN now() ELSE NULL END WHERE handle = $1 RETURNING id, name`, handle, status).Scan(&brandID, &name)
	if db.IsNotFound(err) {
		return fmt.Errorf("no brand @%s", handle)
	}
	if err != nil {
		return err
	}
	text := "Your store is verified and live on wearwhat."
	if status == "rejected" {
		text = "We couldn’t verify your store. Contact support to review your documents."
	}
	if _, err := pool.Exec(ctx, `INSERT INTO notifications (id, scope, brand_id, who, text, kind, href)
		VALUES ('n_' || replace(gen_random_uuid()::text, '-', ''), 'studio', $1, 'wearwhat', $2, 'Orders', '/studio/settings')`, brandID, text); err != nil {
		return err
	}
	fmt.Printf("%s is now %s\n", name, status)
	return nil
}

func brandDocument(ctx context.Context, pool *pgxpool.Pool, args []string, uploadDir string) error {
	handle, err := needArg(args, "handle")
	if err != nil {
		return err
	}
	var path, ctype string
	err = pool.QueryRow(ctx, `SELECT u.path, u.content_type FROM brands b JOIN uploads u ON u.id = b.business ->> 'documentId' WHERE b.handle = $1`, handle).Scan(&path, &ctype)
	if db.IsNotFound(err) {
		return fmt.Errorf("no document on file for @%s", handle)
	}
	if err != nil {
		return err
	}
	fmt.Printf("%s/%s (%s)\n", uploadDir, path, ctype)
	return nil
}

func reports(ctx context.Context, pool *pgxpool.Pool) error {
	rows, err := pool.Query(ctx, `SELECT r.id, r.target_kind, r.target_id, u.handle, r.reason, r.created_at FROM reports r JOIN users u ON u.id = r.reporter_id
		WHERE r.resolved_at IS NULL ORDER BY r.created_at`)
	if err != nil {
		return err
	}
	defer rows.Close()
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tKIND\tTARGET\tREPORTED BY\tREASON\tWHEN")
	for rows.Next() {
		var id, kind, target, handle, reason string
		var at time.Time
		if err := rows.Scan(&id, &kind, &target, &handle, &reason, &at); err != nil {
			return err
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t@%s\t%s\t%s\n", id, kind, target, handle, reason, at.Format("2006-01-02"))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return w.Flush()
}

func resolveReport(ctx context.Context, pool *pgxpool.Pool, args []string) error {
	id, err := needArg(args, "id")
	if err != nil {
		return err
	}
	tag, err := pool.Exec(ctx, `UPDATE reports SET resolved_at = now() WHERE id = $1 AND resolved_at IS NULL`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("no open report %s", id)
	}
	fmt.Println("Report resolved")
	return nil
}

const payoutDueWhere = `o.status = 'DELIVERED' AND o.payout_paid_at IS NULL AND o.delivered_at < now() - interval '2 days'`

func payoutsDue(ctx context.Context, pool *pgxpool.Pool) error {
	fmt.Println("PAYOUTS DUE (delivered over 2 days ago, not yet paid)")
	rows, err := pool.Query(ctx, `SELECT b.handle, b.payouts ->> 'provider', b.payouts ->> 'bank', count(*), sum(o.payout)
		FROM orders o JOIN brands b ON b.id = o.brand_id WHERE `+payoutDueWhere+` GROUP BY b.id ORDER BY sum(o.payout) DESC`)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "BRAND\tPROVIDER\tBANK\tORDERS\tAMOUNT")
	for rows.Next() {
		var handle string
		var provider, bank *string
		var orders, amount int64
		if err := rows.Scan(&handle, &provider, &bank, &orders, &amount); err != nil {
			rows.Close()
			return err
		}
		fmt.Fprintf(w, "@%s\t%s\t%s\t%d\t$%.2f\n", handle, deref(provider), deref(bank), orders, float64(amount)/100)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if err := w.Flush(); err != nil {
		return err
	}

	fmt.Println("\nREFUNDS DUE (returns approved)")
	rows, err = pool.Query(ctx, `SELECT o.seq, b.handle, u.handle, o.subtotal - o.discount + o.shipping FROM orders o
		JOIN brands b ON b.id = o.brand_id JOIN users u ON u.id = o.user_id WHERE o.refund_status = 'due' ORDER BY o.seq`)
	if err != nil {
		return err
	}
	w = tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "ORDER\tBRAND\tCUSTOMER\tAMOUNT")
	for rows.Next() {
		var seq int64
		var brand, customer string
		var amount int
		if err := rows.Scan(&seq, &brand, &customer, &amount); err != nil {
			rows.Close()
			return err
		}
		fmt.Fprintf(w, "WW-%d\t@%s\t@%s\t$%.2f\n", seq, brand, customer, float64(amount)/100)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if err := w.Flush(); err != nil {
		return err
	}

	fmt.Println("\nNEEDS ATTENTION (money arrived but the order wasn't fulfilled: refund or re-fulfil by hand)")
	rows, err = pool.Query(ctx, `SELECT number, status, email, total FROM checkouts WHERE status IN ('paid_after_expiry', 'amount_mismatch') ORDER BY created_at`)
	if err != nil {
		return err
	}
	defer rows.Close()
	w = tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "CHECKOUT\tPROBLEM\tEMAIL\tTOTAL")
	for rows.Next() {
		var number, status, email string
		var total int
		if err := rows.Scan(&number, &status, &email, &total); err != nil {
			return err
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t$%.2f\n", number, status, email, float64(total)/100)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return w.Flush()
}

func markRefundDone(ctx context.Context, pool *pgxpool.Pool, args []string) error {
	id, err := needArg(args, "order")
	if err != nil {
		return err
	}
	seq, ok := domain.ParseOrderID(id)
	if !ok {
		return fmt.Errorf("%q isn't an order id", id)
	}
	tag, err := pool.Exec(ctx, `UPDATE orders SET refund_status = 'done' WHERE seq = $1 AND refund_status = 'due'`, seq)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("no refund due on %s", id)
	}
	fmt.Println("Refund recorded for", domain.ShopperOrderID(seq))
	return nil
}

func markPayoutPaid(ctx context.Context, pool *pgxpool.Pool, args []string) error {
	handle, err := needArg(args, "handle")
	if err != nil {
		return err
	}
	tag, err := pool.Exec(ctx, `UPDATE orders o SET payout_paid_at = now() FROM brands b
		WHERE b.id = o.brand_id AND b.handle = $1 AND `+payoutDueWhere, handle)
	if err != nil {
		return err
	}
	fmt.Printf("Marked %d orders as paid out for @%s\n", tag.RowsAffected(), handle)
	return nil
}
