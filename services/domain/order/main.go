// order is a domain service: it owns the Order aggregate and its lifecycle.
package main

import (
	"net/http"

	"meshshop/internal/platform"
)

func main() {
	svc := platform.NewService("order", "domain")
	register(svc.Mux, NewStore())
	svc.Run()
}

func register(mux *http.ServeMux, store *Store) {
	mux.HandleFunc("POST /v1/orders", func(w http.ResponseWriter, r *http.Request) {
		var in NewOrder
		if err := platform.DecodeJSON(r, &in); err != nil {
			platform.WriteError(w, err)
			return
		}
		o, err := store.Create(in)
		if err != nil {
			platform.WriteError(w, err)
			return
		}
		platform.WriteJSON(w, http.StatusCreated, o)
	})

	// GET /v1/orders?customerId=c-1[&idempotencyKey=k]
	mux.HandleFunc("GET /v1/orders", func(w http.ResponseWriter, r *http.Request) {
		cid := r.URL.Query().Get("customerId")
		if !platform.ValidID(cid) {
			platform.WriteError(w, platform.ErrBadRequest("customerId is missing or malformed"))
			return
		}
		if key := r.URL.Query().Get("idempotencyKey"); key != "" {
			items := []Order{}
			if o, ok := store.ByKey(cid, key); ok {
				items = append(items, o)
			}
			platform.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
			return
		}
		platform.WriteJSON(w, http.StatusOK, map[string]any{"items": store.ByCustomer(cid)})
	})

	mux.HandleFunc("GET /v1/orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		o, err := store.Get(r.PathValue("id"))
		if err != nil {
			platform.WriteError(w, err)
			return
		}
		platform.WriteJSON(w, http.StatusOK, o)
	})

	transition := func(to Status) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			o, err := store.Transition(r.PathValue("id"), to)
			if err != nil {
				platform.WriteError(w, err)
				return
			}
			platform.WriteJSON(w, http.StatusOK, o)
		}
	}
	mux.HandleFunc("POST /v1/orders/{id}/confirm", transition(Confirmed))
	mux.HandleFunc("POST /v1/orders/{id}/cancel", transition(Cancelled))
}
