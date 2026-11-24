package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"meshshop/internal/platform"
)

// maxQty bounds the quantity of one product in a reservation, so merging lines
// can never overflow.
const maxQty = 1000

// Inventory domain: stock levels and reservations.
//
// Invariants:
//   - available never goes below zero
//   - a reservation is all-or-nothing across its lines
//   - a reservation moves held -> committed or held -> released, never back
//   - a held reservation expires after the store's TTL and is released, so a
//     crashed or abandoned checkout cannot lock stock forever

type StockLevel struct {
	ProductID string `json:"productId"`
	Available int    `json:"available"` // free to reserve
	Reserved  int    `json:"reserved"`  // held by open reservations
}

type Line struct {
	ProductID string `json:"productId"`
	Quantity  int    `json:"quantity"`
}

type ReservationStatus string

const (
	Held      ReservationStatus = "held"
	Committed ReservationStatus = "committed"
	Released  ReservationStatus = "released"
)

type Reservation struct {
	ID        string            `json:"id"`
	Ref       string            `json:"ref"` // caller's correlation id (e.g. checkout id)
	Lines     []Line            `json:"lines"`
	Status    ReservationStatus `json:"status"`
	ExpiresAt time.Time         `json:"expiresAt"`
}

type Store struct {
	mu           sync.Mutex
	stock        map[string]*StockLevel
	reservations map[string]*Reservation
	ttl          time.Duration // how long stock stays held without a commit
	now          func() time.Time
}

// DefaultHoldTTL is far longer than a checkout takes (a few hundred ms).
const DefaultHoldTTL = 2 * time.Minute

func NewStore(seed map[string]int) *Store {
	s := &Store{stock: map[string]*StockLevel{}, reservations: map[string]*Reservation{},
		ttl: DefaultHoldTTL, now: time.Now}
	for id, qty := range seed {
		s.stock[id] = &StockLevel{ProductID: id, Available: qty}
	}
	return s
}

func (s *Store) Levels(ids []string) []StockLevel {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]StockLevel, 0, len(ids))
	for _, id := range ids {
		if lvl, ok := s.stock[id]; ok {
			out = append(out, *lvl)
		} else {
			out = append(out, StockLevel{ProductID: id})
		}
	}
	return out
}

// Reserve holds stock for every line or for none of them.
func (s *Store) Reserve(ref string, lines []Line) (Reservation, error) {
	if len(lines) == 0 {
		return Reservation{}, platform.ErrBadRequest("reservation needs at least one line")
	}
	merged := map[string]int{}
	for _, l := range lines {
		if l.Quantity <= 0 || l.Quantity > maxQty {
			return Reservation{}, platform.ErrBadRequest(fmt.Sprintf("quantity must be 1 to %d for %s", maxQty, l.ProductID))
		}
		if merged[l.ProductID] > maxQty-l.Quantity { // checked addition
			return Reservation{}, platform.ErrBadRequest(fmt.Sprintf("total quantity must be at most %d for %s", maxQty, l.ProductID))
		}
		merged[l.ProductID] += l.Quantity
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()

	// The message names products but not stock levels: those are commercial data.
	var short []string
	for id, qty := range merged {
		if lvl, ok := s.stock[id]; !ok || lvl.Available < qty {
			short = append(short, id)
		}
	}
	if len(short) > 0 {
		slices.Sort(short)
		return Reservation{}, platform.ErrConflict("insufficient_stock", "not enough stock for "+strings.Join(short, ", "))
	}

	for id, qty := range merged {
		s.stock[id].Available -= qty
		s.stock[id].Reserved += qty
	}
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	r := &Reservation{
		ID:        "res-" + hex.EncodeToString(b),
		Ref:       ref,
		Lines:     lines,
		Status:    Held,
		ExpiresAt: s.now().Add(s.ttl).UTC(),
	}
	s.reservations[r.ID] = r
	return *r, nil
}

// ExpireHeld releases every held reservation past its TTL and returns how many
// it released. main runs it on a ticker; Reserve and Commit also apply it.
func (s *Store) ExpireHeld() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.expireLocked()
}

func (s *Store) expireLocked() int {
	n, now := 0, s.now()
	for _, r := range s.reservations {
		if r.Status == Held && now.After(r.ExpiresAt) {
			for _, l := range r.Lines {
				s.stock[l.ProductID].Reserved -= l.Quantity
				s.stock[l.ProductID].Available += l.Quantity
			}
			r.Status = Released
			n++
		}
	}
	return n
}

// Commit turns held stock into a permanent deduction.
func (s *Store) Commit(id string) (Reservation, error) {
	return s.transition(id, Committed, func(l Line) {
		s.stock[l.ProductID].Reserved -= l.Quantity
	})
}

// Release returns held stock to available.
func (s *Store) Release(id string) (Reservation, error) {
	return s.transition(id, Released, func(l Line) {
		s.stock[l.ProductID].Reserved -= l.Quantity
		s.stock[l.ProductID].Available += l.Quantity
	})
}

func (s *Store) transition(id string, to ReservationStatus, apply func(Line)) (Reservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	r, ok := s.reservations[id]
	if !ok {
		return Reservation{}, platform.ErrNotFound("reservation " + id + " not found")
	}
	if r.Status == to {
		return *r, nil // idempotent: safe to retry
	}
	if r.Status == Released && to == Committed && s.now().After(r.ExpiresAt) {
		return Reservation{}, platform.ErrConflict("reservation_expired", "reservation "+id+" expired before it was committed")
	}
	if r.Status != Held {
		return Reservation{}, platform.ErrConflict("invalid_transition",
			fmt.Sprintf("reservation %s is %s, cannot become %s", id, r.Status, to))
	}
	for _, l := range r.Lines {
		apply(l)
	}
	r.Status = to
	return *r, nil
}

var seedStock = map[string]int{
	"p-100": 25,
	"p-101": 10,
	"p-102": 3,
	"p-103": 0,
	"p-104": 7,
}
