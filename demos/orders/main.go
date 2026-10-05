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
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/brianvoe/gofakeit/v7"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gisripa/dalforge/dal"
	"github.com/gisripa/dalforge/dal/dalpg"
	"github.com/gisripa/dalforge/demos/orders/gen/shop/v1/shopdal"
	"github.com/gisripa/dalforge/demos/orders/gen/sqlcdb"
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

// app is what a service would hold: the generated repositories (interfaces)
// and the runner for transactions. It never sees a pool.
type app struct {
	run      *dalpg.Runner
	accounts shopdal.AccountRepository
	orders   shopdal.OrderRepository
	fake     *gofakeit.Faker
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

	// Composition root: the only place that sees pools.
	runner := dalpg.New(dalpg.DB{Reader: pool, Writer: pool})
	a := &app{
		run:      runner,
		accounts: shopdal.NewAccountRepository(runner),
		orders:   shopdal.NewOrderRepository(runner),
		fake:     gofakeit.New(seed),
	}
	s := &steps{}
	start := time.Now().Add(-time.Minute)

	// --- Accounts -------------------------------------------------------
	s.title("Create accounts with fake data (ID left nil → the DAL assigns a UUIDv7)")
	var people []shopdal.Account
	for range 3 {
		acct, err := a.accounts.CreateAccount(ctx, shopdal.AccountCreateAccountParams{
			Email: new(a.fake.Email()),
			Name:  new(a.fake.Name()),
		})
		if err := s.check(err == nil && acct.ID.Version() == 7, "created %s <%s>", acct.Name, acct.Email, err); err != nil {
			return err
		}
		people = append(people, acct)
	}

	s.title("Look an account up by its unique email; a duplicate email is rejected")
	byEmail, err := a.accounts.GetAccountByEmail(ctx, people[1].Email)
	if err := s.check(err == nil && byEmail.ID == people[1].ID, "found %s by email", byEmail.Name, err); err != nil {
		return err
	}
	_, err = a.accounts.CreateAccount(ctx, shopdal.AccountCreateAccountParams{Email: new(people[0].Email), Name: new("Copycat")})
	if err := s.check(errors.Is(err, dal.ErrAlreadyExists), "duplicate email → dal.ErrAlreadyExists", err); err != nil {
		return err
	}

	s.title("Upsert by email: an existing email updates the name, a new one inserts")
	renamed, err := a.accounts.UpsertAccountByEmail(ctx, shopdal.AccountUpsertAccountByEmailParams{Email: new(people[2].Email), Name: new("Renamed Person")})
	if err := s.check(err == nil && renamed.ID == people[2].ID && renamed.Name == "Renamed Person", "%s kept its id and is now %q", people[2].Email, renamed.Name, err); err != nil {
		return err
	}
	fresh, err := a.accounts.UpsertAccountByEmail(ctx, shopdal.AccountUpsertAccountByEmailParams{Email: new(a.fake.Email()), Name: new(a.fake.Name())})
	if err := s.check(err == nil && fresh.ID != people[0].ID && fresh.ID != uuid.Nil, "new email %s inserted", fresh.Email, err); err != nil {
		return err
	}

	// --- Orders, with referential integrity in a transaction ------------
	s.title("Place orders in transactions that lock the account (there are no foreign keys)")
	counts := map[string]int{}
	for i, p := range people {
		n := 5
		if i == 0 {
			n = 25 // enough to page through
		}
		for range n {
			status := a.fake.RandomString([]string{"pending", "paid", "shipped"})
			if _, err := a.placeOrder(ctx, p.ID, status); err != nil {
				return s.check(false, "place an order for %s", p.Name, err)
			}
			counts[status]++
		}
	}
	if err := s.check(true, "placed 25 orders for %s and 5 each for the others: %v", people[0].Name, counts, nil); err != nil {
		return err
	}
	_, err = a.placeOrder(ctx, uuid.New(), "pending")
	if err := s.check(errors.Is(err, dal.ErrNotFound), "an order for an unknown account is refused and its transaction rolled back", err); err != nil {
		return err
	}

	s.title("A required field left nil fails before touching the database")
	_, err = a.orders.CreateOrder(ctx, shopdal.OrderCreateOrderParams{AmountCents: new(int64(100))})
	var missing *dal.MissingFieldError
	if err := s.check(errors.As(err, &missing) && missing.Field == "account_id", "%v", err, nil); err != nil {
		return err
	}

	// --- Pagination -----------------------------------------------------
	s.title("Page through an account's orders, newest first (keyset pagination, no OFFSET)")
	byAccount := shopdal.OrderListOrdersByAccountParams{AccountID: people[0].ID, PageSize: 10}
	var seen []shopdal.Order
	var firstToken string
	for page := 1; ; page++ {
		p, err := a.orders.ListOrdersByAccount(ctx, byAccount)
		if err != nil {
			return s.check(false, "page %d", page, err)
		}
		seen = append(seen, p.Items...)
		next := "last page"
		if p.NextPageToken != "" {
			next = "next token " + p.NextPageToken[:16] + "…"
			if firstToken == "" {
				firstToken = p.NextPageToken
			}
		}
		if err := s.check(true, "page %d: %d orders, %s", page, len(p.Items), next, nil); err != nil {
			return err
		}
		if p.NextPageToken == "" {
			break
		}
		byAccount = byAccount.WithPageToken(p.NextPageToken)
	}
	if err := s.check(len(seen) == 25 && unique(seen) && newestFirst(seen), "%d orders in all, no duplicates, newest first", len(seen), nil); err != nil {
		return err
	}

	s.title("dal.All iterates every page, for in-process use")
	paid, err := count(dal.All(ctx, shopdal.OrderListOrdersByStatusParams{Status: "paid", PageSize: 4}, a.orders.ListOrdersByStatus))
	if err := s.check(err == nil && paid == counts["paid"], "%d paid orders across all accounts, 4 per page", paid, err); err != nil {
		return err
	}

	s.title("A page token only works for the list and filters it was issued for")
	_, err = a.orders.ListOrdersByAccount(ctx, shopdal.OrderListOrdersByAccountParams{AccountID: people[1].ID, PageToken: firstToken})
	if err := s.check(errors.Is(err, dal.ErrInvalidPageToken), "token reused for another account → dal.ErrInvalidPageToken", err); err != nil {
		return err
	}

	s.title("A range list: pending orders created in a time window")
	window := shopdal.OrderListOrdersByStatusAndCreatedAtParams{Status: "pending", CreatedAtFrom: start, CreatedAtTo: time.Now().Add(time.Minute)}
	pending, err := count(dal.All(ctx, window, a.orders.ListOrdersByStatusAndCreatedAt))
	if err := s.check(err == nil && pending == counts["pending"], "%d pending orders in the window", pending, err); err != nil {
		return err
	}

	// --- Writes ---------------------------------------------------------
	o := seen[0]
	s.title("Update with the version we read (compare-and-swap); a stale version loses")
	upd, err := a.orders.UpdateOrderStatus(ctx, shopdal.OrderUpdateOrderStatusParams{ID: o.ID, Version: o.Version, Status: new("shipped"), Note: new("left the warehouse")})
	if err := s.check(err == nil && upd.Version == o.Version+1, "order %s → %s, version %d", short(o.ID), upd.Status, upd.Version, err); err != nil {
		return err
	}
	_, err = a.orders.UpdateOrderStatus(ctx, shopdal.OrderUpdateOrderStatusParams{ID: o.ID, Version: o.Version, Status: new("cancelled")})
	if err := s.check(errors.Is(err, dal.ErrVersionConflict), "update at the old version %d → dal.ErrVersionConflict", o.Version, err); err != nil {
		return err
	}
	_, err = a.orders.UpdateOrderStatus(ctx, shopdal.OrderUpdateOrderStatusParams{ID: o.ID, Version: upd.Version})
	if err := s.check(errors.As(err, &missing) && missing.Field == "status", "status left nil: %v", err, nil); err != nil {
		return err
	}

	s.title("Delete is soft: lists and reads stop seeing the order")
	if _, err := a.orders.DeleteOrder(ctx, o.ID); err != nil {
		return s.check(false, "delete", err)
	}
	left, err := count(dal.All(ctx, shopdal.OrderListOrdersByAccountParams{AccountID: people[0].ID}, a.orders.ListOrdersByAccount))
	if err != nil {
		return s.check(false, "list after delete", err)
	}
	_, err = a.orders.GetOrder(ctx, o.ID)
	if err := s.check(left == 24 && errors.Is(err, dal.ErrNotFound), "the account now lists %d orders; reading the deleted one → dal.ErrNotFound", left, err); err != nil {
		return err
	}

	// --- Escape hatch and indexes ----------------------------------------
	s.title("A hand-written sqlc query (queries/custom/reports.sql)")
	totals, err := sqlcdb.New(pool).OrderTotalsByStatus(ctx, people[1].ID)
	if err := s.check(err == nil && len(totals) > 0, "%s's orders by status: %v", people[1].Name, totals, err); err != nil {
		return err
	}

	s.title("The indexes dalforge derived from the list rpcs (schema/schema.sql)")
	return printDerivedIndexes(s)
}

// placeOrder is what a service would do. In one transaction, it locks the
// account row so the account can't be deleted before the order commits
// (what a foreign key would otherwise guarantee), then creates the order.
func (a *app) placeOrder(ctx context.Context, accountID uuid.UUID, status string) (shopdal.Order, error) {
	var placed shopdal.Order
	err := shopdal.WithTx(ctx, a.run, func(ctx context.Context, tx shopdal.Tx) error {
		// A custom query (queries/custom/accounts.sql) in the same transaction.
		if _, err := tx.Queries().LockAccountForKeyShare(ctx, accountID); err != nil {
			if dalpg.IsNoRows(err) {
				return fmt.Errorf("account %s: %w", short(accountID), dal.ErrNotFound)
			}
			return err
		}
		p := shopdal.OrderCreateOrderParams{
			AccountID:   new(accountID),
			AmountCents: new(int64(a.fake.IntRange(500, 50_000))),
		}
		if status != "pending" { // pending is the column default: leave it nil
			p.Status = new(status)
		}
		if a.fake.Bool() {
			p.Note = new(a.fake.Sentence(4))
		}
		var err error
		placed, err = tx.Order().CreateOrder(ctx, p)
		return err
	})
	return placed, err
}

// count drains an iterator from dal.All.
func count[T any](items func(yield func(T, error) bool)) (int, error) {
	n := 0
	for _, err := range items {
		if err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func unique(orders []shopdal.Order) bool {
	seen := map[uuid.UUID]bool{}
	for _, o := range orders {
		if seen[o.ID] {
			return false
		}
		seen[o.ID] = true
	}
	return true
}

func newestFirst(orders []shopdal.Order) bool {
	for i := 1; i < len(orders); i++ {
		if orders[i].CreatedAt.After(orders[i-1].CreatedAt) {
			return false
		}
	}
	return true
}

// printDerivedIndexes lists the indexes dalforge derived and the rpcs each
// one serves.
func printDerivedIndexes(s *steps) error {
	f, err := os.Open("schema/schema.sql")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	var serves string
	shown := 0
	for sc.Scan() {
		line := sc.Text()
		if rpcs, ok := strings.CutPrefix(line, "-- derived for: "); ok {
			serves = rpcs
			continue
		}
		if serves != "" && strings.HasPrefix(line, "CREATE INDEX") {
			fmt.Printf("   • %s\n       serves %s\n", strings.TrimSuffix(line, ";"), serves)
			serves = ""
			shown++
		}
	}
	return s.check(shown > 0, "%d derived indexes", shown, sc.Err())
}

// freshDatabase recreates the demo database and applies the generated
// schema. A real project applies migrations to an existing database instead
// (docs/manual/schema-changes.md).
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
