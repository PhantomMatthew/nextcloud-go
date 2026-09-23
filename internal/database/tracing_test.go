package database

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// tracedTestDB opens a sqlite in-memory DB (uniquely named per test so
// shared-cache instances never collide) and returns it wrapped with a
// SpanRecorder-backed provider.
func tracedTestDB(t *testing.T) (DB, *sdktrace.TracerProvider, *tracetest.SpanRecorder) {
	t.Helper()
	name := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	db, err := Open(context.Background(), Config{
		Driver: DialectSQLite,
		DSN:    "file:" + name + "?mode=memory&cache=shared",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() {
		// Background, not t.Context(): the test context is already canceled
		// when cleanups run, and Shutdown needs a live one.
		if err := tp.Shutdown(context.Background()); err != nil {
			t.Errorf("provider shutdown: %v", err)
		}
	})
	return WithTracing(db, tp), tp, sr
}

func spanAttrs(t *testing.T, s sdktrace.ReadOnlySpan) map[string]attribute.Value {
	t.Helper()
	m := make(map[string]attribute.Value, len(s.Attributes()))
	for _, kv := range s.Attributes() {
		m[string(kv.Key)] = kv.Value
	}
	return m
}

// requireClientSpan asserts the invariants every DB span shares and returns
// its attributes for per-operation checks.
func requireClientSpan(t *testing.T, s sdktrace.ReadOnlySpan, name, operation string) map[string]attribute.Value {
	t.Helper()
	if s.Name() != name {
		t.Errorf("span name = %q, want %q", s.Name(), name)
	}
	if s.SpanKind() != trace.SpanKindClient {
		t.Errorf("span kind = %v, want Client", s.SpanKind())
	}
	if s.Status().Code != codes.Unset {
		t.Errorf("span status = %v, want Unset", s.Status().Code)
	}
	attrs := spanAttrs(t, s)
	if got := attrs["db.system"].AsString(); got != "sqlite" {
		t.Errorf("db.system = %q, want sqlite", got)
	}
	if got := attrs["db.operation"].AsString(); got != operation {
		t.Errorf("db.operation = %q, want %q", got, operation)
	}
	return attrs
}

func spanNames(spans []sdktrace.ReadOnlySpan) []string {
	names := make([]string, 0, len(spans))
	for _, s := range spans {
		names = append(names, s.Name())
	}
	return names
}

func TestWithTracingNilProvider(t *testing.T) {
	t.Parallel()
	db, err := Open(context.Background(), Config{
		Driver: DialectSQLite,
		DSN:    "file:nilprovider?mode=memory&cache=shared",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if got := WithTracing(db, nil); got != db {
		t.Errorf("WithTracing(db, nil) = %#v, want the identical inner DB", got)
	}
}

func TestSpanName(t *testing.T) {
	t.Parallel()
	tests := []struct{ q, want string }{
		{"", "SQL"},
		{"   ", "SQL"},
		{"SELECT * FROM t", "SELECT"},
		{"select 1", "SELECT"},
		{"  insert into t (a) values (?)", "INSERT"},
		{"UPDATE t SET a = ?", "UPDATE"},
		{"DELETE FROM t WHERE a = ?", "DELETE"},
		{"REPLACE INTO t (a) VALUES (?)", "REPLACE"},
		{"\n\tPRAGMA foreign_keys", "PRAGMA"},
		{"CREATE TABLE t (a INT)", "CREATE"},
	}
	for _, tt := range tests {
		t.Run(tt.want+"/"+tt.q, func(t *testing.T) {
			t.Parallel()
			if got := spanName(tt.q); got != tt.want {
				t.Errorf("spanName(%q) = %q, want %q", tt.q, got, tt.want)
			}
		})
	}
}

func TestTruncateStatement(t *testing.T) {
	t.Parallel()
	short := "SELECT 1"
	if got := truncateStatement(short); got != short {
		t.Errorf("short statement = %q, want unchanged %q", got, short)
	}
	long := strings.Repeat("a", maxStatementLen+100)
	if got, want := truncateStatement(long), long[:maxStatementLen]+"…"; got != want {
		t.Errorf("truncated len = %d, want %d bytes + ellipsis", len(got), maxStatementLen)
	}
	// A cut landing mid-rune must back off to the rune boundary.
	mb := "x" + strings.Repeat("é", maxStatementLen)
	got := truncateStatement(mb)
	if !strings.HasSuffix(got, "…") {
		t.Error("truncated statement lacks ellipsis suffix")
	}
	if !utf8.ValidString(got) {
		t.Error("truncated statement is not valid UTF-8")
	}
}

func TestTracedBasicSpans(t *testing.T) {
	db, _, sr := tracedTestDB(t)
	ctx := context.Background()

	if err := db.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	res, err := db.Exec(ctx, `INSERT INTO items (id, name) VALUES (?, ?)`, 1, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		t.Fatalf("rows affected = %d, %v", n, err)
	}
	var name string
	if err := db.QueryRow(ctx, `SELECT name FROM items WHERE id = ?`, 1).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "alpha" {
		t.Fatalf("name = %q", name)
	}

	spans := sr.Ended()
	if len(spans) != 4 {
		t.Fatalf("ended spans = %d, want 4: %v", len(spans), spanNames(spans))
	}
	attrs := requireClientSpan(t, spans[0], "PING", "ping")
	if _, ok := attrs["db.statement"]; ok {
		t.Error("PING must not carry db.statement")
	}
	attrs = requireClientSpan(t, spans[1], "CREATE", "create")
	if got := attrs["db.statement"].AsString(); got != `CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT)` {
		t.Errorf("db.statement = %q", got)
	}
	attrs = requireClientSpan(t, spans[2], "INSERT", "insert")
	if got := attrs["db.statement"].AsString(); got != `INSERT INTO items (id, name) VALUES (?, ?)` {
		t.Errorf("db.statement = %q", got)
	}
	if got := attrs["db.rows_affected"].AsInt64(); got != 1 {
		t.Errorf("db.rows_affected = %d, want 1", got)
	}
	attrs = requireClientSpan(t, spans[3], "SELECT", "select")
	if got := attrs["db.statement"].AsString(); !strings.Contains(got, `SELECT name FROM items WHERE id = ?`) {
		t.Errorf("db.statement = %q", got)
	}
	if _, ok := attrs["db.rows_returned"]; ok {
		t.Error("QueryRow spans carry no db.rows_returned")
	}
}

// TestTracedQueryEndsOnce covers both Query end triggers — full iteration
// (Next false) and Close — and proves subsequent calls never end the span a
// second time.
func TestTracedQueryEndsOnce(t *testing.T) {
	db, _, sr := tracedTestDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"alpha", "beta"} {
		if _, err := db.Exec(ctx, `INSERT INTO items (id, name) VALUES (?, ?)`, len(n), n); err != nil {
			t.Fatal(err)
		}
	}
	sr.Reset()

	rows, err := db.Query(ctx, `SELECT name FROM items ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// Iteration exhausted the span; Close afterwards must be a no-op.
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("iterated %d rows, want 2", count)
	}

	rows2, err := db.Query(ctx, `SELECT name FROM items ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	// Close without iterating ends the span; a late Next must not end it again.
	if err := rows2.Close(); err != nil {
		t.Fatal(err)
	}
	if rows2.Next() {
		t.Fatal("Next after Close returned true")
	}

	spans := sr.Ended()
	if len(spans) != 2 {
		t.Fatalf("ended spans = %d, want 2 (one per Query): %v", len(spans), spanNames(spans))
	}
	attrs := requireClientSpan(t, spans[0], "SELECT", "select")
	if got := attrs["db.rows_returned"].AsInt64(); got != 2 {
		t.Errorf("db.rows_returned = %d, want 2", got)
	}
	attrs = requireClientSpan(t, spans[1], "SELECT", "select")
	if got := attrs["db.rows_returned"].AsInt64(); got != 0 {
		t.Errorf("db.rows_returned = %d, want 0 (closed before iteration)", got)
	}
}

func TestTracedTxSpans(t *testing.T) {
	db, _, sr := tracedTestDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	sr.Reset()

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO items (id, name) VALUES (?, ?)`, 1, "alpha"); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := tx.QueryRow(ctx, `SELECT name FROM items WHERE id = ?`, 1).Scan(&name); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(ctx, `SELECT name FROM items`)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for rows.Next() {
		count++
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("tx iterated %d rows, want 1", count)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	tx2, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx2.Rollback(); err != nil {
		t.Fatal(err)
	}

	spans := sr.Ended()
	if len(spans) != 7 {
		t.Fatalf("ended spans = %d, want 7: %v", len(spans), spanNames(spans))
	}
	byName := make(map[string]sdktrace.ReadOnlySpan, len(spans))
	for _, s := range spans {
		byName[s.Name()] = s
	}
	for _, tc := range []struct{ name, op string }{
		{"BEGIN", "begin"},
		{"INSERT", "insert"},
		{"SELECT", "select"},
		{"COMMIT", "commit"},
		{"ROLLBACK", "rollback"},
	} {
		s, ok := byName[tc.name]
		if !ok {
			t.Fatalf("no %s span in %v", tc.name, spanNames(spans))
		}
		requireClientSpan(t, s, tc.name, tc.op)
	}
	// Both SELECT spans (QueryRow and Query) share the name; spans[3] is the
	// Query one (ended by iteration exhaustion) and carried a row count.
	if got := spanAttrs(t, spans[3])["db.rows_returned"].AsInt64(); got != 1 {
		t.Errorf("tx query db.rows_returned = %d, want 1", got)
	}
	// In-transaction spans parent to their BEGIN span.
	begin1 := spans[0]
	for _, i := range []int{1, 2, 3, 4} {
		if got := spans[i].Parent().SpanID(); got != begin1.SpanContext().SpanID() {
			t.Errorf("%s parent = %s, want BEGIN %s", spans[i].Name(), got, begin1.SpanContext().SpanID())
		}
		if got := spans[i].SpanContext().TraceID(); got != begin1.SpanContext().TraceID() {
			t.Errorf("%s trace = %s, want BEGIN trace %s", spans[i].Name(), got, begin1.SpanContext().TraceID())
		}
	}
	begin2 := spans[5]
	if got := spans[6].Parent().SpanID(); got != begin2.SpanContext().SpanID() {
		t.Errorf("ROLLBACK parent = %s, want second BEGIN %s", got, begin2.SpanContext().SpanID())
	}
}

func TestTracedErrorSpans(t *testing.T) {
	db, _, sr := tracedTestDB(t)
	ctx := context.Background()

	if _, err := db.Exec(ctx, `INSERT INTO missing_table VALUES (?)`, 1); err == nil {
		t.Fatal("exec: expected an error")
	}
	// A start error ends the Query span immediately with Error status.
	if _, err := db.Query(ctx, `SELECT WHERE`); err == nil {
		t.Fatal("query: expected an error")
	}

	spans := sr.Ended()
	if len(spans) != 2 {
		t.Fatalf("ended spans = %d, want 2: %v", len(spans), spanNames(spans))
	}
	for _, s := range spans {
		if s.Status().Code != codes.Error {
			t.Errorf("%s status = %v, want Error", s.Name(), s.Status().Code)
		}
		if s.Status().Description == "" {
			t.Errorf("%s Error status has no description", s.Name())
		}
		recorded := false
		for _, ev := range s.Events() {
			if ev.Name == "exception" {
				recorded = true
			}
		}
		if !recorded {
			t.Errorf("%s recorded no exception event", s.Name())
		}
	}
}

func TestTracedQueryRowNoRows(t *testing.T) {
	db, _, sr := tracedTestDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `CREATE TABLE items (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	sr.Reset()

	row := db.QueryRow(ctx, `SELECT id FROM items WHERE id = ?`, 42)
	var id int
	err := row.Scan(&id)
	if !errors.Is(err, ErrNoRows) {
		t.Fatalf("scan err = %v, want ErrNoRows", err)
	}
	// A repeated Scan (always an error here) must not end the span again.
	_ = row.Scan(&id)

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1: %v", len(spans), spanNames(spans))
	}
	s := spans[0]
	requireClientSpan(t, s, "SELECT", "select")
	if s.Status().Code == codes.Error {
		t.Error("not-found QueryRow must not be an Error span")
	}
	if len(s.Events()) != 0 {
		t.Errorf("not-found QueryRow recorded events: %v", s.Events())
	}
}

func TestTracedStatementTruncated(t *testing.T) {
	db, _, sr := tracedTestDB(t)
	q := `SELECT '` + strings.Repeat("a", maxStatementLen) + `'`
	if _, err := db.Exec(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	attrs := requireClientSpan(t, spans[0], "SELECT", "select")
	if got, want := attrs["db.statement"].AsString(), q[:maxStatementLen]+"…"; got != want {
		t.Errorf("db.statement len = %d, want %d bytes + ellipsis", len(got), maxStatementLen)
	}
}

func TestTracedParenting(t *testing.T) {
	db, tp, sr := tracedTestDB(t)
	tracer := tp.Tracer("test")
	ctx, parent := tracer.Start(context.Background(), "parent")

	if _, err := db.Exec(ctx, `CREATE TABLE items (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO items (id) VALUES (?)`, 1); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	parent.End()

	spans := sr.Ended()
	if len(spans) != 5 {
		t.Fatalf("ended spans = %d, want 5: %v", len(spans), spanNames(spans))
	}
	traceID := parent.SpanContext().TraceID()
	parentID := parent.SpanContext().SpanID()
	byName := make(map[string]sdktrace.ReadOnlySpan, len(spans))
	for _, s := range spans {
		byName[s.Name()] = s
		if s.Name() == "parent" {
			continue
		}
		if got := s.SpanContext().TraceID(); got != traceID {
			t.Errorf("%s trace = %s, want parent trace %s", s.Name(), got, traceID)
		}
	}
	for _, name := range []string{"CREATE", "BEGIN"} {
		if got := byName[name].Parent().SpanID(); got != parentID {
			t.Errorf("%s parent = %s, want %s", name, got, parentID)
		}
	}
	beginID := byName["BEGIN"].SpanContext().SpanID()
	for _, name := range []string{"INSERT", "COMMIT"} {
		if got := byName[name].Parent().SpanID(); got != beginID {
			t.Errorf("%s parent = %s, want BEGIN %s", name, got, beginID)
		}
	}
}
