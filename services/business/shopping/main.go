// shopping is the business layer: it implements shopping use cases by
// orchestrating the catalog, inventory, and order domain services.
// It is called only by BFFs and calls only domain services.
package main

import (
	"net/http"

	"meshshop/internal/platform"
)

func main() {
	svc := platform.NewService("shopping", "business")
	app := &App{
		Catalog:   catalogClient{platform.NewClient(platform.Env("CATALOG_URL", "http://catalog:8080"))},
		Inventory: inventoryClient{platform.NewClient(platform.Env("INVENTORY_URL", "http://inventory:8080"))},
		Orders:    orderClient{platform.NewClient(platform.Env("ORDER_URL", "http://order:8080"))},
		Log:       svc.Log,
	}
	register(svc.Mux, app)
	svc.Run()
}

func register(mux *http.ServeMux, app *App) {
	mux.HandleFunc("GET /v1/products", func(w http.ResponseWriter, r *http.Request) {
		views, err := app.Browse(r.Context())
		respond(w, http.StatusOK, map[string]any{"items": views}, err)
	})

	mux.HandleFunc("GET /v1/products/{id}", func(w http.ResponseWriter, r *http.Request) {
		v, err := app.Product(r.Context(), r.PathValue("id"))
		respond(w, http.StatusOK, v, err)
	})

	mux.HandleFunc("POST /v1/checkouts", func(w http.ResponseWriter, r *http.Request) {
		var req CheckoutRequest
		if err := platform.DecodeJSON(r, &req); err != nil {
			platform.WriteError(w, err)
			return
		}
		res, err := app.Checkout(r.Context(), req)
		status := http.StatusCreated
		if res.Replayed {
			status = http.StatusOK
		}
		respond(w, status, res, err)
	})

	mux.HandleFunc("GET /v1/customers/{cid}/orders", func(w http.ResponseWriter, r *http.Request) {
		orders, err := app.CustomerOrders(r.Context(), r.PathValue("cid"))
		respond(w, http.StatusOK, map[string]any{"items": orders}, err)
	})

	mux.HandleFunc("GET /v1/customers/{cid}/orders/{oid}", func(w http.ResponseWriter, r *http.Request) {
		o, err := app.CustomerOrder(r.Context(), r.PathValue("cid"), r.PathValue("oid"))
		respond(w, http.StatusOK, o, err)
	})
}

func respond(w http.ResponseWriter, status int, v any, err error) {
	if err != nil {
		platform.WriteError(w, err)
		return
	}
	platform.WriteJSON(w, status, v)
}
