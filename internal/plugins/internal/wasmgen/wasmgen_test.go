package wasmgen

import (
	"context"
	"testing"

	"github.com/tetratelabs/wazero"
)

func TestModulesCompile(t *testing.T) {
	ctx := context.Background()
	r := wazero.NewRuntime(ctx)
	t.Cleanup(func() { _ = r.Close(ctx) })
	for _, tc := range []struct {
		name string
		bin  []byte
	}{
		{"hello", HelloModule("hello from wasm")},
		{"wasi", WASIModule()},
		{"noexports", NoExportsModule()},
		{"loop", LoopModule()},
		{"oob", OOBLogModule()},
		{"counter", CounterModule()},
		{"ctxuser", CtxUserModule()},
		{"cache-rt", CacheRoundTripModule("k", "v")},
		{"cache-incr", CacheIncrementModule("n", 5)},
		{"crypto-hash", CryptoHashModule("abc")},
		{"event-probe", EventProbeModule("files.x", -9)},
		{"event", EventModule("demo.ping", "from-a")},
		{"event-fail", EventFailListenerModule()},
		{"event-trap", EventTrapListenerModule()},
		{"route-probe", RouteProbeModule("/apps/x/y", -3)},
		{"db", DBModule("CREATE TABLE pt_items (path TEXT)", "INSERT INTO pt_items (path) VALUES ('hello-item')", "SELECT path FROM pt_items")},
		{"db-denied", DBDeniedModule("SELECT id FROM users", -3)},
		{"db-tx", DBTxModule("INSERT INTO pt_items (path) VALUES ('tx-item')", "SELECT path FROM pt_items")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			compiled, err := r.CompileModule(ctx, tc.bin)
			if err != nil {
				t.Fatalf("compile: %v (len=%d)", err, len(tc.bin))
			}
			_ = compiled.Close(ctx)
		})
	}
}
