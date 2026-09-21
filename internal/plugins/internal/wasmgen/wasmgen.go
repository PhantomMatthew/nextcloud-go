// Package wasmgen builds minimal WASM binaries for plugin host tests.
package wasmgen

const (
	i32         = 0x7f
	i64         = 0x7e
	functype    = 0x60
	opEnd       = 0x0b
	opI32Const  = 0x41
	opI64Const  = 0x42
	opI32Add    = 0x6a
	opI32And    = 0x71
	opI32Eq     = 0x46
	opI64Eq     = 0x51
	opI64Load   = 0x29
	opLocalGet  = 0x20
	opLocalSet  = 0x21
	opGlobalGet = 0x23
	opGlobalSet = 0x24
	opCall      = 0x10
	opDrop      = 0x1a
	opLoop      = 0x03
	opIf        = 0x04
	opBr        = 0x0c
	blockVoid   = 0x40
)

// Standard type table indices used by module specs.
const (
	tLog       = 0 // (i32,i32,i32)->i32
	tNullToI32 = 1 // ()->i32
	tAlloc     = 2 // (i32)->i32
	tFree      = 3 // (i32,i32)->nil
	tOutMax    = 4 // (i32,i32)->i32
	tFourI32   = 5 // (i32,i32,i32,i32)->i32
	tFiveI32   = 6 // (i32,i32,i32,i32,i32)->i32
	tIncrement = 7 // (i32,i32,i64,i32)->i32
	tSixI32    = 8 // (i32,i32,i32,i32,i32,i32)->i32
	tNullToI64 = 9 // ()->i64
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
