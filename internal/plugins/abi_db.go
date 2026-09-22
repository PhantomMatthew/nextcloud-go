package plugins

import (
	"context"
	"encoding/binary"
	"time"

	"github.com/tetratelabs/wazero/api"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/pkg/pluginsdk"
)

// dbQueryer is the subset of database.DB / database.Tx the db_* host
// functions need.
type dbQueryer interface {
	Query(ctx context.Context, q string, args ...any) (database.Rows, error)
	Exec(ctx context.Context, q string, args ...any) (database.Result, error)
}

// readQuery reads the SQL text and MessagePack argument array from the
// guest's memory.
func readQuery(mod api.Module, sqlPtr, sqlLen, argsPtr, argsLen int32) (string, []any, int32) {
	query, code := readString(mod, sqlPtr, sqlLen)
	if code != pluginsdk.ErrCodeOK {
		return "", nil, code
	}
	raw, code := readBytes(mod, argsPtr, argsLen)
	if code != pluginsdk.ErrCodeOK {
		return "", nil, code
	}
	var args []any
	if len(raw) > 0 {
		if err := msgpack.Unmarshal(raw, &args); err != nil {
			return "", nil, pluginsdk.ErrCodeInvalidArgument
		}
	}
	return query, args, pluginsdk.ErrCodeOK
}

// checkQuery parses the statement and enforces the capability model:
// db_query is SELECT-only, db_exec takes writes (and DDL inside lifecycle
// hooks), every referenced table must match the corresponding glob grant.
func checkQuery(ctx context.Context, mod api.Module, sqlPtr, sqlLen, argsPtr, argsLen int32, wantWrite bool) (string, []any, int32) {
	query, args, code := readQuery(mod, sqlPtr, sqlLen, argsPtr, argsLen)
	if code != pluginsdk.ErrCodeOK {
		return "", nil, code
	}
	kind, tables, err := parsePluginSQL(query)
	if err != nil {
		return "", nil, pluginsdk.ErrCodeInvalidArgument
	}
	info := callFromCtx(ctx)
	caps := pluginCaps(ctx)
	switch kind {
	case stmtSelect:
		if wantWrite {
			return "", nil, pluginsdk.ErrCodeInvalidArgument
		}
	case stmtWrite:
		if !wantWrite {
			return "", nil, pluginsdk.ErrCodeInvalidArgument
		}
	case stmtDDL:
		if !wantWrite || !info.inHook {
			return "", nil, pluginsdk.ErrCodePermissionDenied
		}
	default:
		return "", nil, pluginsdk.ErrCodePermissionDenied
	}
	if !checkDBTables(caps, kind, tables) {
		return "", nil, pluginsdk.ErrCodePermissionDenied
	}
	return query, args, pluginsdk.ErrCodeOK
}

// packI64 combines an error code (high 32) and a handle (low 32).
func packI64(code, handle int32) int64 {
	return int64(code)<<32 | int64(uint32(handle)) //nolint:gosec // G115: intentional ABI packing
}

// normalizeValue converts driver values into MessagePack-friendly types.
func normalizeValue(v any) any {
	switch t := v.(type) {
	case time.Time:
		return t.UTC().Format(time.RFC3339Nano)
	default:
		return v
	}
}

func (h *Host) dbRowsNext(ctx context.Context, mod api.Module, handle, outPtr, outMax int32) int32 {
	tabs := h.handlesFor(mod)
	if tabs == nil {
		return pluginsdk.ErrCodeInvalidArgument
	}
	v, ok := tabs.get(handle, handleRows)
	if !ok {
		return pluginsdk.ErrCodeNotFound
	}
	rows, ok := v.(database.Rows)
	if !ok {
		return pluginsdk.ErrCodeInternal
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return pluginsdk.ErrCodeInternal
		}
		return 0 // EOF
	}
	cols, err := rows.Columns()
	if err != nil {
		return pluginsdk.ErrCodeInternal
	}
	dest := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range dest {
		ptrs[i] = &dest[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return pluginsdk.ErrCodeInternal
	}
	for i := range dest {
		dest[i] = normalizeValue(dest[i])
	}
	raw, err := msgpack.Marshal(dest)
	if err != nil {
		return pluginsdk.ErrCodeInternal
	}
	return writeBytes(mod, outPtr, outMax, raw)
}

func (h *Host) dbRowsClose(_ context.Context, mod api.Module, handle int32) int32 {
	tabs := h.handlesFor(mod)
	if tabs == nil {
		return pluginsdk.ErrCodeInvalidArgument
	}
	v, ok := tabs.remove(handle, handleRows)
	if !ok {
		return pluginsdk.ErrCodeNotFound
	}
	if rows, ok := v.(database.Rows); ok {
		_ = rows.Close()
	}
	return pluginsdk.ErrCodeOK
}

func (h *Host) dbUnavailable() bool {
	return h.cfg.DB == nil
}

func (h *Host) dbQuery(ctx context.Context, mod api.Module, sqlPtr, sqlLen, argsPtr, argsLen int32) int64 {
	if h.dbUnavailable() {
		return int64(pluginsdk.ErrCodeUnavailable) << 32
	}
	query, args, code := checkQuery(ctx, mod, sqlPtr, sqlLen, argsPtr, argsLen, false)
	if code != pluginsdk.ErrCodeOK {
		return int64(code) << 32
	}
	release, ok := h.acquireDBSlotCtx(ctx)
	if !ok {
		return int64(pluginsdk.ErrCodeQuotaExceeded) << 32
	}
	defer release()
	return h.doQuery(ctx, mod, h.cfg.DB, query, args)
}

func (h *Host) doQuery(ctx context.Context, mod api.Module, q dbQueryer, query string, args []any) int64 {
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return int64(pluginsdk.ErrCodeInternal) << 32
	}
	tabs := h.handlesFor(mod)
	if tabs == nil {
		_ = rows.Close()
		return int64(pluginsdk.ErrCodeInvalidArgument) << 32
	}
	handle, err := tabs.add(handleRows, rows)
	if err != nil {
		_ = rows.Close()
		return int64(pluginsdk.ErrCodeUnavailable) << 32
	}
	return packI64(pluginsdk.ErrCodeOK, handle)
}

func (h *Host) dbExec(ctx context.Context, mod api.Module, sqlPtr, sqlLen, argsPtr, argsLen, outRows int32) int32 {
	if h.dbUnavailable() {
		return pluginsdk.ErrCodeUnavailable
	}
	query, args, code := checkQuery(ctx, mod, sqlPtr, sqlLen, argsPtr, argsLen, true)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	release, ok := h.acquireDBSlotCtx(ctx)
	if !ok {
		return pluginsdk.ErrCodeQuotaExceeded
	}
	defer release()
	return h.doExec(ctx, mod, h.cfg.DB, query, args, outRows)
}

func (h *Host) doExec(ctx context.Context, mod api.Module, q dbQueryer, query string, args []any, outRows int32) int32 {
	res, err := q.Exec(ctx, query, args...)
	if err != nil {
		return pluginsdk.ErrCodeInternal
	}
	if outRows == 0 {
		return pluginsdk.ErrCodeOK
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return pluginsdk.ErrCodeInternal
	}
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(affected)) //nolint:gosec // G115: intentional bit-cast
	return writeBytes(mod, outRows, 8, buf[:])
}

func (h *Host) dbTxBegin(ctx context.Context, mod api.Module) int64 {
	if h.dbUnavailable() {
		return int64(pluginsdk.ErrCodeUnavailable) << 32
	}
	if !pluginCaps(ctx).hasAnyDB() {
		return int64(pluginsdk.ErrCodePermissionDenied) << 32
	}
	// Begin pins a pool connection immediately, so it draws a statement slot
	// like Query/Exec do; the slot is released when Begin returns — the
	// resulting tx handle stays under the per-instance rows-handle budget.
	release, ok := h.acquireDBSlotCtx(ctx)
	if !ok {
		return int64(pluginsdk.ErrCodeQuotaExceeded) << 32
	}
	defer release()
	tx, err := h.cfg.DB.Begin(ctx)
	if err != nil {
		return int64(pluginsdk.ErrCodeInternal) << 32
	}
	tabs := h.handlesFor(mod)
	if tabs == nil {
		if rerr := tx.Rollback(); rerr != nil {
			h.logHandleCleanup(rerr)
		}
		return int64(pluginsdk.ErrCodeInvalidArgument) << 32
	}
	// Transaction handles share the rows budget (spec defines three budgets;
	// a tx is tracked under handleRows and rolled back on instance release).
	handle, err := tabs.add(handleRows, tx)
	if err != nil {
		if rerr := tx.Rollback(); rerr != nil {
			h.logHandleCleanup(rerr)
		}
		return int64(pluginsdk.ErrCodeUnavailable) << 32
	}
	return packI64(pluginsdk.ErrCodeOK, handle)
}

func (h *Host) txFromHandle(mod api.Module, txHandle int32) (database.Tx, int32) {
	tabs := h.handlesFor(mod)
	if tabs == nil {
		return nil, pluginsdk.ErrCodeInvalidArgument
	}
	v, ok := tabs.get(txHandle, handleRows)
	if !ok {
		return nil, pluginsdk.ErrCodeNotFound
	}
	tx, ok := v.(database.Tx)
	if !ok {
		return nil, pluginsdk.ErrCodeInvalidArgument
	}
	return tx, pluginsdk.ErrCodeOK
}

func (h *Host) dbTxCommit(_ context.Context, mod api.Module, txHandle int32) int32 {
	tabs := h.handlesFor(mod)
	if tabs == nil {
		return pluginsdk.ErrCodeInvalidArgument
	}
	v, ok := tabs.remove(txHandle, handleRows)
	if !ok {
		return pluginsdk.ErrCodeNotFound
	}
	tx, ok := v.(database.Tx)
	if !ok {
		return pluginsdk.ErrCodeInvalidArgument
	}
	if err := tx.Commit(); err != nil {
		return pluginsdk.ErrCodeInternal
	}
	return pluginsdk.ErrCodeOK
}

func (h *Host) dbTxRollback(_ context.Context, mod api.Module, txHandle int32) int32 {
	tabs := h.handlesFor(mod)
	if tabs == nil {
		return pluginsdk.ErrCodeInvalidArgument
	}
	v, ok := tabs.remove(txHandle, handleRows)
	if !ok {
		return pluginsdk.ErrCodeNotFound
	}
	tx, ok := v.(database.Tx)
	if !ok {
		return pluginsdk.ErrCodeInvalidArgument
	}
	if err := tx.Rollback(); err != nil {
		return pluginsdk.ErrCodeInternal
	}
	return pluginsdk.ErrCodeOK
}

func (h *Host) dbTxQuery(ctx context.Context, mod api.Module, txHandle, sqlPtr, sqlLen, argsPtr, argsLen int32) int64 {
	if h.dbUnavailable() {
		return int64(pluginsdk.ErrCodeUnavailable) << 32
	}
	tx, code := h.txFromHandle(mod, txHandle)
	if code != pluginsdk.ErrCodeOK {
		return int64(code) << 32
	}
	query, args, code := checkQuery(ctx, mod, sqlPtr, sqlLen, argsPtr, argsLen, false)
	if code != pluginsdk.ErrCodeOK {
		return int64(code) << 32
	}
	release, ok := h.acquireDBSlotCtx(ctx)
	if !ok {
		return int64(pluginsdk.ErrCodeQuotaExceeded) << 32
	}
	defer release()
	return h.doQuery(ctx, mod, tx, query, args)
}

func (h *Host) dbTxExec(ctx context.Context, mod api.Module, txHandle, sqlPtr, sqlLen, argsPtr, argsLen, outRows int32) int32 {
	if h.dbUnavailable() {
		return pluginsdk.ErrCodeUnavailable
	}
	tx, code := h.txFromHandle(mod, txHandle)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	query, args, code := checkQuery(ctx, mod, sqlPtr, sqlLen, argsPtr, argsLen, true)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	release, ok := h.acquireDBSlotCtx(ctx)
	if !ok {
		return pluginsdk.ErrCodeQuotaExceeded
	}
	defer release()
	return h.doExec(ctx, mod, tx, query, args, outRows)
}
