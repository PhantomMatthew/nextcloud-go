//go:build !wasm

package pluginsdk

// DBRows is a no-op on non-wasm builds so the host can import this package.
type DBRows struct{}

// Next is a no-op on non-wasm builds.
func (*DBRows) Next() bool { return false }

// Scan is a no-op on non-wasm builds.
func (*DBRows) Scan(...any) int32 { return ErrCodeUnsupported }

// Err is a no-op on non-wasm builds.
func (*DBRows) Err() int32 { return ErrCodeUnsupported }

// Close is a no-op on non-wasm builds.
func (*DBRows) Close() int32 { return ErrCodeUnsupported }

// DBQuery is a no-op on non-wasm builds.
func DBQuery(string, ...any) (*DBRows, int32) { return nil, ErrCodeUnsupported }

// DBExec is a no-op on non-wasm builds.
func DBExec(string, ...any) (int64, int32) { return 0, ErrCodeUnsupported }

// DBTx is a no-op on non-wasm builds.
type DBTx struct{}

// DBTxBegin is a no-op on non-wasm builds.
func DBTxBegin() (*DBTx, int32) { return nil, ErrCodeUnsupported }

// Commit is a no-op on non-wasm builds.
func (*DBTx) Commit() int32 { return ErrCodeUnsupported }

// Rollback is a no-op on non-wasm builds.
func (*DBTx) Rollback() int32 { return ErrCodeUnsupported }

// Query is a non-wasm no-op.
func (*DBTx) Query(string, ...any) (*DBRows, int32) { return nil, ErrCodeUnsupported }

// Exec is a non-wasm no-op.
func (*DBTx) Exec(string, ...any) (int64, int32) { return 0, ErrCodeUnsupported }
