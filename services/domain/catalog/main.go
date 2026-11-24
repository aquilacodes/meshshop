// catalog is a domain service: it owns the Product model and its data.
// Domain services call no other services.
package main

import (
	"net/http"
	"strings"

	"meshshop/internal/platform"
)

func main() {
	svc := platform.NewService("catalog", "domain")
	register(svc.Mux, NewStore(seedProducts))
	svc.Run()
}

func register(mux *http.ServeMux, store *Store) {
	// GET /v1/products            all products
	// GET /v1/products?ids=a,b    a subset (unknown ids are skipped)
	mux.HandleFunc("GET /v1/products", func(w http.ResponseWriter, r *http.Request) {
		ids := r.URL.Query().Get("ids")
		if ids == "" {
			platform.WriteJSON(w, http.StatusOK, map[string]any{"items": store.List()})
			return
		}
		list := strings.Split(ids, ",")
		if len(list) > 100 {
			platform.WriteError(w, platform.ErrBadRequest("at most 100 ids"))
			return
		}
		items := []Product{}
		for _, id := range list {
			if p, err := store.Get(strings.TrimSpace(id)); err == nil {
				items = append(items, p)
			}
		}
		platform.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
	})

	mux.HandleFunc("GET /v1/products/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, err := store.Get(r.PathValue("id"))
		if err != nil {
			platform.WriteError(w, err)
			return
		}
		platform.WriteJSON(w, http.StatusOK, p)
	})
}
