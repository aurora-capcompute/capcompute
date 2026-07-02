package otelexport_test

import (
	"context"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/aurora-capcompute/capcompute/otelexport"
	"github.com/aurora-capcompute/capcompute/sim"
)

func TestExportRendersRunAsTrace(t *testing.T) {
	world := sim.NewWorld()
	program := sim.Program{
		{Name: "clock.now"},
		{Name: "transfer.out", Args: `{"amount":100}`},
		{Name: "internet.read", Args: `{"url":"https://example.com"}`},
	}
	if err := sim.Run(world, "run-1", program); err != nil {
		t.Fatalf("run: %v", err)
	}

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	if err := otelexport.Export(context.Background(), world.Journal, provider.Tracer("test"), base); err != nil {
		t.Fatalf("export: %v", err)
	}
	spans := recorder.Ended()
	if len(spans) != 4 { // 3 syscalls + the run root
		t.Fatalf("spans = %d, want 4: %+v", len(spans), spanNames(spans))
	}

	root := spans[len(spans)-1] // ended last
	if root.Name() != "run run-1" {
		t.Fatalf("root span = %q", root.Name())
	}
	rootTrace := root.SpanContext().TraceID()

	var sawLabels bool
	for _, span := range spans[:len(spans)-1] {
		if span.SpanContext().TraceID() != rootTrace {
			t.Fatalf("span %q escaped the run trace", span.Name())
		}
		if span.Parent().SpanID() != root.SpanContext().SpanID() {
			t.Fatalf("span %q is not a child of the run", span.Name())
		}
		for _, attr := range span.Attributes() {
			if attr.Key == "aurora.labels" {
				sawLabels = true
			}
		}
	}
	if !sawLabels {
		t.Fatal("no span carried provenance labels")
	}
}

func TestExportMarksOpenIntent(t *testing.T) {
	world := sim.NewWorld()
	world.Journal.CrashAt = 3 // die at the second completion append
	program := sim.Program{
		{Name: "clock.now"},
		{Name: "transfer.out", Args: `{"amount":100}`},
	}
	if err := sim.Run(world, "run-1", program); err == nil {
		t.Fatal("expected the injected crash")
	}

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	if err := otelexport.Export(context.Background(), world.Journal, provider.Tracer("test"),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("export: %v", err)
	}

	var open int
	for _, span := range recorder.Ended() {
		for _, attr := range span.Attributes() {
			if attr.Key == "aurora.open_intent" && attr.Value.AsBool() {
				open++
				if span.Name() != "transfer.out" {
					t.Fatalf("open intent span = %q, want transfer.out", span.Name())
				}
			}
		}
	}
	if open != 1 {
		t.Fatalf("open intent spans = %d, want 1", open)
	}
}

func TestExportEmptyJournalIsNoop(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	if err := otelexport.Export(context.Background(), sim.NewJournal(), provider.Tracer("test"), time.Time{}); err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(recorder.Ended()) != 0 {
		t.Fatalf("spans = %d, want 0", len(recorder.Ended()))
	}
}

func spanNames(spans []sdktrace.ReadOnlySpan) []string {
	names := make([]string, len(spans))
	for i, span := range spans {
		names[i] = span.Name()
	}
	return names
}
