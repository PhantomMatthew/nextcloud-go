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
		{"route-reg", RouteRegModule(false, true, []RouteReg{{Method: "GET", Path: "/apps/x/y", Handler: "h"}}, 0)},
		{"route-reg-ocs-nohook", RouteRegModule(true, false, []RouteReg{{Method: "POST", Path: "/apps/x/z", Handler: "h2"}}, -3)},
		{"route", RouteModule(200, []string{"X-A: b"}, "body")},
		{"route-body-fail", RouteBodyFailModule(200, "b", -5)},
		{"route-opts", RouteModuleOpts(RouteOpts{Status: 200, Body: "b", HeaderCount: -1, HeaderAtCode: -2, StatusTraps: false, CloseTraps: true})},
		{"route-opts-status-trap", RouteModuleOpts(RouteOpts{StatusTraps: true, Headers: []string{"X-A: b"}})},
		{"route-fail", RouteFailModule(-7)},
		{"route-trap", RouteTrapModule()},
		{"route-no-response", RouteNoResponseModule()},
		{"storage", StorageModule("user:/d/a.txt", "user:/d/b.txt", "user:/d", "hello", "system:/x", -3)},
		{"storage-stat-probe", StorageStatProbeModule("user:/a", -3)},
		{"storage-open-loop", StorageOpenLoopModule("user:/a", 65, -12)},
		{"storage-leak", StorageLeakModule("user:/a", "user:/b", "c")},
		{"storage-write-probe", StorageWriteProbeModule("user:/a", "c", -11)},
		{"storage-event-stat", StorageEventStatModule("user:/a")},
		{"storage-read-probe", StorageReadProbeModule("user:/a", "user:/d")},
		{"storage-op-create", StorageOpProbeModule("create", "user:/a", "", -3)},
		{"storage-op-delete", StorageOpProbeModule("delete", "user:/a", "", -3)},
		{"storage-op-rename", StorageOpProbeModule("rename", "user:/a", "user:/b", -3)},
		{"http-outbound", HTTPOutboundModule([]byte{0x90}, []byte{0x90}, "X-Test", 200, -3)},
		{"http-outbound-no-denied", HTTPOutboundModule([]byte{0x90}, nil, "X-Test", 200, 0)},
		{"http-probe", HTTPProbeModule([]byte{0x90}, -3)},
		{"http-open-loop", HTTPOpenLoopModule([]byte{0x90}, 17, -12)},
		{"http-stale", HTTPStaleModule([]byte{0x90})},
		{"http-absent-header", HTTPAbsentHeaderModule([]byte{0x90}, "X-Missing")},
		{"http-leak", HTTPLeakModule([]byte{0x90})},
		{"config", ConfigModule("tokens.x", "v", "tokens.x", "tokens.miss", "other", -3)},
		{"config-read-only", ConfigModule("", "", "", "foo", "", 0)},
		{"config-get-probe", ConfigGetProbeModule("foo", 4096, -3)},
		{"config-set-probe", ConfigSetProbeModule("foo", 65537, -11)},
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
