// Package otelexport renders a journal as OpenTelemetry spans: the process is the
// trace root, each intent/completion pair is a span, and the record envelope
// maps to attributes — the scope hierarchy (tenant → session → process → revision)
// aligns 1:1 with OTel trace/span/parent, so the exporter is a column mapping,
// not a translation layer.
//
// The journal records order, not duration (journaled timestamps are store
// columns, owned by the Journal implementation), so span times are synthetic:
// base + position×step. Yields are never journaled, so a yielded syscall
// appears once, as the pair its eventual completion produced.
package otelexport

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/aurora-capcompute/capcompute/sys"
	"github.com/aurora-capcompute/capcompute/sys/replay/tape/journaled"
)

// step is the synthetic per-record duration.
const step = time.Millisecond

// Export walks the journal and emits one root span for the process plus one child
// span per intent (execution and compensation alike). An intent without a
// completion exports as an error span marked open_intent. Records are read
// through the Journal interface only — exporting never mutates.
func Export(ctx context.Context, journal journaled.Journal, tracer trace.Tracer, base time.Time) error {
	header, ok, err := journal.Header()
	if err != nil {
		return err
	}
	if !ok {
		return nil // nothing ran, nothing to export
	}

	length := journal.Length()
	rootCtx, root := tracer.Start(ctx, "process "+header.Process,
		trace.WithTimestamp(base),
		trace.WithAttributes(
			attribute.String("sys.process", header.Process),
			attribute.String("sys.program", header.Program),
			attribute.Int("sys.abi", header.ABI),
		))
	defer root.End(trace.WithTimestamp(base.Add(time.Duration(length+1) * step)))

	for position := 0; position < length; position++ {
		record, err := journal.Load(position)
		if err != nil {
			return err
		}
		if record.Kind != journaled.KindIntent && record.Kind != journaled.KindCompensationIntent {
			continue // completions are folded into their intent's span
		}
		if record.Syscall == nil {
			continue
		}

		start := base.Add(time.Duration(position) * step)
		attributes := []attribute.KeyValue{
			attribute.Int("sys.journal.position", record.Position),
			attribute.String("sys.journal.kind", string(record.Kind)),
			attribute.String("sys.syscall.args", string(record.Syscall.Args)),
		}
		if record.Compensates != nil {
			attributes = append(attributes, attribute.Int("sys.compensates", *record.Compensates))
		}

		_, span := tracer.Start(rootCtx, record.Syscall.Name,
			trace.WithTimestamp(start),
			trace.WithAttributes(attributes...))

		end := start.Add(step)
		if position+1 < length {
			if completion, err := journal.Load(position + 1); err == nil && completion.Result != nil &&
				(completion.Kind == journaled.KindCompletion || completion.Kind == journaled.KindCompensationCompletion) {
				annotateResult(span, *completion.Result)
				end = base.Add(time.Duration(position+2) * step)
				position++ // the completion is consumed
				span.End(trace.WithTimestamp(end))
				continue
			}
		}

		// Open intent: dispatched, outcome never journaled.
		span.SetAttributes(attribute.Bool("sys.open_intent", true))
		span.SetStatus(codes.Error, "open intent: outcome never journaled")
		span.End(trace.WithTimestamp(end))
	}
	return nil
}

func annotateResult(span trace.Span, result sys.SyscallResult) {
	if labels := result.Labels(); len(labels) > 0 {
		span.SetAttributes(attribute.StringSlice("sys.labels", labels))
	}
	switch result.Status() {
	case sys.StatusFailed:
		span.SetAttributes(attribute.String("sys.errno", string(result.Errno())))
		span.SetStatus(codes.Error, result.Message())
	default:
		span.SetStatus(codes.Ok, "")
	}
}
