package database

import (
	"context"
	"errors"
	"strings"
	"sync"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// tracerScope is the instrumentation scope name attached to every span this
// package produces (the ADR-0072 per-package convention).
const tracerScope = "github.com/PhantomMatthew/nextcloud-go/internal/database"

// maxStatementLen bounds the db.statement attribute; longer statements are
// truncated and suffixed with an ellipsis.
const maxStatementLen = 4096

// WithTracing returns a DB that wraps every database call of inner in an
// OpenTelemetry client span (ADR-0075). A nil tp returns inner unchanged, so
// a deployment with no configured collector pays exactly zero overhead. The
// wrapped Tx/Rows/Row values end their spans exactly once (sync.Once), and
// Close stays untraced: it is local resource teardown, not a database call.
func WithTracing(inner DB, tp trace.TracerProvider) DB {
	if tp == nil {
		return inner
	}
	return &tracedDB{
		inner: inner,
		dbTracer: dbTracer{
			tracer: tp.Tracer(tracerScope),
			system: dbSystem(inner.Dialect()),
		},
	}
}

// dbSystem maps the dialect to the OTel db.system attribute value.
func dbSystem(d Dialect) string {
	if d == DialectPostgres {
		return "postgresql"
	}
	return string(d)
}

// spanName is the uppercase first SQL keyword of q (SELECT, INSERT, ...).
// Span names stay low-cardinality by construction: the statement text is
// never part of the name, and an empty statement names the span "SQL".
func spanName(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return "SQL"
	}
	word := q
	if i := strings.IndexAny(word, " \t\r\n"); i >= 0 {
		word = word[:i]
	}
	return strings.ToUpper(word)
}

// truncateStatement caps q at maxStatementLen bytes, backing off to a rune
// boundary so the attribute never carries half a UTF-8 sequence.
func truncateStatement(q string) string {
	if len(q) <= maxStatementLen {
		return q
	}
	cut := maxStatementLen
	for cut > 0 && !utf8.RuneStart(q[cut]) {
		cut--
	}
	return q[:cut] + "…"
}

// endWithErr records a non-nil err on the span with Error status, then ends
// the span.
func endWithErr(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

// dbTracer starts the package's client spans; tracedDB and tracedTx share it.
type dbTracer struct {
	tracer trace.Tracer
	system string
}

// statement begins a client span for one SQL statement. The db.statement
// attribute carries the SQL as received — squirrel-built with "?"
// placeholders (Rebind runs inside sqlDB, below this wrapper), so it is
// static per call site and contains no literal values.
func (t dbTracer) statement(ctx context.Context, q string) trace.Span {
	name := spanName(q)
	_, span := t.tracer.Start(ctx, name,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("db.system", t.system),
			attribute.String("db.operation", strings.ToLower(name)),
			attribute.String("db.statement", truncateStatement(q)),
		))
	return span
}

// op begins a client span for a non-statement operation (BEGIN, COMMIT,
// ROLLBACK, PING), which carries no db.statement attribute.
func (t dbTracer) op(ctx context.Context, name string) (context.Context, trace.Span) {
	return t.tracer.Start(ctx, name,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("db.system", t.system),
			attribute.String("db.operation", strings.ToLower(name)),
		))
}

type tracedDB struct {
	inner DB
	dbTracer
}

func (d *tracedDB) Dialect() Dialect { return d.inner.Dialect() }

// Close passes through untraced: pool teardown is local, not a database call.
func (d *tracedDB) Close() error { return d.inner.Close() }

func (d *tracedDB) Ping(ctx context.Context) error {
	_, span := d.op(ctx, "PING")
	err := d.inner.Ping(ctx)
	endWithErr(span, err)
	return err
}

func (d *tracedDB) Query(ctx context.Context, q string, args ...any) (Rows, error) {
	span := d.statement(ctx, q)
	rows, err := d.inner.Query(ctx, q, args...)
	if err != nil {
		endWithErr(span, err)
		return nil, err
	}
	return &tracedRows{Rows: rows, span: span}, nil
}

func (d *tracedDB) QueryRow(ctx context.Context, q string, args ...any) Row {
	span := d.statement(ctx, q)
	return &tracedRow{Row: d.inner.QueryRow(ctx, q, args...), span: span}
}

func (d *tracedDB) Exec(ctx context.Context, q string, args ...any) (Result, error) {
	span := d.statement(ctx, q)
	res, err := d.inner.Exec(ctx, q, args...)
	if err != nil {
		endWithErr(span, err)
		return nil, err
	}
	if n, rerr := res.RowsAffected(); rerr == nil {
		span.SetAttributes(attribute.Int64("db.rows_affected", n))
	}
	span.End()
	return res, nil
}

func (d *tracedDB) Begin(ctx context.Context) (Tx, error) {
	beginCtx, span := d.op(ctx, "BEGIN")
	tx, err := d.inner.Begin(ctx)
	if err != nil {
		endWithErr(span, err)
		return nil, err
	}
	span.End()
	// The BEGIN span ends at return, but its context travels with the Tx so
	// every in-transaction statement span (and COMMIT/ROLLBACK) parents to
	// BEGIN and shares its trace.
	return &tracedTx{inner: tx, dbTracer: d.dbTracer, beginCtx: beginCtx}, nil
}

// tracedTx wraps a Tx; statement spans start from beginCtx (parenting) while
// the caller's ctx still drives the inner call (cancellation).
type tracedTx struct {
	inner Tx
	dbTracer
	beginCtx context.Context
}

func (t *tracedTx) Query(ctx context.Context, q string, args ...any) (Rows, error) {
	span := t.statement(t.beginCtx, q)
	rows, err := t.inner.Query(ctx, q, args...)
	if err != nil {
		endWithErr(span, err)
		return nil, err
	}
	return &tracedRows{Rows: rows, span: span}, nil
}

func (t *tracedTx) QueryRow(ctx context.Context, q string, args ...any) Row {
	span := t.statement(t.beginCtx, q)
	return &tracedRow{Row: t.inner.QueryRow(ctx, q, args...), span: span}
}

func (t *tracedTx) Exec(ctx context.Context, q string, args ...any) (Result, error) {
	span := t.statement(t.beginCtx, q)
	res, err := t.inner.Exec(ctx, q, args...)
	if err != nil {
		endWithErr(span, err)
		return nil, err
	}
	if n, rerr := res.RowsAffected(); rerr == nil {
		span.SetAttributes(attribute.Int64("db.rows_affected", n))
	}
	span.End()
	return res, nil
}

func (t *tracedTx) Commit() error {
	_, span := t.op(t.beginCtx, "COMMIT")
	err := t.inner.Commit()
	endWithErr(span, err)
	return err
}

func (t *tracedTx) Rollback() error {
	_, span := t.op(t.beginCtx, "ROLLBACK")
	err := t.inner.Rollback()
	endWithErr(span, err)
	return err
}

// tracedRows ends the Query span exactly once: on Close, or on Next
// returning false, whichever comes first.
type tracedRows struct {
	Rows
	span trace.Span
	once sync.Once
	n    int64
}

func (r *tracedRows) Next() bool {
	if !r.Rows.Next() {
		r.finish()
		return false
	}
	r.n++
	return true
}

func (r *tracedRows) Close() error {
	err := r.Rows.Close()
	r.finish()
	return err
}

func (r *tracedRows) finish() {
	r.once.Do(func() {
		if err := r.Err(); err != nil {
			r.span.RecordError(err)
			r.span.SetStatus(codes.Error, err.Error())
		}
		r.span.SetAttributes(attribute.Int64("db.rows_returned", r.n))
		r.span.End()
	})
}

// tracedRow ends the QueryRow span at the first Scan. Callers always Scan a
// QueryRow result; ADR-0075 documents that a never-Scanned Row would leak an
// unended span (visible as a missing span, never wrong data).
type tracedRow struct {
	Row
	span trace.Span
	once sync.Once
}

func (r *tracedRow) Scan(dest ...any) error {
	err := r.Row.Scan(dest...)
	r.once.Do(func() {
		// ErrNoRows IS sql.ErrNoRows (re-exported in errors.go), so one
		// errors.Is covers both spellings of a normal not-found.
		if err != nil && !errors.Is(err, ErrNoRows) {
			r.span.RecordError(err)
			r.span.SetStatus(codes.Error, err.Error())
		}
		r.span.End()
	})
	return err
}

var (
	_ DB   = (*tracedDB)(nil)
	_ Tx   = (*tracedTx)(nil)
	_ Rows = (*tracedRows)(nil)
	_ Row  = (*tracedRow)(nil)
)
