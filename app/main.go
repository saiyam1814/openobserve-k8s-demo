// checkout: a tiny HTTP service that emits OpenTelemetry traces and JSON logs
// carrying the trace id, so a backend can correlate the two. A /chaos endpoint
// flips a failure rate at runtime so an SLO burn-rate alert can be demonstrated.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

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

var (
	tracer   = otel.Tracer("checkout")
	failRate atomic.Int64 // percent, 0..100
	logger   *slog.Logger
)

// traceHandler adds trace_id and span_id to every log record so logs and traces
// can be joined on the backend without any extra configuration.
type traceHandler struct{ slog.Handler }

func (h traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs and WithGroup must re-wrap, otherwise Logger.With() returns the
// inner handler and this wrapper silently disappears.
func (h traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceHandler{h.Handler.WithAttrs(attrs)}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{h.Handler.WithGroup(name)}
}

func initTracer(ctx context.Context) (func(context.Context) error, error) {
	// Endpoint and headers come from OTEL_EXPORTER_OTLP_* environment variables.
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	// Plain attribute keys avoid schema-URL conflicts between the SDK's default
	// resource and a pinned semconv package.
	res, err := resource.New(ctx,
		resource.WithFromEnv(), resource.WithTelemetrySDK(), resource.WithHost(),
		resource.WithAttributes(
			attribute.String("service.name", envOr("OTEL_SERVICE_NAME", "checkout")),
			attribute.String("service.version", envOr("APP_VERSION", "1.0.0")),
			attribute.String("deployment.environment.name", envOr("APP_ENV", "demo")),
		))
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	return tp.Shutdown, nil
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func reserveInventory(ctx context.Context, sku string, qty int) error {
	ctx, span := tracer.Start(ctx, "inventory.reserve", trace.WithAttributes(attribute.String("sku", sku), attribute.Int("qty", qty)))
	defer span.End()
	time.Sleep(time.Duration(5+rand.IntN(20)) * time.Millisecond)
	logger.InfoContext(ctx, "inventory reserved", "sku", sku, "qty", qty)
	return nil
}

func chargeCard(ctx context.Context, orderID string, amount float64) error {
	ctx, span := tracer.Start(ctx, "payment.charge", trace.WithAttributes(attribute.String("order_id", orderID), attribute.Float64("amount", amount)))
	defer span.End()
	time.Sleep(time.Duration(20+rand.IntN(60)) * time.Millisecond)
	if int64(rand.IntN(100)) < failRate.Load() {
		err := errors.New("payment gateway timeout")
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		logger.ErrorContext(ctx, "payment failed", "order_id", orderID, "amount", amount, "gateway", "stripe-sandbox", "error", err.Error())
		return err
	}
	logger.InfoContext(ctx, "payment captured", "order_id", orderID, "amount", amount, "gateway", "stripe-sandbox")
	return nil
}

func checkout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orderID := fmt.Sprintf("ord-%06d", rand.IntN(1_000_000))
	amount := float64(5+rand.IntN(200)) + 0.99
	sku := []string{"kube-tshirt", "otel-sticker", "gpu-mug", "rust-hoodie"}[rand.IntN(4)]
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(attribute.String("order_id", orderID), attribute.Float64("order.amount", amount))

	logger.InfoContext(ctx, "checkout started", "order_id", orderID, "sku", sku, "amount", amount)
	if err := reserveInventory(ctx, sku, 1); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := chargeCard(ctx, orderID, amount); err != nil {
		span.SetStatus(codes.Error, "checkout failed")
		http.Error(w, `{"error":"payment failed","order_id":"`+orderID+`"}`, http.StatusBadGateway)
		return
	}
	logger.InfoContext(ctx, "checkout complete", "order_id", orderID, "amount", amount)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"order_id":%q,"amount":%.2f,"status":"paid"}`, orderID, amount)
}

func chaos(w http.ResponseWriter, r *http.Request) {
	if v := r.URL.Query().Get("rate"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 100 {
			http.Error(w, "rate must be 0..100", http.StatusBadRequest)
			return
		}
		failRate.Store(int64(n))
		logger.WarnContext(r.Context(), "failure rate changed", "fail_rate_percent", n)
	}
	fmt.Fprintf(w, `{"fail_rate_percent":%d}`, failRate.Load())
}

func main() {
	logger = slog.New(traceHandler{slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})}).
		With("service", envOr("OTEL_SERVICE_NAME", "checkout"), "version", envOr("APP_VERSION", "1.0.0"))
	slog.SetDefault(logger)
	if v, err := strconv.Atoi(envOr("FAIL_RATE_PERCENT", "2")); err == nil {
		failRate.Store(int64(v))
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	shutdown, err := initTracer(ctx)
	if err != nil {
		logger.Error("tracer init failed", "error", err.Error())
		os.Exit(1)
	}
	defer shutdown(context.Background())

	mux := http.NewServeMux()
	mux.Handle("/checkout", otelhttp.NewHandler(http.HandlerFunc(checkout), "POST /checkout"))
	mux.HandleFunc("/chaos", chaos)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	srv := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		logger.Info("checkout listening", "addr", srv.Addr, "fail_rate_percent", failRate.Load())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server error", "error", err.Error())
			os.Exit(1)
		}
	}()
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
