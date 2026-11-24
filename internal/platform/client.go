package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Headers copied from the inbound request to every outbound call. Trace context
// (traceparent/tracestate) is handled by OpenTelemetry instead: the client
// transport injects a traceparent naming the current client span as parent.
var propagatedHeaders = []string{
	"x-request-id", // Envoy's request id, also logged by every hop
	"x-user-id",    // caller identity established at the BFF
}

type headersKey struct{}

// propagate stores the inbound propagation headers in the request context.
// At the edge, x-user-id is never taken from the request.
func propagate(next http.Handler, edge bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := http.Header{}
		for _, k := range propagatedHeaders {
			if edge && k == "x-user-id" {
				continue
			}
			if v := r.Header.Get(k); v != "" {
				h.Set(k, v)
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), headersKey{}, h)))
	})
}

// Client calls another service inside the mesh. Timeouts and retries are owned
// by Istio (VirtualService); the client timeout here is only a safety net.
type Client struct {
	base string
	http *http.Client
}

func NewClient(baseURL string) *Client {
	return &Client{base: baseURL, http: &http.Client{Timeout: 15 * time.Second, Transport: tracingTransport()}}
}

// Do sends in (if non-nil) as JSON and decodes the response into out (if non-nil).
// Error responses are decoded into *Error so callers can branch on status/code.
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	if h, ok := ctx.Value(headersKey{}).(http.Header); ok {
		for k, v := range h {
			req.Header[k] = v
		}
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// The cause names internal hosts; keep it out of the message.
		return &Error{Status: http.StatusBadGateway, Code: "upstream_error", Message: "upstream unavailable",
			Cause: fmt.Errorf("%s %s: %w", method, c.base+path, err)}
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var env struct {
			Error *Error `json:"error"`
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if json.Unmarshal(raw, &env) == nil && env.Error != nil {
			env.Error.Status = resp.StatusCode
			return env.Error
		}
		// Not our envelope, e.g. an Envoy 503 or an RBAC 403 from the sidecar.
		return &Error{Status: resp.StatusCode, Code: "upstream_error", Message: "upstream error",
			Cause: fmt.Errorf("%s %s: %d %s", method, c.base+path, resp.StatusCode, bytes.TrimSpace(raw))}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// WithUser returns a context whose outbound calls carry x-user-id.
func WithUser(ctx context.Context, userID string) context.Context {
	h, _ := ctx.Value(headersKey{}).(http.Header)
	h = h.Clone()
	if h == nil {
		h = http.Header{}
	}
	h.Set("x-user-id", userID)
	return context.WithValue(ctx, headersKey{}, h)
}
