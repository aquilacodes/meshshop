package main

import (
	"net/http"
	"testing"
	"time"

	"meshshop/internal/platform"
)

func TestPlaceComputesTotal(t *testing.T) {
	o, err := Place("ord-1", NewOrder{
		CustomerID: "c-1", Currency: "USD",
		Lines: []Line{{ProductID: "a", Quantity: 2, UnitPriceCents: 150}, {ProductID: "b", Quantity: 1, UnitPriceCents: 99}},
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if o.TotalCents != 399 || o.Status != Pending {
		t.Fatalf("got total=%d status=%s", o.TotalCents, o.Status)
	}
}

func TestPlaceRejectsInvalid(t *testing.T) {
	cases := map[string]NewOrder{
		"no customer":  {Currency: "USD", Lines: []Line{{ProductID: "a", Quantity: 1}}},
		"no lines":     {CustomerID: "c", Currency: "USD"},
		"zero qty":     {CustomerID: "c", Currency: "USD", Lines: []Line{{ProductID: "a", Quantity: 0}}},
		"bad customer": {CustomerID: "a/../b", Currency: "USD", Lines: []Line{{ProductID: "a", Quantity: 1}}},
	}
	for name, in := range cases {
		if _, err := Place("x", in, time.Now()); !platform.IsStatus(err, http.StatusBadRequest) {
			t.Errorf("%s: want 400, got %v", name, err)
		}
	}
}

func TestLifecycle(t *testing.T) {
	o := Order{ID: "o", Status: Pending}
	if err := o.Transition(Confirmed, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := o.Transition(Confirmed, time.Now()); err != nil {
		t.Fatalf("repeat confirm should be a no-op, got %v", err)
	}
	if err := o.Transition(Cancelled, time.Now()); !platform.IsStatus(err, http.StatusConflict) {
		t.Fatalf("cancel after confirm must conflict, got %v", err)
	}
}

func TestPlaceRejectsOverflowingTotal(t *testing.T) {
	_, err := Place("x", NewOrder{CustomerID: "c", Currency: "USD",
		Lines: []Line{{ProductID: "a", Quantity: maxLineQty, UnitPriceCents: 1 << 62}}}, time.Now())
	if !platform.IsStatus(err, http.StatusBadRequest) {
		t.Fatalf("want 400, got %v", err)
	}
	if _, err := Place("x", NewOrder{CustomerID: "c", Currency: "USD",
		Lines: []Line{{ProductID: "a", Quantity: maxLineQty + 1, UnitPriceCents: 1}}}, time.Now()); err == nil {
		t.Fatal("quantity above the bound must be rejected")
	}
}

func TestIdempotencyKeyIsUniquePerCustomer(t *testing.T) {
	s := NewStore()
	in := NewOrder{CustomerID: "alice", Currency: "USD", IdempotencyKey: "key-00000001",
		Lines: []Line{{ProductID: "a", Quantity: 1, UnitPriceCents: 100}}}
	first, err := s.Create(in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(in); err == nil || platform.AsError(err).Code != "duplicate_order" {
		t.Fatalf("second create with the same key: want duplicate_order, got %v", err)
	}
	if got, ok := s.ByKey("alice", "key-00000001"); !ok || got.ID != first.ID {
		t.Fatalf("ByKey = %+v %v", got, ok)
	}
	in.CustomerID = "bob" // keys are scoped to a customer
	if _, err := s.Create(in); err != nil {
		t.Fatalf("another customer may use the same key: %v", err)
	}
}

func TestOrderIDsAreRandom(t *testing.T) {
	s := NewStore()
	in := NewOrder{CustomerID: "alice", Currency: "USD", Lines: []Line{{ProductID: "a", Quantity: 1}}}
	a, _ := s.Create(in)
	b, _ := s.Create(in)
	if a.ID == b.ID || len(a.ID) < 20 || a.ID == "ord-1001" {
		t.Fatalf("ids %s %s", a.ID, b.ID)
	}
}
