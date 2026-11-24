package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"meshshop/internal/platform"
)

const (
	testIssuer   = "https://meshshop.localhost:8443/auth/realms/meshshop"
	testAudience = "meshshop-api"
)

var signingKey, otherKey = mustKey(), mustKey()

func mustKey() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
}

func testVerifier() tokenVerifier {
	keys := &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&signingKey.PublicKey}}
	return &oidcVerifier{oidc.NewVerifier(testIssuer, keys, &oidc.Config{ClientID: testAudience, SupportedSigningAlgs: []string{oidc.RS256}})}
}

// token signs a Keycloak-shaped access token; edit changes the claims first.
func token(t *testing.T, key *rsa.PrivateKey, edit func(map[string]any)) string {
	t.Helper()
	claims := map[string]any{
		"iss": testIssuer, "aud": []string{testAudience, "account"}, "sub": "c7d1e0a2-alice",
		"typ": "Bearer", "azp": "meshshop-cli",
		"iat": time.Now().Unix(), "exp": time.Now().Add(5 * time.Minute).Unix(),
	}
	if edit != nil {
		edit(claims)
	}
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jwt.Signed(sig).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// bffWith starts the BFF in front of a fake business layer.
func bffWith(t *testing.T, upstream http.HandlerFunc) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	svc := platform.NewService("web-bff", "bff").Edge()
	register(svc.Mux, deps{
		shopping:  platform.NewClient(up.URL),
		auth:      testVerifier(),
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		checkouts: newLimiter(checkoutsPerMin, time.Minute),
	})
	s := httptest.NewServer(svc.Handler())
	t.Cleanup(s.Close)
	return s
}

func call(t *testing.T, s *httptest.Server, method, path, body string, hdr map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, s.URL+path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func errCode(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func confirmedOrder(w http.ResponseWriter) {
	platform.WriteJSON(w, http.StatusCreated, map[string]any{"order": order{ID: "ord-9", Status: "confirmed",
		TotalCents: 2500, Currency: "USD", Lines: []orderLine{{Name: "Mug", Quantity: 2}}}})
}

const bag = `{"items":[{"productId":"p-100","quantity":2}]}`

func TestCardBadges(t *testing.T) {
	cases := []struct {
		p     product
		badge string
		price string
	}{
		{product{PriceCents: 1299, Currency: "USD", Available: 20, InStock: true}, "", "$12.99"},
		{product{PriceCents: 500, Currency: "EUR", Available: 3, InStock: true}, "Only 3 left", "€5.00"},
		{product{PriceCents: 7, Currency: "JPY", Available: 0, InStock: false}, "Sold out", "0.07 JPY"},
	}
	for _, c := range cases {
		got := toCard(c.p)
		if got.Badge != c.badge || got.Price != c.price || got.CanAddToBag != c.p.InStock {
			t.Errorf("toCard(%+v) = %+v", c.p, got)
		}
	}
}

// The customer id comes from the token's subject; a client-sent x-user-id is ignored.
func TestCheckoutUsesTokenSubject(t *testing.T) {
	var gotUser string
	var gotBody map[string]any
	s := bffWith(t, func(w http.ResponseWriter, r *http.Request) {
		gotUser = r.Header.Get("x-user-id")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		confirmedOrder(w)
	})
	resp, out := call(t, s, http.MethodPost, "/api/bag/checkout", bag, map[string]string{
		"Authorization": "Bearer " + token(t, signingKey, nil), "x-user-id": "mallory", "Idempotency-Key": "key-00000001",
	})
	if resp.StatusCode != http.StatusCreated || gotUser != "c7d1e0a2-alice" || gotBody["customerId"] != "c7d1e0a2-alice" ||
		gotBody["idempotencyKey"] != "key-00000001" {
		t.Fatalf("status=%d user=%q body=%v", resp.StatusCode, gotUser, gotBody)
	}
	o := out["order"].(map[string]any)
	if o["total"] != "$25.00" || o["itemCount"] != float64(2) {
		t.Fatalf("summary = %+v", o)
	}
}

func TestTokenRequired(t *testing.T) {
	s := bffWith(t, func(w http.ResponseWriter, r *http.Request) { t.Error("upstream must not be called") })
	cases := map[string]string{
		"no token, spoofed header": "",
		"wrong signing key":        token(t, otherKey, nil),
		"expired":                  token(t, signingKey, func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() }),
		"wrong issuer":             token(t, signingKey, func(c map[string]any) { c["iss"] = "https://evil.example/realms/meshshop" }),
		"wrong audience":           token(t, signingKey, func(c map[string]any) { c["aud"] = "account" }),
		"id token, not access":     token(t, signingKey, func(c map[string]any) { c["typ"] = "ID" }),
		"hostile subject":          token(t, signingKey, func(c map[string]any) { c["sub"] = "../../v1/products" }),
		"alg none":                 "eyJhbGciOiJub25lIn0.eyJzdWIiOiJhbGljZSJ9.",
	}
	for name, tok := range cases {
		hdr := map[string]string{"x-user-id": "alice", "Idempotency-Key": "key-00000001"}
		if tok != "" {
			hdr["Authorization"] = "Bearer " + tok
		}
		for _, path := range []string{"/api/me/orders", "/api/bag/checkout"} {
			method := http.MethodGet
			if strings.HasSuffix(path, "checkout") {
				method = http.MethodPost
			}
			resp, out := call(t, s, method, path, bag, hdr)
			if resp.StatusCode != http.StatusUnauthorized || errCode(out) != "unauthorized" ||
				resp.Header.Get("WWW-Authenticate") == "" {
				t.Errorf("%s %s: status=%d out=%v", name, path, resp.StatusCode, out)
			}
		}
	}
}

func TestCheckoutInputLimits(t *testing.T) {
	s := bffWith(t, func(w http.ResponseWriter, r *http.Request) { t.Error("upstream must not be called") })
	auth := "Bearer " + token(t, signingKey, nil)
	many := `{"items":[` + strings.Repeat(`{"productId":"p-100","quantity":1},`, maxCartLines) + `{"productId":"p-100","quantity":1}]}`
	cases := map[string]struct{ key, body string }{
		"missing idempotency key": {"", bag},
		"short idempotency key":   {"abc", bag},
		"quantity too high":       {"key-00000001", `{"items":[{"productId":"p-100","quantity":11}]}`},
		"overflowing quantity":    {"key-00000001", `{"items":[{"productId":"p-100","quantity":4611686018427387904}]}`},
		"zero quantity":           {"key-00000001", `{"items":[{"productId":"p-100","quantity":0}]}`},
		"too many lines":          {"key-00000001", many},
		"empty bag":               {"key-00000001", `{"items":[]}`},
		"bad product id":          {"key-00000001", `{"items":[{"productId":"../x","quantity":1}]}`},
		"client-set price":        {"key-00000001", `{"items":[{"productId":"p-100","quantity":1,"price":1}]}`},
	}
	for name, c := range cases {
		resp, out := call(t, s, http.MethodPost, "/api/bag/checkout", c.body, map[string]string{"Authorization": auth, "Idempotency-Key": c.key})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status=%d out=%v", name, resp.StatusCode, out)
		}
	}
}

func TestCheckoutRateLimitedPerCustomer(t *testing.T) {
	s := bffWith(t, func(w http.ResponseWriter, r *http.Request) { confirmedOrder(w) })
	alice, bob := "Bearer "+token(t, signingKey, nil), "Bearer "+token(t, signingKey, func(c map[string]any) { c["sub"] = "bob" })
	for i := range checkoutsPerMin + 1 {
		resp, _ := call(t, s, http.MethodPost, "/api/bag/checkout", bag, map[string]string{"Authorization": alice, "Idempotency-Key": fmt.Sprintf("key-%08d", i)})
		want := http.StatusCreated
		if i == checkoutsPerMin {
			want = http.StatusTooManyRequests
			if resp.Header.Get("Retry-After") == "" {
				t.Error("429 without Retry-After")
			}
		}
		if resp.StatusCode != want {
			t.Fatalf("checkout %d: status %d, want %d", i+1, resp.StatusCode, want)
		}
	}
	if resp, _ := call(t, s, http.MethodPost, "/api/bag/checkout", bag, map[string]string{"Authorization": bob, "Idempotency-Key": "key-00000001"}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("another customer is not limited, got %d", resp.StatusCode)
	}
}

func TestIdempotentReplay(t *testing.T) {
	s := bffWith(t, func(w http.ResponseWriter, r *http.Request) {
		platform.WriteJSON(w, http.StatusOK, map[string]any{"replayed": true, "order": order{ID: "ord-9", Status: "confirmed"}})
	})
	resp, _ := call(t, s, http.MethodPost, "/api/bag/checkout", bag, map[string]string{"Authorization": "Bearer " + token(t, signingKey, nil), "Idempotency-Key": "key-00000001"})
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("status=%d replayed=%q", resp.StatusCode, resp.Header.Get("Idempotent-Replayed"))
	}
}

// Downstream errors reach the client only as known, customer-safe outcomes.
func TestErrorsAreSanitised(t *testing.T) {
	cases := []struct {
		name       string
		upstream   http.HandlerFunc
		wantStatus int
		wantCode   string
	}{
		{"envoy 503", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "no healthy upstream", http.StatusServiceUnavailable)
		}, 503, "unavailable"},
		{"sidecar RBAC", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "RBAC: access denied", http.StatusForbidden)
		}, 503, "unavailable"},
		{"internal error envelope", func(w http.ResponseWriter, r *http.Request) {
			platform.WriteError(w, platform.ErrInternal("dial tcp 10.0.0.7:8080: connection refused"))
		}, 500, "internal"},
		{"unknown conflict code", func(w http.ResponseWriter, r *http.Request) {
			platform.WriteError(w, platform.ErrConflict("invalid_transition", "reservation res-1 is committed"))
		}, 500, "internal"},
		{"business rejection passes", func(w http.ResponseWriter, r *http.Request) {
			platform.WriteError(w, platform.ErrConflict("insufficient_stock", "not enough stock for p-103"))
		}, 409, "insufficient_stock"},
	}
	for _, c := range cases {
		s := bffWith(t, c.upstream)
		req, _ := http.NewRequest(http.MethodGet, s.URL+"/api/home", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		if resp.StatusCode != c.wantStatus || errCode(out) != c.wantCode {
			t.Errorf("%s: status=%d body=%s", c.name, resp.StatusCode, raw)
		}
		for _, leak := range []string{"healthy", "RBAC", "10.0.0.7", "res-1", "127.0.0.1"} {
			if strings.Contains(string(raw), leak) {
				t.Errorf("%s: response leaks %q: %s", c.name, leak, raw)
			}
		}
	}
}

func TestSecurityHeadersAndNoIdentityFromHeader(t *testing.T) {
	var gotUser string
	s := bffWith(t, func(w http.ResponseWriter, r *http.Request) {
		gotUser = r.Header.Get("x-user-id")
		platform.WriteJSON(w, http.StatusOK, map[string]any{"items": []product{}})
	})
	resp, _ := call(t, s, http.MethodGet, "/api/home", "", map[string]string{"x-user-id": "alice"})
	if gotUser != "" {
		t.Errorf("client x-user-id reached the business layer: %q", gotUser)
	}
	for h, want := range map[string]string{
		"X-Content-Type-Options": "nosniff", "Cache-Control": "no-store",
		"Cross-Origin-Resource-Policy": "same-origin", "X-Frame-Options": "DENY",
	} {
		if got := resp.Header.Get(h); got != want {
			t.Errorf("%s = %q, want %q", h, got, want)
		}
	}
}

func TestPathIDsValidated(t *testing.T) {
	s := bffWith(t, func(w http.ResponseWriter, r *http.Request) { t.Errorf("upstream called with %s", r.URL.Path) })
	for _, p := range []string{"/api/products/a%2Fb", "/api/products/" + strings.Repeat("x", 65), "/api/products/..."} {
		if resp, _ := call(t, s, http.MethodGet, p, "", nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
}

func TestLimiterRefills(t *testing.T) {
	now := time.Unix(0, 0)
	l := newLimiter(2, time.Minute)
	l.now = func() time.Time { return now }
	for range 2 {
		if ok, _ := l.allow("a"); !ok {
			t.Fatal("burst should be allowed")
		}
	}
	if ok, wait := l.allow("a"); ok || wait <= 0 || wait > 30*time.Second {
		t.Fatalf("third call: ok=%v wait=%v", ok, wait)
	}
	now = now.Add(30 * time.Second)
	if ok, _ := l.allow("a"); !ok {
		t.Fatal("token should have refilled")
	}
}
