package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/otel/attribute"

	"meshshop/internal/platform"
)

// App holds the shopping use cases. It coordinates the domain services and
// owns cross-domain rules (e.g. "an inactive product cannot be bought",
// "a customer may only see their own orders"). It holds no data of its own.
type App struct {
	Catalog   Catalog
	Inventory Inventory
	Orders    Orders
	Log       *slog.Logger
}

// ProductView is a product joined with its stock level.
type ProductView struct {
	Product
	Available int  `json:"available"`
	InStock   bool `json:"inStock"`
}

// Browse lists sellable products with stock.
func (a *App) Browse(ctx context.Context) ([]ProductView, error) {
	products, err := a.Catalog.List(ctx)
	if err != nil {
		return nil, err
	}
	active := products[:0]
	for _, p := range products {
		if p.Active {
			active = append(active, p)
		}
	}
	return a.withStock(ctx, active)
}

func (a *App) Product(ctx context.Context, id string) (ProductView, error) {
	p, err := a.Catalog.Get(ctx, id)
	if err != nil {
		return ProductView{}, err
	}
	if !p.Active {
		return ProductView{}, platform.ErrNotFound("product " + id + " not found")
	}
	views, err := a.withStock(ctx, []Product{p})
	if err != nil {
		return ProductView{}, err
	}
	return views[0], nil
}

func (a *App) withStock(ctx context.Context, products []Product) ([]ProductView, error) {
	views := make([]ProductView, len(products))
	if len(products) == 0 {
		return views, nil
	}
	ids := make([]string, len(products))
	for i, p := range products {
		ids[i] = p.ID
	}
	levels, err := a.Inventory.Levels(ctx, ids)
	if err != nil {
		return nil, err
	}
	avail := map[string]int{}
	for _, l := range levels {
		avail[l.ProductID] = l.Available
	}
	for i, p := range products {
		views[i] = ProductView{Product: p, Available: avail[p.ID], InStock: avail[p.ID] > 0}
	}
	return views, nil
}

type CheckoutItem struct {
	ProductID string `json:"productId"`
	Quantity  int    `json:"quantity"`
}

type CheckoutRequest struct {
	CustomerID     string         `json:"customerId"`
	IdempotencyKey string         `json:"idempotencyKey"`
	Items          []CheckoutItem `json:"items"`
}

// CheckoutResult is the order a checkout produced. Replayed is true when the
// same idempotency key was used before and the earlier order is returned.
type CheckoutResult struct {
	Order    Order `json:"order"`
	Replayed bool  `json:"replayed"`
}

// Business limits on a single checkout. They stop one client from emptying the
// shelves (or overflowing the arithmetic) in one request.
const (
	MaxCartLines  = 20 // lines as sent by the client
	MaxProductQty = 10 // units of one product, after merging duplicate lines
)

// Checkout places an order. It is a small orchestrated saga:
//
//  1. price the cart from the catalog (the catalog's price wins, never the client's)
//  2. reserve stock (all-or-nothing)            compensation: release reservation
//  3. create a pending order                    compensation: cancel order
//  4. commit the reservation
//  5. confirm the order
//
// In production you would persist saga state (or use an outbox / workflow
// engine) so a crash between steps can be resumed. Here compensation is
// best-effort and in-process.
func (a *App) Checkout(ctx context.Context, req CheckoutRequest) (CheckoutResult, error) {
	qty, ids, err := validateCheckout(req)
	if err != nil {
		return CheckoutResult{}, err
	}

	// Idempotency: a retried checkout (same customer, same key) returns the
	// order the first attempt created instead of buying everything twice.
	if prev, ok, err := a.Orders.FindByKey(ctx, req.CustomerID, req.IdempotencyKey); err != nil {
		return CheckoutResult{}, err
	} else if ok {
		return replay(prev, qty)
	}

	o, err := a.checkout(ctx, req, qty, ids)
	if e := platform.AsError(err); e != nil && e.Code == "duplicate_order" {
		// A concurrent request with the same key won the race; our saga has
		// already compensated. Return the winner's order.
		if prev, ok, ferr := a.Orders.FindByKey(ctx, req.CustomerID, req.IdempotencyKey); ferr == nil && ok {
			return replay(prev, qty)
		}
	}
	return CheckoutResult{Order: o}, err
}

// validateCheckout applies the business limits and merges duplicate lines with
// checked addition. It returns quantities per product and products in cart order.
func validateCheckout(req CheckoutRequest) (map[string]int, []string, error) {
	if !platform.ValidID(req.CustomerID) {
		return nil, nil, platform.ErrBadRequest("customerId is missing or malformed")
	}
	if len(req.IdempotencyKey) < 8 || !platform.ValidID(req.IdempotencyKey) {
		return nil, nil, platform.ErrBadRequest("idempotencyKey is missing or malformed")
	}
	if len(req.Items) == 0 {
		return nil, nil, platform.ErrBadRequest("cart is empty")
	}
	if len(req.Items) > MaxCartLines {
		return nil, nil, platform.ErrUnprocessable("cart_limit", fmt.Sprintf("a cart holds at most %d lines", MaxCartLines))
	}
	qty := map[string]int{}
	var ids []string
	for _, it := range req.Items {
		if !platform.ValidID(it.ProductID) {
			return nil, nil, platform.ErrBadRequest("invalid productId")
		}
		if it.Quantity <= 0 {
			return nil, nil, platform.ErrBadRequest("quantity must be positive for " + it.ProductID)
		}
		if _, seen := qty[it.ProductID]; !seen {
			ids = append(ids, it.ProductID)
		}
		// Checked addition: compare before adding, so the sum can never wrap.
		if it.Quantity > MaxProductQty-qty[it.ProductID] {
			return nil, nil, platform.ErrUnprocessable("cart_limit",
				fmt.Sprintf("at most %d of %s per order", MaxProductQty, it.ProductID))
		}
		qty[it.ProductID] += it.Quantity
	}
	return qty, ids, nil
}

// replay returns an earlier order for a repeated idempotency key, after checking
// the key is not being reused for a different cart.
func replay(prev Order, qty map[string]int) (CheckoutResult, error) {
	same := len(prev.Lines) == len(qty)
	for _, l := range prev.Lines {
		same = same && qty[l.ProductID] == l.Quantity
	}
	if !same {
		return CheckoutResult{}, platform.ErrUnprocessable("idempotency_key_reused",
			"this Idempotency-Key was already used for a different bag")
	}
	return CheckoutResult{Order: prev, Replayed: true}, nil
}

// checkout runs the saga.
func (a *App) checkout(ctx context.Context, req CheckoutRequest, qty map[string]int, ids []string) (Order, error) {
	checkoutID := newID("chk")
	log := a.Log.With("checkout_id", checkoutID, "customer_id", req.CustomerID)
	platform.Annotate(ctx, attribute.String("checkout.id", checkoutID), attribute.String("enduser.id", req.CustomerID))

	// 1. price
	var lines []OrderLine
	var resLines []ReservationLine
	var currency string
	err := platform.Step(ctx, "checkout.price_cart", func(ctx context.Context) error {
		products, err := a.Catalog.GetMany(ctx, ids)
		if err != nil {
			return err
		}
		byID := map[string]Product{}
		for _, p := range products {
			byID[p.ID] = p
		}
		for _, id := range ids {
			p, ok := byID[id]
			if !ok || !p.Active {
				return platform.ErrConflict("product_unavailable", "product "+id+" is not available")
			}
			if currency == "" {
				currency = p.Currency
			} else if currency != p.Currency {
				return platform.ErrConflict("mixed_currency", "cart mixes currencies")
			}
			lines = append(lines, OrderLine{ProductID: id, Name: p.Name, Quantity: qty[id], UnitPriceCents: p.PriceCents})
			resLines = append(resLines, ReservationLine{ProductID: id, Quantity: qty[id]})
		}
		return nil
	}, attribute.Int("checkout.distinct_items", len(ids)))
	if err != nil {
		log.WarnContext(ctx, "checkout rejected while pricing", "err", err)
		return Order{}, err
	}

	// 2. reserve
	var res Reservation
	err = platform.Step(ctx, "checkout.reserve_stock", func(ctx context.Context) (err error) {
		res, err = a.Inventory.Reserve(ctx, checkoutID, resLines)
		return err
	})
	if err != nil {
		log.WarnContext(ctx, "checkout rejected: stock reservation failed", "err", err)
		return Order{}, err // nothing to compensate
	}

	// 3. create order
	var order Order
	err = platform.Step(ctx, "checkout.create_order", func(ctx context.Context) (err error) {
		order, err = a.Orders.Create(ctx, NewOrder{
			CustomerID: req.CustomerID, Currency: currency, ReservationID: res.ID,
			IdempotencyKey: req.IdempotencyKey, Lines: lines,
		})
		return err
	})
	if err != nil {
		a.compensate(ctx, log, res.ID, "", err)
		return Order{}, err
	}
	platform.Annotate(ctx, attribute.String("order.id", order.ID))

	// 4. commit stock
	err = platform.Step(ctx, "checkout.commit_stock", func(ctx context.Context) error {
		return a.Inventory.Commit(ctx, res.ID)
	})
	if err != nil {
		a.compensate(ctx, log, res.ID, order.ID, err)
		return Order{}, err
	}

	// 5. confirm
	var confirmed Order
	err = platform.Step(ctx, "checkout.confirm_order", func(ctx context.Context) (err error) {
		confirmed, err = a.Orders.Confirm(ctx, order.ID)
		return err
	})
	if err != nil {
		// Stock is already committed, so we cannot simply roll back. Surface the
		// pending order; a reconciler would retry the confirm.
		log.ErrorContext(ctx, "confirm failed after stock commit; order left pending", "order_id", order.ID, "err", err)
		return Order{}, &platform.Error{Status: http.StatusAccepted, Code: "order_pending",
			Message: fmt.Sprintf("order %s accepted but not yet confirmed", order.ID), Cause: err}
	}
	log.InfoContext(ctx, "checkout complete", "order_id", confirmed.ID, "total_cents", confirmed.TotalCents)
	return confirmed, nil
}

// compensate undoes earlier saga steps. It runs on a context that is not
// cancelled with the caller's request, so it still runs if the client goes away.
func (a *App) compensate(ctx context.Context, log *slog.Logger, reservationID, orderID string, cause error) {
	log.WarnContext(ctx, "checkout failed, compensating", "reservation_id", reservationID, "order_id", orderID, "err", cause)
	_ = platform.Step(context.WithoutCancel(ctx), "checkout.compensate", func(ctx context.Context) error {
		var failed error
		if orderID != "" {
			if err := a.Orders.Cancel(ctx, orderID); err != nil {
				log.ErrorContext(ctx, "compensation: cancel order failed", "order_id", orderID, "err", err)
				failed = err
			}
		}
		if err := a.Inventory.Release(ctx, reservationID); err != nil {
			log.ErrorContext(ctx, "compensation: release reservation failed", "reservation_id", reservationID, "err", err)
			failed = err
		}
		return failed
	})
}

// CustomerOrders lists a customer's orders.
func (a *App) CustomerOrders(ctx context.Context, customerID string) ([]Order, error) {
	if !platform.ValidID(customerID) {
		return nil, platform.ErrBadRequest("customerId is malformed")
	}
	return a.Orders.ByCustomer(ctx, customerID)
}

// CustomerOrder returns one order, hiding orders that belong to someone else.
func (a *App) CustomerOrder(ctx context.Context, customerID, orderID string) (Order, error) {
	if !platform.ValidID(customerID) || !platform.ValidID(orderID) {
		return Order{}, platform.ErrNotFound("order not found")
	}
	platform.Annotate(ctx, attribute.String("enduser.id", customerID), attribute.String("order.id", orderID))
	o, err := a.Orders.Get(ctx, orderID)
	if platform.IsStatus(err, http.StatusNotFound) {
		return Order{}, platform.ErrNotFound("order " + orderID + " not found")
	}
	if err != nil {
		return Order{}, err
	}
	if o.CustomerID != customerID {
		return Order{}, platform.ErrNotFound("order " + orderID + " not found")
	}
	return o, nil
}

func newID(prefix string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}
