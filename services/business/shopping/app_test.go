package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"meshshop/internal/platform"
)

// ---- fakes ---------------------------------------------------------------

type fakeCatalog struct{ products []Product }

func (f *fakeCatalog) List(context.Context) ([]Product, error) { return f.products, nil }
func (f *fakeCatalog) Get(_ context.Context, id string) (Product, error) {
	for _, p := range f.products {
		if p.ID == id {
			return p, nil
		}
	}
	return Product{}, platform.ErrNotFound(id)
}
func (f *fakeCatalog) GetMany(_ context.Context, ids []string) ([]Product, error) {
	var out []Product
	for _, p := range f.products {
		if slices.Contains(ids, p.ID) {
			out = append(out, p)
		}
	}
	return out, nil
}

type fakeInventory struct {
	reserveErr, commitErr error
	calls                 []string
}

func (f *fakeInventory) Levels(_ context.Context, ids []string) ([]StockLevel, error) {
	out := make([]StockLevel, len(ids))
	for i, id := range ids {
		out[i] = StockLevel{ProductID: id, Available: 4}
	}
	return out, nil
}
func (f *fakeInventory) Reserve(context.Context, string, []ReservationLine) (Reservation, error) {
	f.calls = append(f.calls, "reserve")
	return Reservation{ID: "res-1"}, f.reserveErr
}
func (f *fakeInventory) Commit(context.Context, string) error {
	f.calls = append(f.calls, "commit")
	return f.commitErr
}
func (f *fakeInventory) Release(context.Context, string) error {
	f.calls = append(f.calls, "release")
	return nil
}

type fakeOrders struct {
	createErr error
	created   NewOrder
	calls     []string
	byKey     map[string]Order // what FindByKey returns
	findCalls int
}

func (f *fakeOrders) FindByKey(_ context.Context, cid, key string) (Order, bool, error) {
	f.findCalls++
	o, ok := f.byKey[cid+"/"+key]
	return o, ok, nil
}

func (f *fakeOrders) Create(_ context.Context, in NewOrder) (Order, error) {
	f.calls = append(f.calls, "create")
	f.created = in
	var total int64
	for _, l := range in.Lines {
		total += int64(l.Quantity) * l.UnitPriceCents
	}
	return Order{ID: "ord-1", CustomerID: in.CustomerID, Lines: in.Lines, TotalCents: total, Status: "pending"}, f.createErr
}
func (f *fakeOrders) Get(context.Context, string) (Order, error) {
	return Order{ID: "ord-1", CustomerID: "alice"}, nil
}
func (f *fakeOrders) ByCustomer(context.Context, string) ([]Order, error) { return nil, nil }
func (f *fakeOrders) Confirm(_ context.Context, id string) (Order, error) {
	f.calls = append(f.calls, "confirm")
	return Order{ID: id, Status: "confirmed"}, nil
}
func (f *fakeOrders) Cancel(context.Context, string) error {
	f.calls = append(f.calls, "cancel")
	return nil
}

func newApp() (*App, *fakeInventory, *fakeOrders) {
	inv, ord := &fakeInventory{}, &fakeOrders{}
	return &App{
		Catalog: &fakeCatalog{products: []Product{
			{ID: "mug", Name: "Mug", PriceCents: 1000, Currency: "USD", Active: true},
			{ID: "old", Name: "Old", PriceCents: 500, Currency: "USD", Active: false},
		}},
		Inventory: inv,
		Orders:    ord,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, inv, ord
}

// ---- tests ---------------------------------------------------------------

func TestCheckoutHappyPath(t *testing.T) {
	app, inv, ord := newApp()
	res, err := app.Checkout(context.Background(), CheckoutRequest{
		CustomerID: "alice", IdempotencyKey: "key-00000001",
		Items: []CheckoutItem{{"mug", 1}, {"mug", 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if o := res.Order; o.Status != "confirmed" || res.Replayed {
		t.Fatalf("result = %+v", res)
	}
	if got := ord.created.Lines; len(got) != 1 || got[0].Quantity != 3 || got[0].UnitPriceCents != 1000 {
		t.Fatalf("lines should be merged and priced from catalog, got %+v", got)
	}
	if !slices.Equal(inv.calls, []string{"reserve", "commit"}) || !slices.Equal(ord.calls, []string{"create", "confirm"}) {
		t.Fatalf("calls inv=%v ord=%v", inv.calls, ord.calls)
	}
}

func TestCheckoutRejectsInactiveProduct(t *testing.T) {
	app, inv, _ := newApp()
	_, err := app.Checkout(context.Background(), CheckoutRequest{CustomerID: "alice", IdempotencyKey: "key-00000001", Items: []CheckoutItem{{"old", 1}}})
	if !platform.IsStatus(err, http.StatusConflict) {
		t.Fatalf("want 409, got %v", err)
	}
	if len(inv.calls) != 0 {
		t.Fatalf("must not touch inventory, got %v", inv.calls)
	}
}

func TestCheckoutOutOfStockPassesThrough(t *testing.T) {
	app, inv, ord := newApp()
	inv.reserveErr = platform.ErrConflict("insufficient_stock", "nope")
	_, err := app.Checkout(context.Background(), CheckoutRequest{CustomerID: "alice", IdempotencyKey: "key-00000001", Items: []CheckoutItem{{"mug", 9}}})
	if e := platform.AsError(err); e == nil || e.Code != "insufficient_stock" {
		t.Fatalf("want insufficient_stock, got %v", err)
	}
	if len(ord.calls) != 0 {
		t.Fatalf("no order should be created, got %v", ord.calls)
	}
}

func TestCheckoutCompensatesWhenOrderCreateFails(t *testing.T) {
	app, inv, _ := newApp()
	app.Orders.(*fakeOrders).createErr = errors.New("boom")
	if _, err := app.Checkout(context.Background(), CheckoutRequest{CustomerID: "alice", IdempotencyKey: "key-00000001", Items: []CheckoutItem{{"mug", 1}}}); err == nil {
		t.Fatal("want error")
	}
	if !slices.Equal(inv.calls, []string{"reserve", "release"}) {
		t.Fatalf("reservation must be released, got %v", inv.calls)
	}
}

func TestCheckoutCompensatesWhenCommitFails(t *testing.T) {
	app, inv, ord := newApp()
	inv.commitErr = errors.New("boom")
	if _, err := app.Checkout(context.Background(), CheckoutRequest{CustomerID: "alice", IdempotencyKey: "key-00000001", Items: []CheckoutItem{{"mug", 1}}}); err == nil {
		t.Fatal("want error")
	}
	if !slices.Equal(ord.calls, []string{"create", "cancel"}) || !slices.Equal(inv.calls, []string{"reserve", "commit", "release"}) {
		t.Fatalf("calls inv=%v ord=%v", inv.calls, ord.calls)
	}
}

func TestCustomerCannotSeeOthersOrders(t *testing.T) {
	app, _, _ := newApp()
	if _, err := app.CustomerOrder(context.Background(), "mallory", "ord-1"); !platform.IsStatus(err, http.StatusNotFound) {
		t.Fatalf("want 404, got %v", err)
	}
}

func TestBrowseHidesInactive(t *testing.T) {
	app, _, _ := newApp()
	views, err := app.Browse(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].ID != "mug" || !views[0].InStock {
		t.Fatalf("got %+v", views)
	}
}

func checkout(app *App, key string, items ...CheckoutItem) (CheckoutResult, error) {
	return app.Checkout(context.Background(), CheckoutRequest{CustomerID: "alice", IdempotencyKey: key, Items: items})
}

// 4 x 2^62 + 1 used to wrap around to 1 when duplicate lines were merged.
func TestCheckoutRejectsOverflowingQuantities(t *testing.T) {
	app, inv, _ := newApp()
	big := 1 << 62
	_, err := checkout(app, "key-00000001", CheckoutItem{"mug", big}, CheckoutItem{"mug", big}, CheckoutItem{"mug", big}, CheckoutItem{"mug", big}, CheckoutItem{"mug", 1})
	if e := platform.AsError(err); e == nil || e.Code != "cart_limit" {
		t.Fatalf("want cart_limit, got %v", err)
	}
	if len(inv.calls) != 0 {
		t.Fatalf("must not reach inventory, got %v", inv.calls)
	}
}

func TestCheckoutBusinessLimits(t *testing.T) {
	app, _, _ := newApp()
	many := make([]CheckoutItem, MaxCartLines+1)
	for i := range many {
		many[i] = CheckoutItem{"mug", 1}
	}
	cases := map[string][]CheckoutItem{
		"too many units of one product": {{"mug", MaxProductQty}, {"mug", 1}},
		"too many lines":                many,
	}
	for name, items := range cases {
		if _, err := checkout(app, "key-00000001", items...); !platform.IsStatus(err, http.StatusUnprocessableEntity) {
			t.Errorf("%s: want 422, got %v", name, err)
		}
	}
	if _, err := checkout(app, "key-00000001", CheckoutItem{"mug", MaxProductQty}); err != nil {
		t.Fatalf("exactly the limit is fine, got %v", err)
	}
}

func TestCheckoutValidatesIdentifiers(t *testing.T) {
	app, _, _ := newApp()
	for name, req := range map[string]CheckoutRequest{
		"no key":        {CustomerID: "alice", Items: []CheckoutItem{{"mug", 1}}},
		"bad customer":  {CustomerID: "../bob", IdempotencyKey: "key-00000001", Items: []CheckoutItem{{"mug", 1}}},
		"long customer": {CustomerID: strings.Repeat("a", 65), IdempotencyKey: "key-00000001", Items: []CheckoutItem{{"mug", 1}}},
		"bad product":   {CustomerID: "alice", IdempotencyKey: "key-00000001", Items: []CheckoutItem{{"a/b", 1}}},
	} {
		if _, err := app.Checkout(context.Background(), req); !platform.IsStatus(err, http.StatusBadRequest) {
			t.Errorf("%s: want 400, got %v", name, err)
		}
	}
}

func TestCheckoutReplaysSameKey(t *testing.T) {
	app, inv, ord := newApp()
	prev := Order{ID: "ord-1", CustomerID: "alice", Status: "confirmed", Lines: []OrderLine{{ProductID: "mug", Quantity: 2}}}
	ord.byKey = map[string]Order{"alice/key-00000001": prev}

	res, err := checkout(app, "key-00000001", CheckoutItem{"mug", 1}, CheckoutItem{"mug", 1})
	if err != nil || !res.Replayed || res.Order.ID != "ord-1" {
		t.Fatalf("want replay of ord-1, got %+v %v", res, err)
	}
	if len(inv.calls) != 0 || len(ord.calls) != 0 {
		t.Fatalf("a replay must not run the saga: inv=%v ord=%v", inv.calls, ord.calls)
	}
	if _, err := checkout(app, "key-00000001", CheckoutItem{"mug", 3}); !platform.IsStatus(err, http.StatusUnprocessableEntity) {
		t.Fatalf("same key, different bag: want 422, got %v", err)
	}
}

// Two concurrent checkouts with one key: the loser's order create is refused,
// it releases its reservation and returns the winner's order.
func TestCheckoutConcurrentDuplicateKey(t *testing.T) {
	app, inv, ord := newApp()
	ord.createErr = platform.ErrConflict("duplicate_order", "exists")
	winner := Order{ID: "ord-7", CustomerID: "alice", Status: "confirmed", Lines: []OrderLine{{ProductID: "mug", Quantity: 1}}}
	find := 0
	app.Orders = &findAfter{fakeOrders: ord, n: &find, then: winner}

	res, err := checkout(app, "key-00000001", CheckoutItem{"mug", 1})
	if err != nil || !res.Replayed || res.Order.ID != "ord-7" {
		t.Fatalf("want winner's order, got %+v %v", res, err)
	}
	if !slices.Equal(inv.calls, []string{"reserve", "release"}) {
		t.Fatalf("loser must release its stock, got %v", inv.calls)
	}
}

// findAfter finds nothing on the first lookup and the winner's order after.
type findAfter struct {
	*fakeOrders
	n    *int
	then Order
}

func (f *findAfter) FindByKey(context.Context, string, string) (Order, bool, error) {
	*f.n++
	return f.then, *f.n > 1, nil
}
