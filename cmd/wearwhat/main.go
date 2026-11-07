// Command wearwhat is the admin tool: brand verification, moderation and what's owed to whom.
// The admin panel at /admin does the same work; both go through internal/admin.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"text/tabwriter"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"wearwhat-be/internal/admin"
	"wearwhat-be/internal/config"
	"wearwhat-be/internal/db"
	"wearwhat-be/internal/media"
	"wearwhat-be/internal/seed"
)

const usage = `wearwhat admin

  seed --reset                wipe the database and reload demo data (development only)
  make-admin <email>          let this account use the admin panel at /admin
  remove-admin <email>        take admin away from this account
  admins                      list who can use the admin panel
  brands-in-review            list brands waiting for verification
  verify-brand <handle>       approve a brand so shoppers can see it
  reject-brand <handle>       reject a brand
  brand-document <handle>     save a readable copy of a brand's private document in this folder
                              (add --stdout to write it to standard output instead)
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
	case "make-admin":
		return setAdmin(ctx, pool, args, true)
	case "remove-admin":
		return setAdmin(ctx, pool, args, false)
	case "admins":
		return admins(ctx, pool)
	case "brands-in-review":
		return brandsInReview(ctx, pool)
	case "verify-brand":
		return setBrandStatus(ctx, pool, args, admin.Verified)
	case "reject-brand":
		return setBrandStatus(ctx, pool, args, admin.Rejected)
	case "brand-document":
		return brandDocument(ctx, pool, cfg, args)
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

func table() *tabwriter.Writer { return tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0) }

func setAdmin(ctx context.Context, pool *pgxpool.Pool, args []string, on bool) error {
	email, err := needArg(args, "email")
	if err != nil {
		return err
	}
	handle, err := admin.SetAdmin(ctx, pool, admin.CLI, email, on)
	if err != nil {
		return err
	}
	if on {
		fmt.Printf("@%s is now an admin. Sign in as them and open /admin.\n", handle)
	} else {
		fmt.Printf("@%s is no longer an admin\n", handle)
	}
	return nil
}

func admins(ctx context.Context, pool *pgxpool.Pool) error {
	list, err := admin.Admins(ctx, pool)
	if err != nil {
		return err
	}
	w := table()
	fmt.Fprintln(w, "HANDLE\tNAME\tEMAIL")
	for _, a := range list {
		fmt.Fprintf(w, "@%s\t%s\t%s\n", a.Handle, a.Name, a.Email)
	}
	return w.Flush()
}

func brandsInReview(ctx context.Context, pool *pgxpool.Pool) error {
	list, err := admin.Brands(ctx, pool, admin.InReview)
	if err != nil {
		return err
	}
	w := table()
	fmt.Fprintln(w, "HANDLE\tNAME\tLEGAL NAME\tTYPE\tSUBMITTED")
	for _, b := range list {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", b.Handle, b.Name, b.LegalName, b.Type, b.SubmittedAt.Format("2006-01-02"))
	}
	return w.Flush()
}

func setBrandStatus(ctx context.Context, pool *pgxpool.Pool, args []string, status string) error {
	handle, err := needArg(args, "handle")
	if err != nil {
		return err
	}
	name, err := admin.SetBrandStatus(ctx, pool, admin.CLI, handle, status, "")
	if err != nil {
		return err
	}
	fmt.Printf("%s is now %s\n", name, status)
	return nil
}

func brandDocument(ctx context.Context, pool *pgxpool.Pool, cfg *config.Config, args []string) error {
	toStdout := slices.Contains(args, "--stdout")
	args = slices.DeleteFunc(slices.Clone(args), func(a string) bool { return a == "--stdout" })
	handle, err := needArg(args, "handle")
	if err != nil {
		return err
	}
	doc, err := admin.Document(ctx, pool, media.NewStore(cfg), cfg.DataKey, handle)
	if err != nil {
		return err
	}
	if toStdout {
		// the type goes to stderr, so it stays out of a redirected file and still reaches the terminal
		fmt.Fprintf(os.Stderr, "This is a %s file. It's a readable copy of a private document: delete it once you've reviewed it.\n", media.Extension(doc.ContentType))
		_, err = os.Stdout.Write(doc.Data)
		return err
	}
	out, err := os.OpenFile(doc.Name, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := out.Write(doc.Data); err != nil {
		return err
	}
	fmt.Printf("Saved %s (%s). This is a readable copy of a private document: delete it once you've reviewed it.\n", doc.Name, doc.ContentType)
	return nil
}

func reports(ctx context.Context, pool *pgxpool.Pool) error {
	list, err := admin.Reports(ctx, pool)
	if err != nil {
		return err
	}
	w := table()
	fmt.Fprintln(w, "ID\tKIND\tTARGET\tREPORTED BY\tREASON\tWHEN")
	for _, r := range list {
		fmt.Fprintf(w, "%s\t%s\t%s\t@%s\t%s\t%s\n", r.ID, r.Kind, r.TargetID, r.Reporter, r.Reason, r.At.Format("2006-01-02"))
	}
	return w.Flush()
}

func resolveReport(ctx context.Context, pool *pgxpool.Pool, args []string) error {
	id, err := needArg(args, "id")
	if err != nil {
		return err
	}
	if err := admin.ResolveReport(ctx, pool, admin.CLI, id); err != nil {
		return err
	}
	fmt.Println("Report resolved")
	return nil
}

func payoutsDue(ctx context.Context, pool *pgxpool.Pool) error {
	m, err := admin.GetMoney(ctx, pool)
	if err != nil {
		return err
	}
	fmt.Println("PAYOUTS DUE (delivered over 2 days ago, not yet paid)")
	w := table()
	fmt.Fprintln(w, "BRAND\tPROVIDER\tBANK\tORDERS\tAMOUNT")
	for _, p := range m.Payouts {
		fmt.Fprintf(w, "@%s\t%s\t%s\t%d\t%s\n", p.Handle, p.Provider, p.Bank, p.Orders, admin.Dollars(p.Amount))
	}
	if err := w.Flush(); err != nil {
		return err
	}

	fmt.Println("\nREFUNDS DUE (returns approved)")
	w = table()
	fmt.Fprintln(w, "ORDER\tBRAND\tCUSTOMER\tAMOUNT")
	for _, r := range m.Refunds {
		fmt.Fprintf(w, "%s\t@%s\t@%s\t%s\n", r.Order, r.Brand, r.Customer, admin.Dollars(r.Amount))
	}
	if err := w.Flush(); err != nil {
		return err
	}

	fmt.Println("\nNEEDS ATTENTION (money arrived but the order wasn't fulfilled: refund or re-fulfil by hand)")
	w = table()
	fmt.Fprintln(w, "CHECKOUT\tPROBLEM\tEMAIL\tTOTAL")
	for _, a := range m.Attention {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", a.Checkout, a.Problem, a.Email, admin.Dollars(a.Total))
	}
	return w.Flush()
}

func markRefundDone(ctx context.Context, pool *pgxpool.Pool, args []string) error {
	order, err := needArg(args, "order")
	if err != nil {
		return err
	}
	id, err := admin.MarkRefundDone(ctx, pool, admin.CLI, order)
	if err != nil {
		return err
	}
	fmt.Println("Refund recorded for", id)
	return nil
}

func markPayoutPaid(ctx context.Context, pool *pgxpool.Pool, args []string) error {
	handle, err := needArg(args, "handle")
	if err != nil {
		return err
	}
	orders, total, err := admin.MarkPayoutPaid(ctx, pool, admin.CLI, handle, nil)
	if err != nil {
		return err
	}
	fmt.Printf("Marked %d orders (%s) as paid out for @%s\n", orders, admin.Dollars(total), handle)
	return nil
}
