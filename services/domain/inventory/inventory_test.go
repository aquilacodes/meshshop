package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"meshshop/internal/platform"
)

func TestReserveIsAllOrNothing(t *testing.T) {
	s := NewStore(map[string]int{"a": 5, "b": 1})

	_, err := s.Reserve("chk-1", []Line{{"a", 2}, {"b", 2}})
	if !platform.IsStatus(err, http.StatusConflict) {
		t.Fatalf("want 409 insufficient stock, got %v", err)
	}
	lv := s.Levels([]string{"a", "b"})
	if lv[0].Available != 5 || lv[1].Available != 1 {
		t.Fatalf("failed reservation must not change stock, got %+v", lv)
	}
}

func TestReserveMergesDuplicateLines(t *testing.T) {
	s := NewStore(map[string]int{"a": 3})
	if _, err := s.Reserve("chk", []Line{{"a", 2}, {"a", 2}}); !platform.IsStatus(err, http.StatusConflict) {
		t.Fatalf("2+2 of a with 3 in stock must conflict, got %v", err)
	}
}

func TestCommitAndRelease(t *testing.T) {
	s := NewStore(map[string]int{"a": 5})

	r1, err := s.Reserve("chk-1", []Line{{"a", 2}})
	if err != nil {
		t.Fatal(err)
	}
	if lv := s.Levels([]string{"a"})[0]; lv.Available != 3 || lv.Reserved != 2 {
		t.Fatalf("after reserve: %+v", lv)
	}
	if _, err := s.Commit(r1.ID); err != nil {
		t.Fatal(err)
	}
	if lv := s.Levels([]string{"a"})[0]; lv.Available != 3 || lv.Reserved != 0 {
		t.Fatalf("after commit: %+v", lv)
	}

	r2, _ := s.Reserve("chk-2", []Line{{"a", 1}})
	if _, err := s.Release(r2.ID); err != nil {
		t.Fatal(err)
	}
	if lv := s.Levels([]string{"a"})[0]; lv.Available != 3 || lv.Reserved != 0 {
		t.Fatalf("after release: %+v", lv)
	}

	// idempotent repeat, illegal cross-transition
	if _, err := s.Release(r2.ID); err != nil {
		t.Fatalf("repeat release should be a no-op, got %v", err)
	}
	if _, err := s.Commit(r2.ID); !platform.IsStatus(err, http.StatusConflict) {
		t.Fatalf("commit after release must conflict, got %v", err)
	}
}

func TestReserveRejectsOverflow(t *testing.T) {
	s := NewStore(map[string]int{"a": 5})
	big := 1 << 62
	_, err := s.Reserve("chk", []Line{{"a", big}, {"a", big}, {"a", big}, {"a", big}, {"a", 1}})
	if !platform.IsStatus(err, http.StatusBadRequest) {
		t.Fatalf("want 400, got %v", err)
	}
	if _, err := s.Reserve("chk", []Line{{"a", maxQty}, {"a", 1}}); !platform.IsStatus(err, http.StatusBadRequest) {
		t.Fatalf("merged quantity above the bound: want 400, got %v", err)
	}
}

func TestInsufficientStockHidesLevels(t *testing.T) {
	s := NewStore(map[string]int{"a": 3})
	_, err := s.Reserve("chk", []Line{{"a", 4}})
	if e := platform.AsError(err); e == nil || strings.Contains(e.Message, "3") || !strings.Contains(e.Message, "a") {
		t.Fatalf("message should name the product but not the stock level: %v", err)
	}
}

func TestHeldReservationsExpire(t *testing.T) {
	now := time.Unix(1000, 0)
	s := NewStore(map[string]int{"a": 5})
	s.now = func() time.Time { return now }

	r, err := s.Reserve("chk", []Line{{"a", 4}})
	if err != nil {
		t.Fatal(err)
	}
	if r.ID == "res-1" || len(r.ID) < 20 {
		t.Fatalf("ids must be random, got %s", r.ID)
	}
	now = now.Add(DefaultHoldTTL + time.Second)
	if n := s.ExpireHeld(); n != 1 {
		t.Fatalf("want 1 expired, got %d", n)
	}
	if lv := s.Levels([]string{"a"})[0]; lv.Available != 5 || lv.Reserved != 0 {
		t.Fatalf("expired hold must return stock, got %+v", lv)
	}
	if _, err := s.Commit(r.ID); err == nil || platform.AsError(err).Code != "reservation_expired" {
		t.Fatalf("commit after expiry: want reservation_expired, got %v", err)
	}
}
