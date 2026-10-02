// Command orders is a small app built on a dalforge-generated DAL. It plays
// out a shop's day with fake data and checks each outcome, so it doubles as
// a smoke test: it exits non-zero if anything behaves unexpectedly.
//
// Run it with `mise run demo` from the repository root, or by hand:
//
//	dalforge generate     # schema, queries, sqlc.yaml and the DAL under gen/
//	go run .              # needs Postgres; see DATABASE_URL below
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/brianvoe/gofakeit/v7"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gisripa/dalforge/dal"
	"github.com/gisripa/dalforge/dal/dalpg"
	"github.com/gisripa/dalforge/examples/orders/gen/shop/v1/shopdal"
	"github.com/gisripa/dalforge/examples/orders/gen/sqlcdb"
)

// _defaultURL is the repository's local Postgres (compose.yaml).
const _defaultURL = "postgres://dalforge:dalforge@localhost:55432/dalforge?sslmode=disable"

// _demoDB is recreated on every run, so the demo is repeatable.
const _demoDB = "orders_demo"

func main() {
	seed := flag.Uint64("seed", 0, "fake-data seed; 0 picks a random one")
	flag.Parse()
	if err := run(context.Background(), *seed); err != nil {
		fmt.Fprintf(os.Stderr, "\nFAILED: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("\nAll checks passed.")
}

func run(ctx context.Context, seed uint64) error {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = _defaultURL
	}
	pool, err := freshDatabase(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Composition root: the only place that sees pools. Services get the
	// generated interfaces.
	runner := dalpg.New(dalpg.DB{Reader: pool, Writer: pool})
	accounts := shopdal.NewAccountRepository(runner)
	orders := shopdal.NewOrderRepository(runner)

	fake := gofakeit.New(seed)
	s := &steps{}

	// 1. Accounts.
	s.title("Create accounts with fake data")
	var people []shopdal.Account
	for range 3 {
		a, err := accounts.CreateAccount(ctx, shopdal.AccountCreateAccountParams{
			Email: new(fake.Email()),
			Name:  new(fake.Name()),
			// ID left nil: the DAL assigns a UUIDv7.
		})
		if err := s.check(err == nil && a.ID.Version() == 7, "created %s <%s> id=%s", a.Name, a.Email, a.ID, err); err != nil {
			return err
		}
		people = append(people, a)
	}

	s.title("Look an account up by its unique email")
	byEmail, err := accounts.GetAccountByEmail(ctx, people[1].Email)
	if err := s.check(err == nil && byEmail.ID == people[1].ID, "found %s by %s", byEmail.Name, people[1].Email, err); err != nil {
		return err
	}

	s.title("A duplicate email is rejected")
	_, err = accounts.CreateAccount(ctx, shopdal.AccountCreateAccountParams{Email: new(people[0].Email), Name: new("Copycat")})
	if err := s.check(errors.Is(err, dal.ErrAlreadyExists), "duplicate email → dal.ErrAlreadyExists", err); err != nil {
		return err
	}

	// 2. Orders.
	s.title("Place orders (status left nil → column default 'pending')")
	var placed []shopdal.Order
	for _, a := range people {
		o, err := placeOrder(ctx, accounts, orders, a.ID, fake)
		if err := s.check(err == nil && o.Status == "pending" && o.Version == 1,
			"order %s for %s: $%.2f, status=%s, version=%d", short(o.ID), a.Name, float64(o.AmountCents)/100, o.Status, o.Version, err); err != nil {
			return err
		}
		placed = append(placed, o)
	}

	s.title("Referential integrity is the app's job (no foreign keys)")
	_, err = placeOrder(ctx, accounts, orders, uuid.New(), fake)
	if err := s.check(errors.Is(err, dal.ErrNotFound), "order for an unknown account refused (account lookup → dal.ErrNotFound)", err); err != nil {
		return err
	}

	s.title("A required field left nil fails before touching the database")
	_, err = orders.CreateOrder(ctx, shopdal.OrderCreateOrderParams{AmountCents: new(int64(100))})
	var missing *dal.MissingFieldError
	if err := s.check(errors.As(err, &missing), "missing field reported: %v", err, nil); err != nil {
		return err
	}

	// 3. Optimistic locking.
	o := placed[0]
	s.title("Update with the version we read (compare-and-swap)")
	paid, err := orders.UpdateOrderStatus(ctx, shopdal.OrderUpdateOrderStatusParams{
		ID: o.ID, Version: o.Version, Status: new("paid"), Note: new("paid by card"),
	})
	if err := s.check(err == nil && paid.Status == "paid" && paid.Version == 2, "order %s → status=%s, version=%d", short(paid.ID), paid.Status, paid.Version, err); err != nil {
		return err
	}

	s.title("A stale version loses: someone else already changed the order")
	_, err = orders.UpdateOrderStatus(ctx, shopdal.OrderUpdateOrderStatusParams{ID: o.ID, Version: o.Version, Status: new("cancelled")})
	if err := s.check(errors.Is(err, dal.ErrVersionConflict), "update at version %d → dal.ErrVersionConflict", o.Version, err); err != nil {
		return err
	}

	s.title("Update with a required field left nil fails before touching the database")
	_, err = orders.UpdateOrderStatus(ctx, shopdal.OrderUpdateOrderStatusParams{ID: o.ID, Version: paid.Version})
	if err := s.check(errors.As(err, &missing) && missing.Field == "status", "missing field reported: %v", err, nil); err != nil {
		return err
	}

	// 4. Soft delete.
	gone := placed[1]
	s.title("Delete is soft: the row stays, reads stop seeing it")
	deleted, err := orders.DeleteOrder(ctx, gone.ID)
	if err := s.check(err == nil && deleted.DeletedAt != nil, "order %s deleted at %s", short(gone.ID), deleted.DeletedAt, err); err != nil {
		return err
	}
	_, err = orders.GetOrder(ctx, gone.ID)
	if err := s.check(errors.Is(err, dal.ErrNotFound), "reading it again → dal.ErrNotFound", err); err != nil {
		return err
	}

	// 5. The escape hatch.
	s.title("A hand-written sqlc query (queries/custom/reports.sql)")
	totals, err := sqlcdb.New(pool).OrderTotalsByStatus(ctx, people[0].ID)
	if err := s.check(err == nil && len(totals) == 1 && totals[0].Status == "paid", "%s's orders by status: %v", people[0].Name, totals, err); err != nil {
		return err
	}
	return nil
}

// placeOrder is what a service would do: check the account exists (there's
// no foreign key), then create the order. Phase 2 adds transactions so the
// check and the insert can share one.
func placeOrder(ctx context.Context, accounts shopdal.AccountReadRepository, orders shopdal.OrderWriteRepository, accountID uuid.UUID, fake *gofakeit.Faker) (shopdal.Order, error) {
	if _, err := accounts.GetAccount(ctx, accountID); err != nil {
		return shopdal.Order{}, fmt.Errorf("account %s: %w", short(accountID), err)
	}
	p := shopdal.OrderCreateOrderParams{
		AccountID:   new(accountID),
		AmountCents: new(int64(fake.IntRange(500, 50_000))),
	}
	if fake.Bool() {
		p.Note = new(fake.Sentence(4))
	}
	return orders.CreateOrder(ctx, p)
}

// freshDatabase recreates the demo database and applies the generated
// schema. (A real project applies migrations; dalforge generates those in
// phase 3.)
func freshDatabase(ctx context.Context, url string) (*pgxpool.Pool, error) {
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w (is Postgres up? mise run db:up)", url, err)
	}
	defer func() { _ = admin.Close(ctx) }()
	for _, sql := range []string{"DROP DATABASE IF EXISTS " + _demoDB + " WITH (FORCE)", "CREATE DATABASE " + _demoDB} {
		if _, err := admin.Exec(ctx, sql); err != nil {
			return nil, fmt.Errorf("%s: %w", sql, err)
		}
	}

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.Database = _demoDB
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	schema, err := os.ReadFile("schema/schema.sql")
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("%w (run dalforge generate first)", err)
	}
	if _, err := pool.Exec(ctx, string(schema)); err != nil {
		pool.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	fmt.Printf("Fresh database %q with the generated schema.\n", _demoDB)
	return pool, nil
}

// steps prints a numbered walkthrough and turns failed expectations into
// errors.
type steps struct{ n int }

func (s *steps) title(t string) {
	s.n++
	fmt.Printf("\n%d. %s\n", s.n, t)
}

// check prints "  ✓ msg" when ok, and otherwise returns an error. The last
// argument is the operation's error, shown on failure.
func (s *steps) check(ok bool, format string, args ...any) error {
	err, _ := args[len(args)-1].(error)
	msg := fmt.Sprintf(format, args[:len(args)-1]...)
	if ok {
		fmt.Printf("   ✓ %s\n", msg)
		return nil
	}
	return fmt.Errorf("step %d: %s (error: %v)", s.n, msg, err)
}

// short shows the random tail of an ID; a UUIDv7 starts with a timestamp,
// so IDs created together share their first characters.
func short(id uuid.UUID) string { s := id.String(); return s[len(s)-8:] }
