// inventory is a domain service: it owns stock levels and reservations.
package main

import (
	"net/http"
	"strings"
	"time"

	"meshshop/internal/platform"
)

func main() {
	svc := platform.NewService("inventory", "domain")
	store := NewStore(seedStock)
	if ttl, err := time.ParseDuration(platform.Env("RESERVATION_TTL", "")); err == nil && ttl > 0 {
		store.ttl = ttl
	}
	go func() { // reaper: return stock held by abandoned checkouts
		for range time.Tick(15 * time.Second) {
			if n := store.ExpireHeld(); n > 0 {
				svc.Log.Warn("released expired reservations", "count", n)
			}
		}
	}()
	register(svc.Mux, store)
	svc.Run()
}

func register(mux *http.ServeMux, store *Store) {
	// GET /v1/stock?productIds=a,b
	mux.HandleFunc("GET /v1/stock", func(w http.ResponseWriter, r *http.Request) {
		raw := r.URL.Query().Get("productIds")
		if raw == "" {
			platform.WriteError(w, platform.ErrBadRequest("productIds is required"))
			return
		}
		ids := strings.Split(raw, ",")
		if len(ids) > 100 {
			platform.WriteError(w, platform.ErrBadRequest("at most 100 productIds"))
			return
		}
		for i := range ids {
			ids[i] = strings.TrimSpace(ids[i])
		}
		platform.WriteJSON(w, http.StatusOK, map[string]any{"items": store.Levels(ids)})
	})

	// POST /v1/reservations {"ref": "...", "lines": [{"productId": "...", "quantity": 1}]}
	mux.HandleFunc("POST /v1/reservations", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Ref   string `json:"ref"`
			Lines []Line `json:"lines"`
		}
		if err := platform.DecodeJSON(r, &req); err != nil {
			platform.WriteError(w, err)
			return
		}
		res, err := store.Reserve(req.Ref, req.Lines)
		if err != nil {
			platform.WriteError(w, err)
			return
		}
		platform.WriteJSON(w, http.StatusCreated, res)
	})

	mux.HandleFunc("POST /v1/reservations/{id}/commit", func(w http.ResponseWriter, r *http.Request) {
		res, err := store.Commit(r.PathValue("id"))
		if err != nil {
			platform.WriteError(w, err)
			return
		}
		platform.WriteJSON(w, http.StatusOK, res)
	})

	mux.HandleFunc("POST /v1/reservations/{id}/release", func(w http.ResponseWriter, r *http.Request) {
		res, err := store.Release(r.PathValue("id"))
		if err != nil {
			platform.WriteError(w, err)
			return
		}
		platform.WriteJSON(w, http.StatusOK, res)
	})
}
