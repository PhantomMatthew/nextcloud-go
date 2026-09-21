//go:build wasm

package pluginsdk

import (
	"github.com/vmihailenco/msgpack/v5"
)

//go:wasmimport ncgo db_query
func hostDBQuery(sqlPtr, sqlLen, argsPtr, argsLen int32) int64

//go:wasmimport ncgo db_exec
func hostDBExec(sqlPtr, sqlLen, argsPtr, argsLen, outRows int32) int32

//go:wasmimport ncgo db_rows_next
func hostDBRowsNext(handle, outPtr, outMax int32) int32

//go:wasmimport ncgo db_rows_close
func hostDBRowsClose(handle int32) int32

//go:wasmimport ncgo db_tx_begin
func hostDBTxBegin() int64

//go:wasmimport ncgo db_tx_commit
func hostDBTxCommit(tx int32) int32

//go:wasmimport ncgo db_tx_rollback
func hostDBTxRollback(tx int32) int32

//go:wasmimport ncgo db_tx_query
func hostDBTxQuery(tx, sqlPtr, sqlLen, argsPtr, argsLen int32) int64

//go:wasmimport ncgo db_tx_exec
func hostDBTxExec(tx, sqlPtr, sqlLen, argsPtr, argsLen, outRows int32) int32

// encodeArgs marshals the argument list as a MessagePack array.
func encodeArgs(args []any) (int32, int32, int32) {
	if len(args) == 0 {
		// Empty array: single 0x90 byte.
		ptr := alloc(1)
		writeByte(ptr, 0x90)
		return ptr, 1, ErrCodeOK
	}
	raw, err := msgpack.Marshal(args)
	if err != nil {
		return 0, 0, ErrCodeInvalidArgument
	}
	ptr := alloc(int32(len(raw)))
	copyTo(ptr, raw)
	return ptr, int32(len(raw)), ErrCodeOK
}

// unpackI64 splits a packed host result into error code (high) and handle
// (low).
func unpackI64(v int64) (int32, int32) {
	return int32(v >> 32), int32(v)
}

// DBRows iterates a db_query result set.
type DBRows struct {
	handle int32
	row    []any
	err    int32
}

// DBQuery runs a SELECT against granted tables.
func DBQuery(query string, args ...any) (*DBRows, int32) {
	sqlPtr, sqlLen := bytesPtr([]byte(query))
	argsPtr, argsLen, code := encodeArgs(args)
	if code != ErrCodeOK {
		return nil, code
	}
	errCode, handle := unpackI64(hostDBQuery(sqlPtr, sqlLen, argsPtr, argsLen))
	if errCode != ErrCodeOK {
		return nil, errCode
	}
	return &DBRows{handle: handle}, ErrCodeOK
}

// Next advances to the next row; false means EOF or error (see Err).
func (r *DBRows) Next() bool {
	const maxRow = 1 << 20
	ptr := alloc(maxRow)
	n := hostDBRowsNext(r.handle, ptr, maxRow)
	if n < 0 {
		r.err = n
		return false
	}
	if n == 0 {
		return false
	}
	var row []any
	if err := msgpack.Unmarshal(readAt(ptr, n), &row); err != nil {
		r.err = ErrCodeInternal
		return false
	}
	r.row = row
	return true
}

// Scan assigns row values to dest pointers (string, int64, float64, bool,
// []byte supported).
func (r *DBRows) Scan(dest ...any) int32 {
	if len(dest) > len(r.row) {
		return ErrCodeInvalidArgument
	}
	for i, d := range dest {
		if code := assignValue(d, r.row[i]); code != ErrCodeOK {
			return code
		}
	}
	return ErrCodeOK
}

// Err returns the error that ended iteration, if any.
func (r *DBRows) Err() int32 { return r.err }

// Close releases the rows handle.
func (r *DBRows) Close() int32 { return hostDBRowsClose(r.handle) }

// DBExec runs INSERT/UPDATE/DELETE against granted tables (DDL only inside
// install/uninstall hooks) and returns rows affected.
func DBExec(query string, args ...any) (int64, int32) {
	sqlPtr, sqlLen := bytesPtr([]byte(query))
	argsPtr, argsLen, code := encodeArgs(args)
	if code != ErrCodeOK {
		return 0, code
	}
	outPtr := alloc(8)
	if code := hostDBExec(sqlPtr, sqlLen, argsPtr, argsLen, outPtr); code != ErrCodeOK {
		return 0, code
	}
	return readI64LE(outPtr), ErrCodeOK
}

// DBTx is an open transaction.
type DBTx struct{ handle int32 }

// DBTxBegin starts a transaction. Requires any db grant.
func DBTxBegin() (*DBTx, int32) {
	errCode, handle := unpackI64(hostDBTxBegin())
	if errCode != ErrCodeOK {
		return nil, errCode
	}
	return &DBTx{handle: handle}, ErrCodeOK
}

// Commit commits the transaction.
func (tx *DBTx) Commit() int32 { return hostDBTxCommit(tx.handle) }

// Rollback rolls the transaction back.
func (tx *DBTx) Rollback() int32 { return hostDBTxRollback(tx.handle) }

// Query runs a SELECT inside the transaction.
func (tx *DBTx) Query(query string, args ...any) (*DBRows, int32) {
	sqlPtr, sqlLen := bytesPtr([]byte(query))
	argsPtr, argsLen, code := encodeArgs(args)
	if code != ErrCodeOK {
		return nil, code
	}
	errCode, handle := unpackI64(hostDBTxQuery(tx.handle, sqlPtr, sqlLen, argsPtr, argsLen))
	if errCode != ErrCodeOK {
		return nil, errCode
	}
	return &DBRows{handle: handle}, ErrCodeOK
}

// Exec runs a write inside the transaction.
func (tx *DBTx) Exec(query string, args ...any) (int64, int32) {
	sqlPtr, sqlLen := bytesPtr([]byte(query))
	argsPtr, argsLen, code := encodeArgs(args)
	if code != ErrCodeOK {
		return 0, code
	}
	outPtr := alloc(8)
	if code := hostDBTxExec(tx.handle, sqlPtr, sqlLen, argsPtr, argsLen, outPtr); code != ErrCodeOK {
		return 0, code
	}
	return readI64LE(outPtr), ErrCodeOK
}
