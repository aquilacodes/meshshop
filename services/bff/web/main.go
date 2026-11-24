// web-bff is the Backend-for-Frontend for the web storefront. It is the only
// service exposed through the Istio ingress gateway. It:
//
//   - establishes who the caller is from a Keycloak access token (OIDC/JWT)
//   - shapes business-layer data for the web UI (formatted prices, badges)
//   - turns errors into a small set of client-safe messages
//   - calls only the business layer, never domain services
//
// A mobile app would get its own BFF (e.g. services/bff/mobile) over the same
// business layer, with a different response shape.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"meshshop/internal/platform"
)

func main() {
	svc := platform.NewService("web-bff", "bff").Edge()
	verifier, err := newOIDCVerifier(os.Getenv("OIDC_ISSUER"), os.Getenv("OIDC_JWKS_URL"), os.Getenv("OIDC_AUDIENCE"))
	if err != nil {
		svc.Log.Error("auth config", "err", err)
		os.Exit(1)
	}
	register(svc.Mux, deps{
		shopping:  platform.NewClient(platform.Env("SHOPPING_URL", "http://shopping:8080")),
		auth:      verifier,
		log:       svc.Log,
		checkouts: newLimiter(checkoutsPerMin, time.Minute),
	})
	svc.Run()
}

type deps struct {
	shopping  *platform.Client
	auth      tokenVerifier
	log       *slog.Logger
	checkouts *limiter // per-customer checkout rate
}

// ---- business-layer contract (as consumed by this BFF) -------------------

type product struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	PriceCents  int64  `json:"priceCents"`
	Currency    string `json:"currency"`
	Available   int    `json:"available"`
	InStock     bool   `json:"inStock"`
}

type orderLine struct {
	ProductID      string `json:"productId"`
	Name           string `json:"name"`
	Quantity       int    `json:"quantity"`
	UnitPriceCents int64  `json:"unitPriceCents"`
}

type order struct {
	ID         string      `json:"id"`
	Lines      []orderLine `json:"lines"`
	TotalCents int64       `json:"totalCents"`
	Currency   string      `json:"currency"`
	Status     string      `json:"status"`
	CreatedAt  string      `json:"createdAt"`
}

// ---- web view models ------------------------------------------------------

type productCard struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Subtitle    string `json:"subtitle"`
	Price       string `json:"price"`
	Badge       string `json:"badge,omitempty"`
	CanAddToBag bool   `json:"canAddToBag"`
}

type orderSummary struct {
	OrderID   string   `json:"orderId"`
	Status    string   `json:"status"`
	Total     string   `json:"total"`
	ItemCount int      `json:"itemCount"`
	Items     []string `json:"items"`
	PlacedAt  string   `json:"placedAt"`
}

func toCard(p product) productCard {
	c := productCard{
		ID:          p.ID,
		Title:       p.Name,
		Subtitle:    p.Description,
		Price:       money(p.PriceCents, p.Currency),
		CanAddToBag: p.InStock,
	}
	switch {
	case !p.InStock:
		c.Badge = "Sold out"
	case p.Available <= 5:
		c.Badge = fmt.Sprintf("Only %d left", p.Available)
	}
	return c
}

func toSummary(o order) orderSummary {
	s := orderSummary{
		OrderID:  o.ID,
		Status:   o.Status,
		Total:    money(o.TotalCents, o.Currency),
		PlacedAt: o.CreatedAt,
		Items:    []string{},
	}
	for _, l := range o.Lines {
		s.ItemCount += l.Quantity
		s.Items = append(s.Items, fmt.Sprintf("%d × %s", l.Quantity, l.Name))
	}
	return s
}

func money(cents int64, currency string) string {
	sym := map[string]string{"USD": "$", "EUR": "€", "GBP": "£", "INR": "₹"}[currency]
	if sym == "" {
		return fmt.Sprintf("%d.%02d %s", cents/100, cents%100, currency)
	}
	return fmt.Sprintf("%s%d.%02d", sym, cents/100, cents%100)
}

// ---- routes ---------------------------------------------------------------

func register(mux *http.ServeMux, d deps) {
	mux.HandleFunc("GET /api/home", func(w http.ResponseWriter, r *http.Request) {
		var out struct{ Items []product }
		if err := d.shopping.Do(r.Context(), http.MethodGet, "/v1/products", nil, &out); err != nil {
			d.fail(w, r, err)
			return
		}
		cards := make([]productCard, len(out.Items))
		for i, p := range out.Items {
			cards[i] = toCard(p)
		}
		platform.WriteJSON(w, http.StatusOK, map[string]any{"title": "Shop", "products": cards})
	})

	mux.HandleFunc("GET /api/products/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !platform.ValidID(id) {
			platform.WriteError(w, platform.ErrNotFound("product not found"))
			return
		}
		var p product
		if err := d.shopping.Do(r.Context(), http.MethodGet, "/v1/products/"+url.PathEscape(id), nil, &p); err != nil {
			d.fail(w, r, err)
			return
		}
		platform.WriteJSON(w, http.StatusOK, toCard(p))
	})

	mux.HandleFunc("POST /api/bag/checkout", d.authed(func(ctx context.Context, user string, w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if len(key) < 8 || !platform.ValidID(key) {
			platform.WriteError(w, platform.ErrBadRequest("Idempotency-Key header required: 8-64 characters of [A-Za-z0-9_-], e.g. a UUID"))
			return
		}
		var bag struct {
			Items []struct {
				ProductID string `json:"productId"`
				Quantity  int    `json:"quantity"`
			} `json:"items"`
		}
		if err := platform.DecodeJSON(r, &bag); err != nil {
			platform.WriteError(w, err)
			return
		}
		if len(bag.Items) == 0 || len(bag.Items) > maxCartLines {
			platform.WriteError(w, platform.ErrBadRequest(fmt.Sprintf("a bag holds 1 to %d items", maxCartLines)))
			return
		}
		for _, it := range bag.Items {
			if !platform.ValidID(it.ProductID) {
				platform.WriteError(w, platform.ErrBadRequest("invalid productId"))
				return
			}
			if it.Quantity < 1 || it.Quantity > maxLineQty {
				platform.WriteError(w, platform.ErrBadRequest(fmt.Sprintf("quantity must be 1 to %d", maxLineQty)))
				return
			}
		}
		if ok, wait := d.checkouts.allow(user); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			platform.WriteError(w, platform.ErrTooManyRequests("too many checkouts, please wait a moment"))
			return
		}

		req := map[string]any{"customerId": user, "idempotencyKey": key, "items": bag.Items}
		var res struct {
			Order    order `json:"order"`
			Replayed bool  `json:"replayed"`
		}
		if err := d.shopping.Do(ctx, http.MethodPost, "/v1/checkouts", req, &res); err != nil {
			d.fail(w, r, err)
			return
		}
		status := http.StatusCreated
		if res.Replayed { // same Idempotency-Key as an earlier checkout: same order, not a new one
			w.Header().Set("Idempotent-Replayed", "true")
			status = http.StatusOK
		}
		platform.WriteJSON(w, status, map[string]any{
			"message": "Thanks! Your order is " + res.Order.Status + ".",
			"order":   toSummary(res.Order),
		})
	}))

	mux.HandleFunc("GET /api/me/orders", d.authed(func(ctx context.Context, user string, w http.ResponseWriter, r *http.Request) {
		var out struct{ Items []order }
		if err := d.shopping.Do(ctx, http.MethodGet, "/v1/customers/"+url.PathEscape(user)+"/orders", nil, &out); err != nil {
			d.fail(w, r, err)
			return
		}
		list := make([]orderSummary, len(out.Items))
		for i, o := range out.Items {
			list[i] = toSummary(o)
		}
		platform.WriteJSON(w, http.StatusOK, map[string]any{"orders": list})
	}))

	mux.HandleFunc("GET /api/me/orders/{id}", d.authed(func(ctx context.Context, user string, w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !platform.ValidID(id) {
			platform.WriteError(w, platform.ErrNotFound("order not found"))
			return
		}
		var o order
		path := "/v1/customers/" + url.PathEscape(user) + "/orders/" + url.PathEscape(id)
		if err := d.shopping.Do(ctx, http.MethodGet, path, nil, &o); err != nil {
			d.fail(w, r, err)
			return
		}
		platform.WriteJSON(w, http.StatusOK, toSummary(o))
	}))
}

// authed requires a valid access token and passes its subject on as the
// customer id. Nothing else (no header, no body field) can set the identity.
func (d deps) authed(h func(ctx context.Context, user string, w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		if tok == "" {
			unauthorized(w, "sign in required")
			return
		}
		user, err := d.auth.Verify(r.Context(), tok)
		if err != nil || !platform.ValidID(user) {
			d.log.WarnContext(r.Context(), "token rejected", "err", err)
			unauthorized(w, "invalid or expired token")
			return
		}
		platform.Annotate(r.Context(), attribute.String("enduser.id", user))
		h(platform.WithUser(r.Context(), user), user, w, r)
	}
}

// clientCodes are business-layer error codes whose messages are written for
// customers and may be shown as they are. Everything else is replaced.
var clientCodes = map[string]bool{
	"bad_request": true, "not_found": true, "rate_limited": true,
	"product_unavailable": true, "mixed_currency": true, "insufficient_stock": true,
	"cart_limit": true, "idempotency_key_reused": true, "order_pending": true,
}

// clientError maps any downstream error to what a customer may see: known
// business outcomes pass through; infrastructure failures (Envoy 503s, RBAC
// denials, transport errors) become a generic "try again" with no internal detail.
func clientError(err error) *platform.Error {
	e := platform.AsError(err)
	switch {
	case e == nil:
		return platform.ErrInternal("internal error")
	case clientCodes[e.Code] && e.Status < 500:
		return &platform.Error{Status: e.Status, Code: e.Code, Message: e.Message}
	case e.Status == http.StatusBadGateway || e.Status == http.StatusServiceUnavailable ||
		e.Status == http.StatusGatewayTimeout || e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden:
		return platform.ErrUnavailable()
	default:
		return platform.ErrInternal("internal error")
	}
}

// fail logs the full error (with trace id) and writes the client-safe version.
func (d deps) fail(w http.ResponseWriter, r *http.Request, err error) {
	ce := clientError(err)
	if ce.Status >= 500 {
		d.log.ErrorContext(r.Context(), "request failed", "err", err)
	}
	platform.WriteError(w, ce)
}
