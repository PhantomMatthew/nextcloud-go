// Package wasmgen builds minimal WASM binaries for plugin host tests.
package wasmgen

const (
	i32           = 0x7f
	i64           = 0x7e
	functype      = 0x60
	opEnd         = 0x0b
	opI32Const    = 0x41
	opI64Const    = 0x42
	opI32Add      = 0x6a
	opI32And      = 0x71
	opI32Eq       = 0x46
	opI64Eq       = 0x51
	opI64Load     = 0x29
	opLocalGet    = 0x20
	opLocalSet    = 0x21
	opGlobalGet   = 0x23
	opGlobalSet   = 0x24
	opCall        = 0x10
	opDrop        = 0x1a
	opBlock       = 0x02
	opLoop        = 0x03
	opIf          = 0x04
	opBr          = 0x0c
	opBrIf        = 0x0d
	opI32Load8U   = 0x2d
	opI32Store8   = 0x3a
	opI32GeU      = 0x4f
	opI64ShrU     = 0x88
	opI32WrapI64  = 0xa7
	opUnreachable = 0x00
	blockVoid     = 0x40
)

// Standard type table indices used by module specs.
const (
	tLog        = 0  // (i32,i32,i32)->i32
	tNullToI32  = 1  // ()->i32
	tAlloc      = 2  // (i32)->i32
	tFree       = 3  // (i32,i32)->nil
	tOutMax     = 4  // (i32,i32)->i32
	tFourI32    = 5  // (i32,i32,i32,i32)->i32
	tFiveI32    = 6  // (i32,i32,i32,i32,i32)->i32
	tIncrement  = 7  // (i32,i32,i64,i32)->i32
	tSixI32     = 8  // (i32,i32,i32,i32,i32,i32)->i32
	tNullToI64  = 9  // ()->i64
	tFourI32I64 = 10 // (i32,i32,i32,i32)->i64
	tFiveI32I64 = 11 // (i32,i32,i32,i32,i32)->i64
)

func u32(n uint32) []byte {
	var b []byte
	for {
		c := byte(n & 0x7f)
		n >>= 7
		if n != 0 {
			c |= 0x80
		}
		b = append(b, c)
		if n == 0 {
			break
		}
	}
	return b
}

func s64(v int64) []byte {
	var b []byte
	for {
		c := byte(v & 0x7f)
		v >>= 7
		more := (v != 0 || c&0x40 != 0) && (v != -1 || c&0x40 == 0)
		if more {
			c |= 0x80
		}
		b = append(b, c)
		if !more {
			break
		}
	}
	return b
}

func s32(v int32) []byte { return s64(int64(v)) }

func i32n(n int) int32 {
	if n < 0 || n > 4096 {
		return 0
	}
	return int32(n)
}

func u32len(n int) uint32 {
	if n < 0 || n > 1<<20 {
		return 0
	}
	return uint32(n)
}

func name(s string) []byte {
	b := u32(u32len(len(s)))
	return append(b, []byte(s)...)
}

func section(id byte, payload []byte) []byte {
	out := []byte{id}
	out = append(out, u32(u32len(len(payload)))...)
	return append(out, payload...)
}

func vec(items ...[]byte) []byte {
	out := u32(u32len(len(items)))
	for _, it := range items {
		out = append(out, it...)
	}
	return out
}

func ft(params, results []byte) []byte {
	b := make([]byte, 0, 4+len(params)+len(results))
	b = append(b, functype)
	b = append(b, u32(u32len(len(params)))...)
	b = append(b, params...)
	b = append(b, u32(u32len(len(results)))...)
	b = append(b, results...)
	return b
}

func i32c(v int32) []byte {
	return append([]byte{opI32Const}, s32(v)...)
}

func i64c(v int64) []byte {
	return append([]byte{opI64Const}, s64(v)...)
}

func code(localsI32 int, expr []byte) []byte {
	var body []byte
	if localsI32 == 0 {
		body = append(body, 0x00)
	} else {
		body = append(body, 0x01)
		body = append(body, u32(u32len(localsI32))...)
		body = append(body, i32)
	}
	body = append(body, expr...)
	body = append(body, opEnd)
	out := u32(u32len(len(body)))
	return append(out, body...)
}

func align8(n uint32) uint32 {
	return (n + 7) &^ 7
}

func export(n string, kind byte, idx uint32) []byte {
	b := name(n)
	b = append(b, kind)
	b = append(b, u32(idx)...)
	return b
}

func typeTable() []byte {
	return vec(
		ft([]byte{i32, i32, i32}, []byte{i32}),                // 0 log
		ft(nil, []byte{i32}),                                  // 1 ()->i32
		ft([]byte{i32}, []byte{i32}),                          // 2 alloc
		ft([]byte{i32, i32}, nil),                             // 3 free
		ft([]byte{i32, i32}, []byte{i32}),                     // 4 (out,max)->i32
		ft([]byte{i32, i32, i32, i32}, []byte{i32}),           // 5
		ft([]byte{i32, i32, i32, i32, i32}, []byte{i32}),      // 6
		ft([]byte{i32, i32, i64, i32}, []byte{i32}),           // 7 increment
		ft([]byte{i32, i32, i32, i32, i32, i32}, []byte{i32}), // 8
		ft(nil, []byte{i64}),                                  // 9 ()->i64
		ft([]byte{i32, i32, i32, i32}, []byte{i64}),           // 10 db_query
		ft([]byte{i32, i32, i32, i32, i32}, []byte{i64}),      // 11 db_tx_query
	)
}

// imp declares one host import: ncgo.<name> with a type table index.
type imp struct {
	name string
	typ  uint32
}

// extraFn is an additional exported guest function.
type extraFn struct {
	name   string
	typ    uint32
	locals int
	expr   []byte
}

// guestSpec describes a full test module.
type guestSpec struct {
	imports         []imp
	onInstall       []byte // must leave one i32 on the stack
	onInstallLocals int
	data            [][]byte
	extras          []extraFn
	counter         bool // add a mutable i32 global at index 1
}

// dataOffsets returns the aligned offset of each data blob.
func (s guestSpec) dataOffsets() []int32 {
	offs := make([]int32, len(s.data))
	pos := uint32(64)
	for i, blob := range s.data {
		offs[i] = int32(pos)
		pos += align8(u32len(len(blob)))
	}
	return offs
}

func (s guestSpec) build() []byte {
	imports := make([][]byte, 0, len(s.imports))
	for _, im := range s.imports {
		e := append(name("ncgo"), name(im.name)...)
		e = append(e, 0x00) // func import
		e = append(e, u32(im.typ)...)
		imports = append(imports, e)
	}
	base := uint32(len(imports)) //nolint:gosec // G115: test modules have few imports

	fns := make([][]byte, 0, 4+len(s.extras))
	fns = append(fns, []byte{tNullToI32}, []byte{tAlloc}, []byte{tFree}, []byte{tNullToI32})
	for _, ex := range s.extras {
		fns = append(fns, []byte{byte(ex.typ)}) //nolint:gosec // G115: type indices are small constants
	}

	heap := uint32(64)
	for _, blob := range s.data {
		heap += align8(u32len(len(blob)))
	}
	if heap < 64 {
		heap = 64
	}

	globals := [][]byte{
		append(append([]byte{i32, 0x01}, i32c(int32(heap))...), opEnd),
	}
	if s.counter {
		globals = append(globals, append(append([]byte{i32, 0x01}, i32c(0)...), opEnd))
	}

	exports := make([][]byte, 0, 5+len(s.extras))
	exports = append(exports,
		export("memory", 0x02, 0),
		export("ncgo_abi_version", 0x00, base),
		export("ncgo_alloc", 0x00, base+1),
		export("ncgo_free", 0x00, base+2),
		export("ncgo_on_install", 0x00, base+3),
	)
	for i, ex := range s.extras {
		exports = append(exports, export(ex.name, 0x00, base+4+uint32(i)))
	}

	allocExpr := make([]byte, 0, 24)
	allocExpr = append(allocExpr,
		opGlobalGet, 0x00,
		opLocalSet, 0x01,
		opGlobalGet, 0x00,
		opLocalGet, 0x00,
		opI32Add,
		opI32Const,
	)
	allocExpr = append(allocExpr, s32(7)...)
	allocExpr = append(allocExpr, opI32Add, opI32Const)
	allocExpr = append(allocExpr, s32(-8)...)
	allocExpr = append(allocExpr, opI32And, opGlobalSet, 0x00, opLocalGet, 0x01)

	codes := make([][]byte, 0, 4+len(s.extras))
	codes = append(codes,
		code(0, i32c(1)),
		code(1, allocExpr),
		code(0, nil),
		code(s.onInstallLocals, s.onInstall),
	)
	for _, ex := range s.extras {
		codes = append(codes, code(ex.locals, ex.expr))
	}

	out := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	out = append(out, section(1, typeTable())...)
	out = append(out, section(2, vec(imports...))...)
	out = append(out, section(3, vec(fns...))...)
	out = append(out, section(5, vec(append([]byte{0x00}, u32(1)...)))...)
	out = append(out, section(6, vec(globals...))...)
	out = append(out, section(7, vec(exports...))...)
	out = append(out, section(10, vec(codes...))...)

	offs := s.dataOffsets()
	inits := make([][]byte, 0, len(s.data))
	for i, blob := range s.data {
		if len(blob) == 0 {
			continue
		}
		init := append([]byte{0x00}, i32c(offs[i])...)
		init = append(init, opEnd)
		init = append(init, u32(u32len(len(blob)))...)
		init = append(init, blob...)
		inits = append(inits, init)
	}
	if len(inits) > 0 {
		out = append(out, section(11, vec(inits...))...)
	}
	return out
}

// logCall emits log(1, ptr, len) for import index 0.
func logCall(ptr, length int32) []byte {
	expr := append(i32c(1), i32c(ptr)...)
	expr = append(expr, i32c(length)...)
	return append(expr, opCall, 0x00)
}

// HelloModule returns a wasm binary that logs msg at info on ncgo_on_install.
func HelloModule(msg string) []byte {
	s := guestSpec{
		imports:   []imp{{"log", tLog}},
		onInstall: logCall(64, i32n(len(msg))),
		data:      [][]byte{[]byte(msg)},
	}
	return s.build()
}

// OOBLogModule calls ncgo.log with an out-of-bounds pointer.
func OOBLogModule() []byte {
	s := guestSpec{
		imports:   []imp{{"log", tLog}},
		onInstall: logCall(0x7fffffff, 4),
	}
	return s.build()
}

// LoopModule runs an infinite loop in ncgo_on_install.
func LoopModule() []byte {
	loop := make([]byte, 0, 8)
	loop = append(loop, opLoop, blockVoid, opBr, 0x00, opEnd)
	loop = append(loop, i32c(0)...)
	s := guestSpec{onInstall: loop}
	return s.build()
}

// CounterModule exports bump() -> i32, incrementing a mutable global.
func CounterModule() []byte {
	bump := make([]byte, 0, 12)
	bump = append(bump, opGlobalGet, 0x01)
	bump = append(bump, i32c(1)...)
	bump = append(bump, opI32Add, opGlobalSet, 0x01, opGlobalGet, 0x01)
	s := guestSpec{
		onInstall: i32c(0),
		extras:    []extraFn{{name: "bump", typ: tNullToI32, expr: bump}},
		counter:   true,
	}
	return s.build()
}

// CtxUserModule logs the ctx user id at info on install.
func CtxUserModule() []byte {
	expr := make([]byte, 0, 32)
	expr = append(expr, i32c(256)...)
	expr = append(expr, i32c(64)...)
	expr = append(expr, opCall, 0x01) // ctx_user_id(256, 64)
	expr = append(expr, opLocalSet, 0x00)
	expr = append(expr, i32c(1)...)
	expr = append(expr, i32c(256)...)
	expr = append(expr, opLocalGet, 0x00)
	expr = append(expr, opCall, 0x00) // log(1, 256, n)
	s := guestSpec{
		imports:         []imp{{"log", tLog}, {"ctx_user_id", tOutMax}},
		onInstall:       expr,
		onInstallLocals: 1,
	}
	return s.build()
}

// CacheRoundTripModule stores val under key via cache_set, reads it back
// with cache_get, and logs the fetched bytes.
func CacheRoundTripModule(key, val string) []byte {
	s := guestSpec{
		imports: []imp{{"log", tLog}, {"cache_set", tFiveI32}, {"cache_get", tFourI32}},
		data:    [][]byte{[]byte(key), []byte(val)},
	}
	offs := s.dataOffsets()
	keyOff, keyLen := offs[0], i32n(len(key))
	valOff, valLen := offs[1], i32n(len(val))

	expr := make([]byte, 0, 64)
	expr = append(expr, i32c(keyOff)...)
	expr = append(expr, i32c(keyLen)...)
	expr = append(expr, i32c(valOff)...)
	expr = append(expr, i32c(valLen)...)
	expr = append(expr, i32c(60)...)
	expr = append(expr, opCall, 0x01, opDrop) // cache_set
	expr = append(expr, i32c(keyOff)...)
	expr = append(expr, i32c(keyLen)...)
	expr = append(expr, i32c(256)...)
	expr = append(expr, i32c(1024)...)
	expr = append(expr, opCall, 0x02) // cache_get
	expr = append(expr, opLocalSet, 0x00)
	expr = append(expr, i32c(1)...)
	expr = append(expr, i32c(256)...)
	expr = append(expr, opLocalGet, 0x00)
	expr = append(expr, opCall, 0x00) // log fetched value
	s.onInstall = expr
	s.onInstallLocals = 1
	return s.build()
}

// CacheIncrementModule increments key by delta and logs "incr-ok" when the
// returned counter equals delta.
func CacheIncrementModule(key string, delta int64) []byte {
	s := guestSpec{
		imports: []imp{{"log", tLog}, {"cache_increment", tIncrement}},
		data:    [][]byte{[]byte(key), []byte("incr-ok")},
	}
	offs := s.dataOffsets()
	keyOff, keyLen := offs[0], i32n(len(key))
	msgOff, msgLen := offs[1], i32n(len("incr-ok"))

	expr := make([]byte, 0, 64)
	expr = append(expr, i32c(keyOff)...)
	expr = append(expr, i32c(keyLen)...)
	expr = append(expr, i64c(delta)...)
	expr = append(expr, i32c(256)...)
	expr = append(expr, opCall, 0x01, opDrop) // cache_increment(key, delta, out=256)
	expr = append(expr, i32c(0)...)           // base address for the load
	expr = append(expr, opI64Load, 0x00)      // align 0
	expr = append(expr, u32(256)...)          // offset 256
	expr = append(expr, i64c(delta)...)
	expr = append(expr, opI64Eq, opIf, blockVoid)
	expr = append(expr, logCall(msgOff, msgLen)...)
	expr = append(expr, opDrop)
	expr = append(expr, opEnd)
	expr = append(expr, i32c(0)...)
	s.onInstall = expr
	return s.build()
}

// CryptoHashModule hashes input with SHA-256 and logs "hash-ok" when the
// host reports a 32-byte digest.
func CryptoHashModule(input string) []byte {
	s := guestSpec{
		imports: []imp{{"log", tLog}, {"crypto_hash", tFiveI32}},
		data:    [][]byte{[]byte(input), []byte("hash-ok")},
	}
	offs := s.dataOffsets()
	inOff, inLen := offs[0], i32n(len(input))
	msgOff, msgLen := offs[1], i32n(len("hash-ok"))

	expr := make([]byte, 0, 48)
	expr = append(expr, i32c(0)...) // algo sha256
	expr = append(expr, i32c(inOff)...)
	expr = append(expr, i32c(inLen)...)
	expr = append(expr, i32c(256)...)
	expr = append(expr, i32c(64)...)
	expr = append(expr, opCall, 0x01) // crypto_hash
	expr = append(expr, i32c(32)...)
	expr = append(expr, opI32Eq, opIf, blockVoid)
	expr = append(expr, logCall(msgOff, msgLen)...)
	expr = append(expr, opDrop)
	expr = append(expr, opEnd)
	expr = append(expr, i32c(0)...)
	s.onInstall = expr
	return s.build()
}

// EventProbeModule publishes topic and logs "probe-ok" when the host returns
// the expected code (e.g. -3 denied, -9 unsupported).
func EventProbeModule(topic string, want int32) []byte {
	s := guestSpec{
		imports: []imp{{"log", tLog}, {"event_publish", tFourI32}},
		data:    [][]byte{[]byte(topic), []byte("probe-ok")},
	}
	offs := s.dataOffsets()
	topicOff, topicLen := offs[0], i32n(len(topic))
	msgOff, msgLen := offs[1], i32n(len("probe-ok"))

	expr := make([]byte, 0, 48)
	expr = append(expr, i32c(topicOff)...)
	expr = append(expr, i32c(topicLen)...)
	expr = append(expr, i32c(0)...)
	expr = append(expr, i32c(0)...)
	expr = append(expr, opCall, 0x01) // event_publish(topic, "", 0)
	expr = append(expr, i32c(want)...)
	expr = append(expr, opI32Eq, opIf, blockVoid)
	expr = append(expr, logCall(msgOff, msgLen)...)
	expr = append(expr, opDrop)
	expr = append(expr, opEnd)
	expr = append(expr, i32c(0)...)
	s.onInstall = expr
	return s.build()
}

// RouteProbeModule registers path and logs "probe-ok" when the host returns
// the expected code.
func RouteProbeModule(path string, want int32) []byte {
	s := guestSpec{
		imports: []imp{{"log", tLog}, {"route_register", tSixI32}},
		data:    [][]byte{[]byte(path), []byte("probe-ok")},
	}
	offs := s.dataOffsets()
	pathOff, pathLen := offs[0], i32n(len(path))
	msgOff, msgLen := offs[1], i32n(len("probe-ok"))

	expr := make([]byte, 0, 48)
	expr = append(expr, i32c(0)...) // method ""
	expr = append(expr, i32c(0)...)
	expr = append(expr, i32c(pathOff)...)
	expr = append(expr, i32c(pathLen)...)
	expr = append(expr, i32c(0)...) // handler ""
	expr = append(expr, i32c(0)...)
	expr = append(expr, opCall, 0x01) // route_register
	expr = append(expr, i32c(want)...)
	expr = append(expr, opI32Eq, opIf, blockVoid)
	expr = append(expr, logCall(msgOff, msgLen)...)
	expr = append(expr, opDrop)
	expr = append(expr, opEnd)
	expr = append(expr, i32c(0)...)
	s.onInstall = expr
	return s.build()
}

// DBModule builds a module exercising the db_* host functions:
// on_install (hook context) runs createSQL + insertSQL via db_exec;
// the exported probe entry runs selectSQL, logs the first row's MessagePack
// bytes, verifies EOF on the second db_rows_next, and closes the handle;
// ddlprobe re-runs createSQL outside a hook and logs "ddl-ok" when the
// host answers -3 (permission denied).
func DBModule(createSQL, insertSQL, selectSQL string) []byte {
	s := guestSpec{
		imports: []imp{
			{"log", tLog},
			{"db_exec", tFiveI32},
			{"db_query", tFourI32I64},
			{"db_rows_next", tLog},
			{"db_rows_close", tAlloc},
		},
		data: [][]byte{[]byte(createSQL), []byte(insertSQL), []byte(selectSQL), {0x90}, []byte("eof-ok"), []byte("ddl-ok")},
	}
	offs := s.dataOffsets()
	argsOff := offs[3]

	execCall := func(sqlOff, sqlLen int32) []byte {
		expr := append(i32c(sqlOff), i32c(sqlLen)...)
		expr = append(expr, i32c(argsOff)...)
		expr = append(expr, i32c(1)...)
		expr = append(expr, i32c(0)...) // out_rows = null
		return append(expr, opCall, 0x01, opDrop)
	}

	onInstall := execCall(offs[0], i32n(len(createSQL)))
	onInstall = append(onInstall, execCall(offs[1], i32n(len(insertSQL)))...)
	onInstall = append(onInstall, i32c(0)...)
	s.onInstall = onInstall

	probe := make([]byte, 0, 64)
	probe = append(probe, i32c(offs[2])...)
	probe = append(probe, i32c(i32n(len(selectSQL)))...)
	probe = append(probe, i32c(argsOff)...)
	probe = append(probe, i32c(1)...)
	probe = append(probe, opCall, 0x02) // db_query
	probe = append(probe, opI32WrapI64, opLocalSet, 0x00)
	probe = append(probe, opLocalGet, 0x00)
	probe = append(probe, i32c(256)...)
	probe = append(probe, i32c(1024)...)
	probe = append(probe, opCall, 0x03) // db_rows_next
	probe = append(probe, opLocalSet, 0x01)
	probe = append(probe, i32c(1)...)
	probe = append(probe, i32c(256)...)
	probe = append(probe, opLocalGet, 0x01)
	probe = append(probe, opCall, 0x00, opDrop) // log row bytes
	probe = append(probe, opLocalGet, 0x00)
	probe = append(probe, i32c(256)...)
	probe = append(probe, i32c(1024)...)
	probe = append(probe, opCall, 0x03) // second next → EOF
	probe = append(probe, i32c(0)...)
	probe = append(probe, opI32Eq, opIf, blockVoid)
	probe = append(probe, logCall(offs[4], i32n(len("eof-ok")))...)
	probe = append(probe, opDrop)
	probe = append(probe, opEnd)
	probe = append(probe, opLocalGet, 0x00)
	probe = append(probe, opCall, 0x04, opDrop) // db_rows_close
	probe = append(probe, i32c(0)...)
	s.extras = append(s.extras, extraFn{name: "probe", typ: tNullToI32, locals: 2, expr: probe})

	ddl := make([]byte, 0, 32)
	ddl = append(ddl, i32c(offs[0])...)
	ddl = append(ddl, i32c(i32n(len(createSQL)))...)
	ddl = append(ddl, i32c(argsOff)...)
	ddl = append(ddl, i32c(1)...)
	ddl = append(ddl, i32c(0)...)
	ddl = append(ddl, opCall, 0x01) // db_exec outside a hook
	ddl = append(ddl, i32c(-3)...)
	ddl = append(ddl, opI32Eq, opIf, blockVoid)
	ddl = append(ddl, logCall(offs[5], i32n(len("ddl-ok")))...)
	ddl = append(ddl, opDrop)
	ddl = append(ddl, opEnd)
	ddl = append(ddl, i32c(0)...)
	s.extras = append(s.extras, extraFn{name: "ddlprobe", typ: tNullToI32, expr: ddl})
	return s.build()
}

// DBDeniedModule runs querySQL via db_query and logs "denied-ok" when the
// host returns the expected error code in the high 32 bits.
func DBDeniedModule(querySQL string, want int32) []byte {
	s := guestSpec{
		imports: []imp{{"log", tLog}, {"db_query", tFourI32I64}},
		data:    [][]byte{[]byte(querySQL), []byte("denied-ok")},
	}
	offs := s.dataOffsets()

	expr := make([]byte, 0, 48)
	expr = append(expr, i32c(offs[0])...)
	expr = append(expr, i32c(i32n(len(querySQL)))...)
	expr = append(expr, i32c(0)...)
	expr = append(expr, i32c(0)...) // empty args
	expr = append(expr, opCall, 0x01)
	expr = append(expr, i64c(32)...)
	expr = append(expr, opI64ShrU, opI32WrapI64) // high32 = error code
	expr = append(expr, i32c(want)...)
	expr = append(expr, opI32Eq, opIf, blockVoid)
	expr = append(expr, logCall(offs[1], i32n(len("denied-ok")))...)
	expr = append(expr, opDrop)
	expr = append(expr, opEnd)
	expr = append(expr, i32c(0)...)
	s.onInstall = expr
	return s.build()
}

// DBTxModule exercises transactions on install: tx1 inserts insertSQL and
// selects selectSQL inside the tx (logging the row) then commits; tx2
// inserts and rolls back, then logs "rb-ok" when committing the rolled-back
// handle fails with -4 (not found).
func DBTxModule(insertSQL, selectSQL string) []byte {
	s := guestSpec{
		imports: []imp{
			{"log", tLog},                // 0
			{"db_tx_begin", tNullToI64},  // 1
			{"db_tx_exec", tSixI32},      // 2
			{"db_tx_query", tFiveI32I64}, // 3
			{"db_rows_next", tLog},       // 4
			{"db_tx_commit", tAlloc},     // 5
			{"db_tx_rollback", tAlloc},   // 6
			{"db_rows_close", tAlloc},    // 7
		},
		data: [][]byte{[]byte(insertSQL), []byte(selectSQL), {0x90}, []byte("rb-ok")},
	}
	offs := s.dataOffsets()
	insOff, insLen := offs[0], i32n(len(insertSQL))
	selOff, selLen := offs[1], i32n(len(selectSQL))
	argsOff := offs[2]
	msgOff, msgLen := offs[3], i32n(len("rb-ok"))

	// locals: 0 = tx handle, 1 = rows handle, 2 = row byte count
	expr := make([]byte, 0, 128)
	// tx1: begin, insert, select, log row, close rows, commit
	expr = append(expr, opCall, 0x01, opI32WrapI64, opLocalSet, 0x00)
	expr = append(expr, opLocalGet, 0x00)
	expr = append(expr, i32c(insOff)...)
	expr = append(expr, i32c(insLen)...)
	expr = append(expr, i32c(argsOff)...)
	expr = append(expr, i32c(1)...)
	expr = append(expr, i32c(0)...)
	expr = append(expr, opCall, 0x02, opDrop) // tx_exec insert
	expr = append(expr, opLocalGet, 0x00)
	expr = append(expr, i32c(selOff)...)
	expr = append(expr, i32c(selLen)...)
	expr = append(expr, i32c(argsOff)...)
	expr = append(expr, i32c(1)...)
	expr = append(expr, opCall, 0x03, opI32WrapI64, opLocalSet, 0x01) // tx_query
	expr = append(expr, opLocalGet, 0x01)
	expr = append(expr, i32c(256)...)
	expr = append(expr, i32c(1024)...)
	expr = append(expr, opCall, 0x04, opLocalSet, 0x02) // rows_next
	expr = append(expr, i32c(1)...)
	expr = append(expr, i32c(256)...)
	expr = append(expr, opLocalGet, 0x02)
	expr = append(expr, opCall, 0x00, opDrop) // log row
	expr = append(expr, opLocalGet, 0x01)
	expr = append(expr, opCall, 0x07, opDrop) // rows_close
	expr = append(expr, opLocalGet, 0x00)
	expr = append(expr, opCall, 0x05, opDrop) // commit tx1
	// tx2: begin, insert, rollback; commit on the dead handle must give -4
	expr = append(expr, opCall, 0x01, opI32WrapI64, opLocalSet, 0x00)
	expr = append(expr, opLocalGet, 0x00)
	expr = append(expr, i32c(insOff)...)
	expr = append(expr, i32c(insLen)...)
	expr = append(expr, i32c(argsOff)...)
	expr = append(expr, i32c(1)...)
	expr = append(expr, i32c(0)...)
	expr = append(expr, opCall, 0x02, opDrop) // tx_exec insert
	expr = append(expr, opLocalGet, 0x00)
	expr = append(expr, opCall, 0x06, opDrop) // rollback tx2
	expr = append(expr, opLocalGet, 0x00)
	expr = append(expr, opCall, 0x05) // commit dead handle
	expr = append(expr, i32c(-4)...)
	expr = append(expr, opI32Eq, opIf, blockVoid)
	expr = append(expr, logCall(msgOff, msgLen)...)
	expr = append(expr, opDrop)
	expr = append(expr, opEnd)
	expr = append(expr, i32c(0)...)
	s.onInstall = expr
	s.onInstallLocals = 3
	return s.build()
}

// memcpy emits dst[0:n] = src[0:n] byte-by-byte; iLocal is clobbered.
func memcpy(dst, src, n []byte, iLocal uint32) []byte {
	e := i32c(0)
	e = append(e, opLocalSet, byte(iLocal)) //nolint:gosec // G115: local indices are small constants
	e = append(e, opBlock, blockVoid, opLoop, blockVoid)
	e = append(e, opLocalGet, byte(iLocal)) //nolint:gosec // G115: local indices are small constants
	e = append(e, n...)
	e = append(e, opI32GeU, opBrIf, 0x01)
	e = append(e, dst...)
	e = append(e, opLocalGet, byte(iLocal), opI32Add) //nolint:gosec // G115: local indices are small constants
	e = append(e, src...)
	e = append(e, opLocalGet, byte(iLocal), opI32Add) //nolint:gosec // G115: local indices are small constants
	e = append(e, opI32Load8U, 0x00, 0x00)
	e = append(e, opI32Store8, 0x00, 0x00)
	e = append(e, opLocalGet, byte(iLocal)) //nolint:gosec // G115: local indices are small constants
	e = append(e, i32c(1)...)
	e = append(e, opI32Add, opLocalSet, byte(iLocal)) //nolint:gosec // G115: local indices are small constants
	e = append(e, opBr, 0x00, opEnd, opEnd)
	return e
}

// EventModule builds an event subscriber/publisher probe: ncgo_on_event logs
// "event <topic> <payload>" at info, and do_publish calls event_publish with
// the baked-in topic/payload and returns the host's result code.
func EventModule(publishTopic, publishPayload string) []byte {
	const prefix = "event "
	s := guestSpec{
		imports:   []imp{{"log", tLog}, {"event_publish", tFourI32}},
		data:      [][]byte{[]byte(prefix), []byte(publishTopic), []byte(publishPayload)},
		onInstall: i32c(0),
	}
	offs := s.dataOffsets()
	allocIdx := uint32(len(s.imports)) + 1 //nolint:gosec // G115: test modules have few imports

	// params: 0=topicPtr 1=topicLen 2=payloadPtr 3=payloadLen
	// locals: 4=dst 5=i 6=total
	onEvent := i32c(int32(len(prefix)))
	onEvent = append(onEvent, opLocalGet, 0x01, opI32Add)
	onEvent = append(onEvent, i32c(1)...)
	onEvent = append(onEvent, opI32Add, opLocalGet, 0x03, opI32Add, opLocalSet, 0x06)
	onEvent = append(onEvent, opLocalGet, 0x06, opCall)
	onEvent = append(onEvent, u32(allocIdx)...)
	onEvent = append(onEvent, opLocalSet, 0x04) // dst = alloc(total)
	onEvent = append(onEvent, memcpy([]byte{opLocalGet, 0x04}, i32c(offs[0]), i32c(int32(len(prefix))), 5)...)
	dstTopic := append([]byte{opLocalGet, 0x04}, i32c(int32(len(prefix)))...)
	dstTopic = append(dstTopic, opI32Add)
	onEvent = append(onEvent, memcpy(dstTopic, []byte{opLocalGet, 0x00}, []byte{opLocalGet, 0x01}, 5)...)
	onEvent = append(onEvent, dstTopic...)
	onEvent = append(onEvent, opLocalGet, 0x01, opI32Add)
	onEvent = append(onEvent, i32c(0x20)...)
	onEvent = append(onEvent, opI32Store8, 0x00, 0x00) // dst[6+topicLen] = ' '
	dstPayload := append([]byte{opLocalGet, 0x04}, i32c(int32(len(prefix)+1))...)
	dstPayload = append(dstPayload, opI32Add, opLocalGet, 0x01, opI32Add)
	onEvent = append(onEvent, memcpy(dstPayload, []byte{opLocalGet, 0x02}, []byte{opLocalGet, 0x03}, 5)...)
	onEvent = append(onEvent, i32c(1)...)
	onEvent = append(onEvent, opLocalGet, 0x04, opLocalGet, 0x06, opCall, 0x00, opDrop) // log built message
	onEvent = append(onEvent, i32c(0)...)
	s.extras = append(s.extras, extraFn{name: "ncgo_on_event", typ: tFourI32, locals: 3, expr: onEvent})

	doPublish := i32c(offs[1])
	doPublish = append(doPublish, i32c(i32n(len(publishTopic)))...)
	doPublish = append(doPublish, i32c(offs[2])...)
	doPublish = append(doPublish, i32c(i32n(len(publishPayload)))...)
	doPublish = append(doPublish, opCall, 0x01) // event_publish; the code is the return value
	s.extras = append(s.extras, extraFn{name: "do_publish", typ: tNullToI32, expr: doPublish})
	return s.build()
}

// EventFailListenerModule exports ncgo_on_event returning a non-zero code.
func EventFailListenerModule() []byte {
	s := guestSpec{onInstall: i32c(0)}
	s.extras = []extraFn{{name: "ncgo_on_event", typ: tFourI32, expr: i32c(7)}}
	return s.build()
}

// EventTrapListenerModule exports ncgo_on_event that traps (unreachable).
func EventTrapListenerModule() []byte {
	s := guestSpec{onInstall: i32c(0)}
	s.extras = []extraFn{{name: "ncgo_on_event", typ: tFourI32, expr: []byte{opUnreachable}}}
	return s.build()
}

// WASIModule imports wasi_snapshot_preview1.fd_write.
func WASIModule() []byte {
	types := vec(ft([]byte{i32, i32, i32, i32}, []byte{i32}))
	imp := append(name("wasi_snapshot_preview1"), name("fd_write")...)
	imp = append(imp, 0x00)
	imp = append(imp, u32(0)...)
	out := make([]byte, 0, 64)
	out = append(out, 0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00)
	out = append(out, section(1, types)...)
	out = append(out, section(2, vec(imp))...)
	return out
}

// NoExportsModule is a valid empty wasm module.
func NoExportsModule() []byte {
	return []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
}
