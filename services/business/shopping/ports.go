package main

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"meshshop/internal/platform"
)

// Ports: what the business layer needs from each domain service. The types
// below are this layer's view of the domain contracts (consumer-driven),
// so a domain service can add fields without breaking us.

type Product struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	PriceCents  int64  `json:"priceCents"`
	Currency    string `json:"currency"`
	Active      bool   `json:"active"`
}

type StockLevel struct {
	ProductID string `json:"productId"`
	Available int    `json:"available"`
}

type ReservationLine struct {
	ProductID string `json:"productId"`
	Quantity  int    `json:"quantity"`
}

type Reservation struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type OrderLine struct {
	ProductID      string `json:"productId"`
	Name           string `json:"name"`
	Quantity       int    `json:"quantity"`
	UnitPriceCents int64  `json:"unitPriceCents"`
}

type Order struct {
	ID            string      `json:"id"`
	CustomerID    string      `json:"customerId"`
	Lines         []OrderLine `json:"lines"`
	TotalCents    int64       `json:"totalCents"`
	Currency      string      `json:"currency"`
	Status        string      `json:"status"`
	ReservationID string      `json:"reservationId,omitempty"`
	CreatedAt     string      `json:"createdAt"`
}

type NewOrder struct {
	CustomerID     string      `json:"customerId"`
	Currency       string      `json:"currency"`
	ReservationID  string      `json:"reservationId"`
	IdempotencyKey string      `json:"idempotencyKey"`
	Lines          []OrderLine `json:"lines"`
}

type Catalog interface {
	List(ctx context.Context) ([]Product, error)
	Get(ctx context.Context, id string) (Product, error)
	GetMany(ctx context.Context, ids []string) ([]Product, error)
}

type Inventory interface {
	Levels(ctx context.Context, productIDs []string) ([]StockLevel, error)
	Reserve(ctx context.Context, ref string, lines []ReservationLine) (Reservation, error)
	Commit(ctx context.Context, reservationID string) error
	Release(ctx context.Context, reservationID string) error
}

type Orders interface {
	Create(ctx context.Context, in NewOrder) (Order, error)
	Get(ctx context.Context, id string) (Order, error)
	ByCustomer(ctx context.Context, customerID string) ([]Order, error)
	// FindByKey returns the order created with this idempotency key, if any.
	FindByKey(ctx context.Context, customerID, key string) (Order, bool, error)
	Confirm(ctx context.Context, id string) (Order, error)
	Cancel(ctx context.Context, id string) error
}

// ---- HTTP adapters ------------------------------------------------------

type catalogClient struct{ c *platform.Client }

func (a catalogClient) List(ctx context.Context) ([]Product, error) {
	var out struct{ Items []Product }
	return out.Items, a.c.Do(ctx, http.MethodGet, "/v1/products", nil, &out)
}

func (a catalogClient) Get(ctx context.Context, id string) (Product, error) {
	var p Product
	return p, a.c.Do(ctx, http.MethodGet, "/v1/products/"+url.PathEscape(id), nil, &p)
}

func (a catalogClient) GetMany(ctx context.Context, ids []string) ([]Product, error) {
	var out struct{ Items []Product }
	q := url.Values{"ids": {strings.Join(ids, ",")}}
	return out.Items, a.c.Do(ctx, http.MethodGet, "/v1/products?"+q.Encode(), nil, &out)
}

type inventoryClient struct{ c *platform.Client }

func (a inventoryClient) Levels(ctx context.Context, ids []string) ([]StockLevel, error) {
	var out struct{ Items []StockLevel }
	q := url.Values{"productIds": {strings.Join(ids, ",")}}
	return out.Items, a.c.Do(ctx, http.MethodGet, "/v1/stock?"+q.Encode(), nil, &out)
}

func (a inventoryClient) Reserve(ctx context.Context, ref string, lines []ReservationLine) (Reservation, error) {
	var r Reservation
	body := map[string]any{"ref": ref, "lines": lines}
	return r, a.c.Do(ctx, http.MethodPost, "/v1/reservations", body, &r)
}

func (a inventoryClient) Commit(ctx context.Context, id string) error {
	return a.c.Do(ctx, http.MethodPost, "/v1/reservations/"+url.PathEscape(id)+"/commit", nil, nil)
}

func (a inventoryClient) Release(ctx context.Context, id string) error {
	return a.c.Do(ctx, http.MethodPost, "/v1/reservations/"+url.PathEscape(id)+"/release", nil, nil)
}

type orderClient struct{ c *platform.Client }

func (a orderClient) Create(ctx context.Context, in NewOrder) (Order, error) {
	var o Order
	return o, a.c.Do(ctx, http.MethodPost, "/v1/orders", in, &o)
}

func (a orderClient) Get(ctx context.Context, id string) (Order, error) {
	var o Order
	return o, a.c.Do(ctx, http.MethodGet, "/v1/orders/"+url.PathEscape(id), nil, &o)
}

func (a orderClient) ByCustomer(ctx context.Context, cid string) ([]Order, error) {
	var out struct{ Items []Order }
	q := url.Values{"customerId": {cid}}
	return out.Items, a.c.Do(ctx, http.MethodGet, "/v1/orders?"+q.Encode(), nil, &out)
}

func (a orderClient) FindByKey(ctx context.Context, cid, key string) (Order, bool, error) {
	var out struct{ Items []Order }
	q := url.Values{"customerId": {cid}, "idempotencyKey": {key}}
	if err := a.c.Do(ctx, http.MethodGet, "/v1/orders?"+q.Encode(), nil, &out); err != nil {
		return Order{}, false, err
	}
	if len(out.Items) == 0 {
		return Order{}, false, nil
	}
	return out.Items[0], true, nil
}

func (a orderClient) Confirm(ctx context.Context, id string) (Order, error) {
	var o Order
	return o, a.c.Do(ctx, http.MethodPost, "/v1/orders/"+url.PathEscape(id)+"/confirm", nil, &o)
}

func (a orderClient) Cancel(ctx context.Context, id string) error {
	return a.c.Do(ctx, http.MethodPost, "/v1/orders/"+url.PathEscape(id)+"/cancel", nil, nil)
}
