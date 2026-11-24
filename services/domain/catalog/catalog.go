package main

import (
	"slices"
	"sync"

	"meshshop/internal/platform"
)

// Product is the catalog's aggregate. The catalog is the source of truth for
// what can be sold and at what price; it knows nothing about stock or orders.
type Product struct {
	ID          string `json:"id"`
	SKU         string `json:"sku"`
	Name        string `json:"name"`
	Description string `json:"description"`
	PriceCents  int64  `json:"priceCents"`
	Currency    string `json:"currency"`
	Active      bool   `json:"active"`
}

type Store struct {
	mu       sync.RWMutex
	products map[string]Product
}

func NewStore(seed []Product) *Store {
	s := &Store{products: map[string]Product{}}
	for _, p := range seed {
		s.products[p.ID] = p
	}
	return s
}

func (s *Store) List() []Product {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Product, 0, len(s.products))
	for _, p := range s.products {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b Product) int { return compare(a.ID, b.ID) })
	return out
}

func (s *Store) Get(id string) (Product, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.products[id]
	if !ok {
		return Product{}, platform.ErrNotFound("product " + id + " not found")
	}
	return p, nil
}

func compare(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

var seedProducts = []Product{
	{ID: "p-100", SKU: "MUG-CER-01", Name: "Ceramic Mug", Description: "350ml stoneware mug", PriceCents: 1299, Currency: "USD", Active: true},
	{ID: "p-101", SKU: "TEE-ORG-M", Name: "Organic Tee", Description: "Cotton t-shirt, size M", PriceCents: 2499, Currency: "USD", Active: true},
	{ID: "p-102", SKU: "BTL-STL-75", Name: "Steel Bottle", Description: "750ml insulated bottle", PriceCents: 3199, Currency: "USD", Active: true},
	{ID: "p-103", SKU: "NTB-A5-DOT", Name: "Dotted Notebook", Description: "A5, 120 pages", PriceCents: 899, Currency: "USD", Active: true},
	{ID: "p-104", SKU: "CAP-WOOL-OS", Name: "Wool Cap", Description: "Discontinued", PriceCents: 1999, Currency: "USD", Active: false},
}
