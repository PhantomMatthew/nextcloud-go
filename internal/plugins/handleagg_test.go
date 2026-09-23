package plugins

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/migrations"
	"github.com/PhantomMatthew/nextcloud-go/internal/plugins/internal/wasmgen"
)

// testFileDB opens a file-backed sqlite database: the aggregate tests hold
// up to 16 rows handles open at once, and testDB's mode=memory DSN forces
// MaxOpenConns=1, which serializes (then times out) any second open cursor.
func testFileDB(t *testing.T) database.DB {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, database.Config{
		Driver: database.DialectSQLite,
		DSN:    filepath.Join(t.TempDir(), "agg.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	std, ok := database.Unwrap(db)
	if !ok {
		t.Fatal("unwrap")
	}
	if _, err := migrations.Up(ctx, std, database.DialectSQLite, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestHandleAggregateBoundary(t *testing.T) {
	t.Parallel()
	agg := newHandleAggregate()
	for i := int32(0); i < maxRowsHandles; i++ {
		if !agg.acquire("p1", handleRows) {
			t.Fatalf("acquire %d denied, want allow", i)
		}
	}
	if agg.acquire("p1", handleRows) {
		t.Fatal("acquire past the budget allowed, want deny")
	}
	agg.release("p1", handleRows)
	if !agg.acquire("p1", handleRows) {
		t.Fatal("acquire after release denied, want allow")
	}
	for i := int32(0); i < maxRowsHandles; i++ {
		agg.release("p1", handleRows)
	}
	if got := len(agg.per); got != 0 {
		t.Fatalf("per entries = %d, want 0 (zero count deletes the key)", got)
	}
}

// Budgets are keyed by handle kind: exhausting rows never blocks streams.
func TestHandleAggregateKindIsolation(t *testing.T) {
	t.Parallel()
	agg := newHandleAggregate()
	for i := int32(0); i < maxRowsHandles; i++ {
		if !agg.acquire("p1", handleRows) {
			t.Fatalf("rows acquire %d denied, want allow", i)
		}
	}
	if !agg.acquire("p1", handleStream) {
		t.Fatal("stream acquire denied, want isolated budget")
	}
	if !agg.acquire("p1", handleHTTP) {
		t.Fatal("http acquire denied, want isolated budget")
	}
	if agg.acquire("p1", handleRows) {
		t.Fatal("rows acquire past the budget allowed, want deny")
	}
}

// Budgets are keyed by plugin id: one plugin's exhaustion never blocks
// another.
func TestHandleAggregatePluginIsolation(t *testing.T) {
	t.Parallel()
	agg := newHandleAggregate()
	for i := int32(0); i < maxHTTPHandles; i++ {
		if !agg.acquire("plugin.a", handleHTTP) {
			t.Fatalf("plugin.a acquire %d denied, want allow", i)
		}
	}
	if agg.acquire("plugin.a", handleHTTP) {
		t.Fatal("plugin.a acquire past the budget allowed, want deny")
	}
	if !agg.acquire("plugin.b", handleHTTP) {
		t.Fatal("plugin.b acquire denied, want isolated budget")
	}
	agg.release("plugin.b", handleHTTP)
	if got := len(agg.per); got != 1 {
		t.Fatalf("per entries = %d, want 1 (only plugin.a held)", got)
	}
}

// Concurrent acquire/release churn across plugins and kinds must leave the
// map exactly empty; run under -race to pin the lock-internal accounting.
func TestHandleAggregateConcurrentChurn(t *testing.T) {
	t.Parallel()
	agg := newHandleAggregate()
	kinds := []handleKind{handleStream, handleRows, handleHTTP}
	ids := []string{"p1", "p2", "p3"}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			id := ids[g%len(ids)]
			kind := kinds[g%len(kinds)]
			for i := 0; i < 200; i++ {
				if agg.acquire(id, kind) {
					agg.release(id, kind)
				}
			}
		}(g)
	}
	wg.Wait()
	if got := len(agg.per); got != 0 {
		t.Fatalf("per entries = %d after churn, want 0", got)
	}
}

// Two instances of one plugin share the aggregate: a 10+6 split fills the
// 16-rows budget, the 17th add fails from either table, and remove/closeAll
// return slots precisely.
func TestHandleTableSharedAggregate(t *testing.T) {
	t.Parallel()
	agg := newHandleAggregate()
	a := newSharedHandleTable(agg, "p1")
	b := newSharedHandleTable(agg, "p1")
	for i := 0; i < 10; i++ {
		if _, err := a.add(handleRows, i); err != nil {
			t.Fatalf("a add %d: %v", i, err)
		}
	}
	var bFirst int32
	for i := 0; i < 6; i++ {
		id, err := b.add(handleRows, i)
		if err != nil {
			t.Fatalf("b add %d: %v", i, err)
		}
		if i == 0 {
			bFirst = id
		}
	}
	// Neither table is full (10 and 6 of 16), so this refusal is the
	// aggregate's; it must consume no slot.
	if _, err := a.add(handleRows, nil); !errors.Is(err, ErrHandleLimit) {
		t.Fatalf("17th add err = %v, want ErrHandleLimit", err)
	}
	if _, err := b.add(handleRows, nil); !errors.Is(err, ErrHandleLimit) {
		t.Fatalf("17th add from b err = %v, want ErrHandleLimit", err)
	}
	if _, ok := b.remove(bFirst, handleRows); !ok {
		t.Fatal("b remove failed")
	}
	if _, err := a.add(handleRows, nil); err != nil {
		t.Fatalf("add after remove: %v", err)
	}
	if err := a.closeAll(); err != nil {
		t.Fatal(err)
	}
	if err := b.closeAll(); err != nil {
		t.Fatal(err)
	}
	if got := len(agg.per); got != 0 {
		t.Fatalf("per entries = %d after closeAll, want 0 (no leaked slots)", got)
	}
}

// A table-full refusal runs before the aggregate acquire, so it must not
// consume a slot either; a table holding the full budget returns exactly
// that many on closeAll.
func TestHandleTableFullRefusalKeepsAggregate(t *testing.T) {
	t.Parallel()
	agg := newHandleAggregate()
	a := newSharedHandleTable(agg, "p1")
	for i := int32(0); i < maxRowsHandles; i++ {
		if _, err := a.add(handleRows, i); err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	if _, err := a.add(handleRows, nil); !errors.Is(err, ErrHandleLimit) {
		t.Fatalf("err = %v, want ErrHandleLimit", err)
	}
	// The raw aggregate must still see exactly maxRowsHandles held: one more
	// raw acquire denied, and closeAll drains the map to empty.
	if agg.acquire("p1", handleRows) {
		t.Fatal("raw aggregate acquire allowed past the budget, want deny")
	}
	if err := a.closeAll(); err != nil {
		t.Fatal(err)
	}
	if got := len(agg.per); got != 0 {
		t.Fatalf("per entries = %d after closeAll, want 0", got)
	}
}

// gateServer serves /fast immediately and blocks /gate until open is called,
// signaling arrival on reached. It lets one guest call hold handles open
// while a concurrent call runs on another instance of the same plugin.
type gateServer struct {
	*httptest.Server
	reached chan struct{}
	open    func()
}

func newGateServer(t *testing.T) *gateServer {
	t.Helper()
	g := &gateServer{reached: make(chan struct{})}
	release := make(chan struct{})
	var reachedOnce, releaseOnce sync.Once
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gate" {
			reachedOnce.Do(func() { close(g.reached) })
			<-release
		}
		_, _ = w.Write([]byte("x"))
	}))
	g.open = func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		g.open()
		g.Close()
	})
	return g
}

// ADR-0068: the 16-rows budget is per plugin across instances — with
// instance A holding 10 rows and instance B 6, the 17th open fails -12 even
// though neither instance's own table is full.
func TestHandleAggregatePooledRows(t *testing.T) {
	db := testFileDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, dbCreateSQL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, dbInsertSQL); err != nil {
		t.Fatal(err)
	}
	gate := newGateServer(t)
	h, buf := testHost(t, HostConfig{DB: db})
	m := dbManifest()
	m.Runtime.InstanceModel = "pooled"
	m.Runtime.PoolSize = 2
	m.Capabilities.HTTP = HTTPCapabilities{Outbound: []string{httpHostPort(gate.Server)}, OutboundAllowPrivate: true}
	gateReq := httpReqBytes(t, "GET", gate.URL+"/gate", nil, nil, 0)
	p, err := h.Load(ctx, m, wasmgen.AggregateHoldProbeModule("db_query", 4, []byte(dbSelectSQL), gateReq, 10, 6, ErrCodeUnavailable))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	if err := p.Install(ctx); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := p.Call(ctx, "hold"); err != nil {
			t.Errorf("hold: %v", err)
		}
	}()
	select {
	case <-gate.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("hold never reached the gate")
	}
	if _, err := p.Call(ctx, "probe"); err != nil {
		t.Fatal(err)
	}
	gate.open()
	wg.Wait()
	out := buf.String()
	if !strings.Contains(out, "held-ok") {
		t.Fatalf("hold fill marker missing: %q", out)
	}
	if !strings.Contains(out, "budget-ok") {
		t.Fatalf("aggregate refusal marker missing: %q", out)
	}
}

// ADR-0068: same cross-instance shape for the 16-HTTP-response budget.
func TestHandleAggregatePooledHTTP(t *testing.T) {
	ctx := context.Background()
	gate := newGateServer(t)
	h, buf := testHost(t, HostConfig{})
	m := httpManifest(httpHostPort(gate.Server))
	m.Runtime.InstanceModel = "pooled"
	m.Runtime.PoolSize = 2
	fastReq := httpReqBytes(t, "GET", gate.URL+"/fast", nil, nil, 0)
	gateReq := httpReqBytes(t, "GET", gate.URL+"/gate", nil, nil, 0)
	p, err := h.Load(ctx, m, wasmgen.AggregateHoldProbeModule("http_request", 2, fastReq, gateReq, 10, 6, ErrCodeUnavailable))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	if err := p.Install(ctx); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := p.Call(ctx, "hold"); err != nil {
			t.Errorf("hold: %v", err)
		}
	}()
	select {
	case <-gate.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("hold never reached the gate")
	}
	if _, err := p.Call(ctx, "probe"); err != nil {
		t.Fatal(err)
	}
	gate.open()
	wg.Wait()
	out := buf.String()
	if !strings.Contains(out, "held-ok") {
		t.Fatalf("hold fill marker missing: %q", out)
	}
	if !strings.Contains(out, "budget-ok") {
		t.Fatalf("aggregate refusal marker missing: %q", out)
	}
}

// ADR-0068: same cross-instance shape for the 64-stream budget (40 + 24
// fill it; the 65th open fails -12).
func TestHandleAggregatePooledStreams(t *testing.T) {
	f := newStorageFixture(t)
	f.mkdirAndWrite(t, "/docs", "/docs/a.txt", "hello")
	ctx := context.Background()
	gate := newGateServer(t)
	h, buf := testHost(t, f.hostConfig())
	m := storageManifest([]string{"user"}, nil)
	m.Runtime.InstanceModel = "pooled"
	m.Runtime.PoolSize = 2
	m.Capabilities.HTTP = HTTPCapabilities{Outbound: []string{httpHostPort(gate.Server)}, OutboundAllowPrivate: true}
	gateReq := httpReqBytes(t, "GET", gate.URL+"/gate", nil, nil, 0)
	p, err := h.Load(ctx, m, wasmgen.AggregateHoldProbeModule("storage_open", 2, []byte("user:/docs/a.txt"), gateReq, 40, 24, ErrCodeUnavailable))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	if err := p.Install(ctx); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := p.Call(aliceCtx(), "hold"); err != nil {
			t.Errorf("hold: %v", err)
		}
	}()
	select {
	case <-gate.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("hold never reached the gate")
	}
	if _, err := p.Call(aliceCtx(), "probe"); err != nil {
		t.Fatal(err)
	}
	gate.open()
	wg.Wait()
	out := buf.String()
	if !strings.Contains(out, "held-ok") {
		t.Fatalf("hold fill marker missing: %q", out)
	}
	if !strings.Contains(out, "budget-ok") {
		t.Fatalf("aggregate refusal marker missing: %q", out)
	}
}

// ADR-0068: a trapped instance returns its aggregate slots exactly — after
// filltrap destroys the instance holding all 16 rows handles, refill opens
// the full 16 again and the 17th still fails -12. A single leaked slot
// would fail the refill's 16th open.
func TestHandleAggregateTrapReturnsSlots(t *testing.T) {
	db := testFileDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, dbCreateSQL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, dbInsertSQL); err != nil {
		t.Fatal(err)
	}
	h, buf := testHost(t, HostConfig{DB: db})
	m := dbManifest()
	m.Runtime.InstanceModel = "pooled"
	m.Runtime.PoolSize = 1
	p, err := h.Load(ctx, m, wasmgen.AggregateFillTrapModule("db_query", 4, []byte(dbSelectSQL), 16, ErrCodeUnavailable))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	if err := p.Install(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Call(ctx, "filltrap"); !errors.Is(err, ErrTrap) {
		t.Fatalf("filltrap err = %v, want ErrTrap", err)
	}
	if _, err := p.Call(ctx, "refill"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, marker := range []string{"fill-ok", "refill-open-ok", "refill-ok"} {
		if !strings.Contains(out, marker) {
			t.Fatalf("marker %q missing from log: %q", marker, out)
		}
	}
}

// ADR-0068: the normal per_request release path (closeAll on instance
// close) also returns every aggregate slot — a second refill fills the full
// budget again.
func TestHandleAggregatePerRequestRefill(t *testing.T) {
	db := testFileDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, dbCreateSQL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, dbInsertSQL); err != nil {
		t.Fatal(err)
	}
	h, buf := testHost(t, HostConfig{DB: db})
	p, err := h.Load(ctx, dbManifest(), wasmgen.AggregateFillTrapModule("db_query", 4, []byte(dbSelectSQL), 16, ErrCodeUnavailable))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	if err := p.Install(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := p.Call(ctx, "refill"); err != nil {
			t.Fatalf("refill %d: %v", i, err)
		}
	}
	out := buf.String()
	if got := strings.Count(out, "refill-open-ok"); got != 2 {
		t.Fatalf("refill-open-ok count = %d, want 2: %q", got, out)
	}
	if got := strings.Count(out, "refill-ok"); got != 2 {
		t.Fatalf("refill-ok count = %d, want 2: %q", got, out)
	}
}
