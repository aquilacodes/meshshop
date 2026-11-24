package platform

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// Two services in-process: "front" calls "back". Checks span naming, parent/child
// links across the HTTP hop, and trace ids in logs.
func TestTracePropagationAndLogCorrelation(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	var logs bytes.Buffer
	newSvc := func(name string) *Service {
		s := NewService(name, "test")
		s.Log = slog.New(traceLogHandler{slog.NewJSONHandler(&logs, nil)})
		return s
	}

	back := newSvc("back")
	var gotTraceparent string
	back.Mux.HandleFunc("GET /items/{id}", func(w http.ResponseWriter, r *http.Request) {
		gotTraceparent = r.Header.Get("traceparent")
		WriteJSON(w, http.StatusOK, map[string]string{"id": r.PathValue("id")})
	})
	backSrv := httptest.NewServer(back.Handler())
	defer backSrv.Close()

	front := newSvc("front")
	client := NewClient(backSrv.URL)
	front.Mux.HandleFunc("GET /view/{id}", func(w http.ResponseWriter, r *http.Request) {
		var out map[string]string
		if err := client.Do(r.Context(), http.MethodGet, "/items/"+r.PathValue("id"), nil, &out); err != nil {
			WriteError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, out)
	})
	frontSrv := httptest.NewServer(front.Handler())
	defer frontSrv.Close()

	resp, err := http.Get(frontSrv.URL + "/view/42")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	spans := rec.Ended()
	byName := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range spans {
		byName[s.Name()] = s
	}
	frontSpan, clientSpan, backSpan := byName["GET /view/{id}"], byName["GET 127.0.0.1"], byName["GET /items/{id}"]
	if frontSpan == nil || clientSpan == nil || backSpan == nil {
		names := []string{}
		for n := range byName {
			names = append(names, n)
		}
		t.Fatalf("missing spans, got %v", names)
	}
	traceID := frontSpan.SpanContext().TraceID()
	if clientSpan.Parent().SpanID() != frontSpan.SpanContext().SpanID() ||
		backSpan.Parent().SpanID() != clientSpan.SpanContext().SpanID() ||
		backSpan.SpanContext().TraceID() != traceID {
		t.Fatal("spans are not linked front -> client -> back in one trace")
	}
	if !strings.Contains(gotTraceparent, traceID.String()) {
		t.Fatalf("traceparent %q does not carry trace id %s", gotTraceparent, traceID)
	}

	// Both request log lines carry the shared trace id.
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var m map[string]any
		_ = json.Unmarshal([]byte(line), &m)
		if m["msg"] == "request" {
			n++
			if m["trace_id"] != traceID.String() {
				t.Errorf("log line without trace id: %s", line)
			}
		}
	}
	if n != 2 {
		t.Fatalf("want 2 request log lines, got %d: %s", n, logs.String())
	}
}
