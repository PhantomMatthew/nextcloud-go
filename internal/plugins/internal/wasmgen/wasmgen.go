// Package wasmgen builds minimal WASM binaries for plugin host tests.
package wasmgen

const (
	i32         = 0x7f
	functype    = 0x60
	opEnd       = 0x0b
	opI32Const  = 0x41
	opI32Add    = 0x6a
	opI32And    = 0x71
	opLocalGet  = 0x20
	opLocalSet  = 0x21
	opGlobalGet = 0x23
	opGlobalSet = 0x24
	opCall      = 0x10
	opLoop      = 0x03
	opBr        = 0x0c
	blockVoid   = 0x40
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

func s32(v int32) []byte {
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

// HelloModule returns a wasm binary that logs msg at info on ncgo_on_install.
func HelloModule(msg string) []byte {
	expr := append(i32c(1), i32c(64)...)
	expr = append(expr, i32c(i32n(len(msg)))...)
	expr = append(expr, opCall, 0x00)
	return guestModule(expr, []byte(msg))
}

// OOBLogModule calls ncgo.log with an out-of-bounds pointer.
func OOBLogModule() []byte {
	expr := append(i32c(1), i32c(0x7fffffff)...)
	expr = append(expr, i32c(4)...)
	expr = append(expr, opCall, 0x00)
	return guestModule(expr, nil)
}

// LoopModule runs an infinite loop in ncgo_on_install.
func LoopModule() []byte {
	loop := make([]byte, 0, 8)
	loop = append(loop, opLoop, blockVoid, opBr, 0x00, opEnd)
	loop = append(loop, i32c(0)...)
	return guestModule(loop, nil)
}

func guestModule(onInstall, data []byte) []byte {
	const dataOff = 64
	heap := align8(dataOff + u32len(len(data)))
	if len(data) == 0 {
		heap = 64
	}

	types := vec(
		ft([]byte{i32, i32, i32}, []byte{i32}), // 0 log
		ft(nil, []byte{i32}),                   // 1 ()->i32
		ft([]byte{i32}, []byte{i32}),           // 2 alloc
		ft([]byte{i32, i32}, nil),              // 3 free
	)

	imp := append(name("ncgo"), name("log")...)
	imp = append(imp, 0x00) // func
	imp = append(imp, u32(0)...)
	imports := vec(imp)

	fns := vec([]byte{0x01}, []byte{0x02}, []byte{0x03}, []byte{0x01})

	mem := vec(append([]byte{0x00}, u32(1)...)) // min 1 page

	globInit := append([]byte{i32, 0x01}, i32c(int32(heap))...)
	globInit = append(globInit, opEnd)
	globals := vec(globInit)

	exports := vec(
		export("memory", 0x02, 0),
		export("ncgo_abi_version", 0x00, 1),
		export("ncgo_alloc", 0x00, 2),
		export("ncgo_free", 0x00, 3),
		export("ncgo_on_install", 0x00, 4),
	)

	abiExpr := i32c(1)

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

	codes := vec(
		code(0, abiExpr),
		code(1, allocExpr),
		code(0, nil),
		code(0, onInstall),
	)

	out := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	out = append(out, section(1, types)...)
	out = append(out, section(2, imports)...)
	out = append(out, section(3, fns)...)
	out = append(out, section(5, mem)...)
	out = append(out, section(6, globals)...)
	out = append(out, section(7, exports)...)
	out = append(out, section(10, codes)...)
	if len(data) > 0 {
		init := append([]byte{0x00}, i32c(dataOff)...)
		init = append(init, opEnd)
		init = append(init, u32(u32len(len(data)))...)
		init = append(init, data...)
		out = append(out, section(11, vec(init))...)
	}
	return out
}

func export(n string, kind byte, idx uint32) []byte {
	b := name(n)
	b = append(b, kind)
	b = append(b, u32(idx)...)
	return b
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
