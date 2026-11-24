package platform

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteErrorHidesUnknownErrors(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, errors.New("open /etc/secret: permission denied"))
	if rec.Code != 500 || strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestClientErrorsKeepDetailsOutOfMessage(t *testing.T) {
	// Transport failure: the dial error names an internal host.
	err := NewClient("http://127.0.0.1:1").Do(context.Background(), http.MethodGet, "/v1/x", nil, nil)
	e := AsError(err)
	if e == nil || e.Code != "upstream_error" || strings.Contains(e.Message, "127.0.0.1") || e.Cause == nil {
		t.Fatalf("transport: %#v", e)
	}
	// Non-envelope error body, e.g. Envoy's "no healthy upstream".
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no healthy upstream", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	e = AsError(NewClient(srv.URL).Do(context.Background(), http.MethodGet, "/", nil, nil))
	if e == nil || e.Status != 503 || strings.Contains(e.Message, "healthy") || !strings.Contains(e.Error(), "healthy") {
		t.Fatalf("envoy body: %#v", e)
	}
}

func TestValidID(t *testing.T) {
	for _, ok := range []string{"p-100", "ord-0f3a", "c7d1e0a2-4b1f-4e8a-9a51-2f0e6c1d9b77", "a"} {
		if !ValidID(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "../x", "a/b", "a%2Fb", "-a", "a b", strings.Repeat("x", 65), "ä"} {
		if ValidID(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestEdgeIgnoresIdentityHeader(t *testing.T) {
	var got string
	h := propagate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr, _ := r.Context().Value(headersKey{}).(http.Header)
		got = hdr.Get("x-user-id")
	}), true)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("x-user-id", "alice")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got != "" {
		t.Fatalf("edge propagated client identity %q", got)
	}
}
