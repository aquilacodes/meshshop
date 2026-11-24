package platform

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// initTracing installs the global OpenTelemetry tracer provider and the W3C
// trace-context propagator.
//
// Spans are exported over OTLP/HTTP only when OTEL_EXPORTER_OTLP_ENDPOINT (or
// the traces-specific variant) is set, as it is in Kubernetes. Otherwise
// (unit tests, `go run`) the propagator is still installed but no spans leave
// the process. All standard OTEL_* environment variables are honoured.
func initTracing(name, layer string) (shutdown func(context.Context) error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))

	noop := func(context.Context) error { return nil }
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "" {
		return noop
	}

	ctx := context.Background()
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		slog.Error("tracing disabled: exporter", "err", err)
		return noop
	}
	res, err := resource.New(ctx,
		resource.WithFromEnv(), // OTEL_RESOURCE_ATTRIBUTES, OTEL_SERVICE_NAME
		resource.WithTelemetrySDK(),
		resource.WithAttributes(
			attribute.String("service.name", name),
			attribute.String("service.namespace", "meshshop"),
			attribute.String("meshshop.layer", layer),
		),
	)
	if err != nil {
		slog.Warn("tracing resource incomplete", "err", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		// Follow the caller's decision (the ingress gateway samples at the edge);
		// sample everything for requests that start here.
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown
}

// traceServer wraps a handler in an OpenTelemetry server span, named after the
// ServeMux route pattern (e.g. "GET /v1/products/{id}") once routing is done.
//
// Every response carries a W3C `traceresponse` header naming the trace, so a
// client (or Postman) can jump to it in Kibana. Clients cannot choose the trace:
// the gateway drops inbound trace context and starts a fresh one.
func traceServer(mux http.Handler, inner func(http.Handler) http.Handler) http.Handler {
	named := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sc := trace.SpanContextFromContext(r.Context()); sc.IsValid() {
			w.Header().Set("traceresponse", "00-"+sc.TraceID().String()+"-"+sc.SpanID().String()+"-"+sc.TraceFlags().String())
		}
		mux.ServeHTTP(w, r) // ServeMux sets r.Pattern on this request
		if r.Pattern != "" {
			span := trace.SpanFromContext(r.Context())
			span.SetName(r.Pattern)
			if _, route, ok := strings.Cut(r.Pattern, " "); ok {
				span.SetAttributes(attribute.String("http.route", route))
			}
		}
	})
	return otelhttp.NewHandler(inner(named), "http.server",
		otelhttp.WithFilter(func(r *http.Request) bool {
			return r.URL.Path != "/healthz" && r.URL.Path != "/readyz"
		}),
	)
}

// tracingTransport creates a client span per outbound call and injects traceparent.
func tracingTransport() http.RoundTripper {
	return otelhttp.NewTransport(http.DefaultTransport,
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + r.URL.Hostname()
		}),
	)
}

// Step runs fn inside a child span. Use it for meaningful units of work inside
// a request, e.g. each step of a saga. Server-side failures (5xx or unknown
// errors) mark the span as failed; 4xx business rejections are recorded as an
// attribute so they don't count as errors.
func Step(ctx context.Context, name string, fn func(ctx context.Context) error, attrs ...attribute.KeyValue) error {
	ctx, span := otel.Tracer("meshshop").Start(ctx, name, trace.WithAttributes(attrs...))
	defer span.End()
	err := fn(ctx)
	if err != nil {
		if e := AsError(err); e != nil && e.Status < 500 {
			span.SetAttributes(attribute.String("meshshop.rejected", e.Code))
		} else {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
	}
	return err
}

// Annotate adds attributes to the current span (usually the server span).
func Annotate(ctx context.Context, attrs ...attribute.KeyValue) {
	trace.SpanFromContext(ctx).SetAttributes(attrs...)
}

// traceLogHandler adds trace_id and span_id to every log record written with a
// context that carries a span (slog.InfoContext etc.). Logstash maps them to the
// ECS fields trace.id / span.id, which is what links logs to traces in Kibana.
type traceLogHandler struct{ slog.Handler }

func (h traceLogHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return h.Handler.Handle(ctx, r)
}

func (h traceLogHandler) WithAttrs(a []slog.Attr) slog.Handler {
	return traceLogHandler{h.Handler.WithAttrs(a)}
}

func (h traceLogHandler) WithGroup(name string) slog.Handler {
	return traceLogHandler{h.Handler.WithGroup(name)}
}
