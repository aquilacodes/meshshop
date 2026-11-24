package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"slices"
	"sync"
	"time"

	"meshshop/internal/platform"
)

// maxLineQty bounds a single line. The business layer applies a much smaller
// customer-facing limit; this one only keeps the arithmetic below safe.
const maxLineQty = 1000

// Order domain: the Order aggregate and its lifecycle.
//
//	pending --confirm--> confirmed
//	pending --cancel---> cancelled
//
// Confirmed and cancelled are terminal. The order stores the price it was sold
// at; it does not look prices up itself (that is the business layer's job).

type Status string

const (
	Pending   Status = "pending"
	Confirmed Status = "confirmed"
	Cancelled Status = "cancelled"
)

type Line struct {
	ProductID      string `json:"productId"`
	Name           string `json:"name"`
	Quantity       int    `json:"quantity"`
	UnitPriceCents int64  `json:"unitPriceCents"`
}

type Order struct {
	ID            string `json:"id"`
	CustomerID    string `json:"customerId"`
	Lines         []Line `json:"lines"`
	TotalCents    int64  `json:"totalCents"`
	Currency      string `json:"currency"`
	Status        Status `json:"status"`
	ReservationID string `json:"reservationId,omitempty"`
	// IdempotencyKey is the client's key for the checkout that created the order.
	// At most one order exists per (customer, key).
	IdempotencyKey string    `json:"idempotencyKey,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

type NewOrder struct {
	CustomerID     string `json:"customerId"`
	Currency       string `json:"currency"`
	ReservationID  string `json:"reservationId"`
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
	Lines          []Line `json:"lines"`
}

// Place validates a new order and computes its total.
func Place(id string, in NewOrder, now time.Time) (Order, error) {
	if !platform.ValidID(in.CustomerID) {
		return Order{}, platform.ErrBadRequest("customerId is missing or malformed")
	}
	if in.IdempotencyKey != "" && !platform.ValidID(in.IdempotencyKey) {
		return Order{}, platform.ErrBadRequest("idempotencyKey is malformed")
	}
	if in.Currency == "" {
		return Order{}, platform.ErrBadRequest("currency is required")
	}
	if len(in.Lines) == 0 {
		return Order{}, platform.ErrBadRequest("an order needs at least one line")
	}
	var total int64
	for _, l := range in.Lines {
		if l.Quantity <= 0 || l.Quantity > maxLineQty || l.UnitPriceCents < 0 {
			return Order{}, platform.ErrBadRequest(fmt.Sprintf("invalid line for %s", l.ProductID))
		}
		// Checked arithmetic: a total must never wrap around.
		if l.UnitPriceCents > 0 && int64(l.Quantity) > (math.MaxInt64-total)/l.UnitPriceCents {
			return Order{}, platform.ErrBadRequest("order total is too large")
		}
		total += int64(l.Quantity) * l.UnitPriceCents
	}
	return Order{
		ID:             id,
		CustomerID:     in.CustomerID,
		Lines:          in.Lines,
		TotalCents:     total,
		Currency:       in.Currency,
		Status:         Pending,
		ReservationID:  in.ReservationID,
		IdempotencyKey: in.IdempotencyKey,
		CreatedAt:      now,
		UpdatedAt:      now,
	}, nil
}

// Transition moves the order to a new status if the lifecycle allows it.
// Repeating the current status is a no-op, so callers can retry safely.
func (o *Order) Transition(to Status, now time.Time) error {
	if o.Status == to {
		return nil
	}
	if o.Status != Pending {
		return platform.ErrConflict("invalid_transition",
			fmt.Sprintf("order %s is %s, cannot become %s", o.ID, o.Status, to))
	}
	o.Status = to
	o.UpdatedAt = now
	return nil
}

type Store struct {
	mu     sync.RWMutex
	orders map[string]*Order
	byKey  map[string]string // customerId + "/" + idempotencyKey -> order id
	now    func() time.Time
}

func NewStore() *Store {
	return &Store{orders: map[string]*Order{}, byKey: map[string]string{}, now: time.Now}
}

// newOrderID returns an unguessable id, so ids leak neither order volume nor
// each other (ownership is still checked on every read).
func newOrderID() string {
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	return "ord-" + hex.EncodeToString(b)
}

func (s *Store) Create(in NewOrder) (Order, error) {
	o, err := Place(newOrderID(), in, s.now().UTC())
	if err != nil {
		return Order{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if o.IdempotencyKey != "" {
		k := o.CustomerID + "/" + o.IdempotencyKey
		if _, dup := s.byKey[k]; dup {
			return Order{}, platform.ErrConflict("duplicate_order", "an order with this idempotency key already exists")
		}
		s.byKey[k] = o.ID
	}
	s.orders[o.ID] = &o
	return o, nil
}

// ByKey returns the order a checkout with this idempotency key created, if any.
func (s *Store) ByKey(customerID, key string) (Order, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byKey[customerID+"/"+key]
	if !ok {
		return Order{}, false
	}
	return *s.orders[id], true
}

func (s *Store) Get(id string) (Order, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.orders[id]
	if !ok {
		return Order{}, platform.ErrNotFound("order " + id + " not found")
	}
	return *o, nil
}

func (s *Store) ByCustomer(customerID string) []Order {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []Order{}
	for _, o := range s.orders {
		if o.CustomerID == customerID {
			out = append(out, *o)
		}
	}
	slices.SortFunc(out, func(a, b Order) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out
}

func (s *Store) Transition(id string, to Status) (Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok {
		return Order{}, platform.ErrNotFound("order " + id + " not found")
	}
	if err := o.Transition(to, s.now().UTC()); err != nil {
		return Order{}, err
	}
	return *o, nil
}
