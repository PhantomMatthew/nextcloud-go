package plugins

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path"
	"strings"

	"github.com/tetratelabs/wazero/api"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
	"github.com/PhantomMatthew/nextcloud-go/pkg/pluginsdk"
)

// defaultMaxSpoolBytes caps user-scope write spools (host buffering before
// the one-shot DAV commit).
const defaultMaxSpoolBytes int64 = 1 << 30

// storageEntry is the MessagePack shape returned by storage_stat and
// storage_list.
type storageEntry struct {
	Path        string `msgpack:"path"`
	Size        int64  `msgpack:"size"`
	MtimeUnixMS int64  `msgpack:"mtime_unix_ms"`
	IsDir       bool   `msgpack:"is_dir"`
}

// storageTarget is a resolved plugin storage path: the scope, the
// normalized scope-relative path, the calling user (user scope), and the
// backend paths (system scope: root is the plugin's tree root, full the
// target inside it).
type storageTarget struct {
	system bool
	path   string
	user   string
	root   string
	full   string
}

// resolveStorage parses the scheme prefix (user:/…, system:/…, bare paths
// default to user:), normalizes the remainder, and enforces the scope's
// capability grant and backend availability.
func (h *Host) resolveStorage(ctx context.Context, mod api.Module, ptr, length int32, write bool) (storageTarget, int32) {
	raw, code := readString(mod, ptr, length)
	if code != pluginsdk.ErrCodeOK {
		return storageTarget{}, code
	}
	var t storageTarget
	p := raw
	if i := strings.Index(raw, ":"); i >= 0 {
		switch raw[:i] {
		case "user":
		case "system":
			t.system = true
		default:
			return storageTarget{}, pluginsdk.ErrCodeInvalidArgument
		}
		p = raw[i+1:]
	}
	np, err := files.NormalizePath(p)
	if err != nil {
		return storageTarget{}, pluginsdk.ErrCodeInvalidArgument
	}
	t.path = np
	caps := pluginCaps(ctx)
	granted := caps.canStorageUserRead()
	if write {
		granted = caps.canStorageUserWrite()
	}
	if t.system {
		granted = caps.canStorageSystemRead()
		if write {
			granted = caps.canStorageSystemWrite()
		}
	}
	if !granted {
		return storageTarget{}, pluginsdk.ErrCodePermissionDenied
	}
	info := callFromCtx(ctx)
	if t.system {
		if h.cfg.SystemStorage == nil {
			return storageTarget{}, pluginsdk.ErrCodeUnavailable
		}
		// The capability grant above implies a non-nil plugin.
		t.root = path.Join(h.cfg.SystemPrefix, info.plugin.manifest.Plugin.ID)
		t.full = path.Join(t.root, strings.TrimPrefix(np, "/"))
		return t, pluginsdk.ErrCodeOK
	}
	if h.cfg.Files == nil {
		return storageTarget{}, pluginsdk.ErrCodeUnavailable
	}
	t.user = info.call.UserID
	if t.user == "" {
		return storageTarget{}, pluginsdk.ErrCodeUnavailable
	}
	return t, pluginsdk.ErrCodeOK
}

// mapStorageErr translates DAV/storage backend errors into ABI codes,
// mirroring abi_db.go's mapping style; unmapped errors are logged.
func (h *Host) mapStorageErr(ctx context.Context, op string, err error) int32 {
	switch {
	case errors.Is(err, webdav.ErrNotFound), errors.Is(err, storage.ErrNotFound),
		errors.Is(err, webdav.ErrParentMissing):
		return pluginsdk.ErrCodeNotFound
	case errors.Is(err, webdav.ErrExists), errors.Is(err, storage.ErrExists):
		return pluginsdk.ErrCodeAlreadyExists
	case errors.Is(err, webdav.ErrForbidden), errors.Is(err, storage.ErrInvalidPath):
		return pluginsdk.ErrCodePermissionDenied
	case errors.Is(err, webdav.ErrIsDir), errors.Is(err, storage.ErrIsDir),
		errors.Is(err, webdav.ErrNotDir), errors.Is(err, storage.ErrNotDir):
		return pluginsdk.ErrCodeInvalidArgument
	default:
		if h.logger != nil {
			h.logger.WarnContext(ctx, "plugins: storage host function failed",
				slog.String("op", op), slog.String("error", err.Error()))
		}
		return pluginsdk.ErrCodeInternal
	}
}

func entryFromDAV(e *webdav.Entry, fallback string) storageEntry {
	p := e.Path
	if p == "" {
		p = fallback
	}
	return storageEntry{Path: p, Size: e.Size, MtimeUnixMS: e.ModTime.UnixMilli(), IsDir: e.IsDir}
}

// storageWriteSpool buffers a user-scope create in a temp file; the content
// is committed through the DAV (filecache-consistent, one-shot) when the
// stream closes. Close without commit discards the spool.
type storageWriteSpool struct {
	f       *os.File
	user    string
	path    string
	plugin  *Plugin
	written int64
	closed  bool
}

// Close discards the spool: close the temp file and remove it.
func (s *storageWriteSpool) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	name := s.f.Name()
	err := s.f.Close()
	if rerr := os.Remove(name); rerr != nil && !errors.Is(rerr, os.ErrNotExist) && err == nil {
		err = rerr
	}
	return err
}

// commitSpool writes the spooled content through the DAV and always
// discards the spool. The commit runs under a fresh context built from the
// identity captured at create time (the original call context may be gone).
func (h *Host) commitSpool(ctx context.Context, s *storageWriteSpool) (code int32) {
	defer func() {
		if cerr := s.Close(); cerr != nil {
			h.logHandleCleanup(cerr)
		}
	}()
	if _, err := s.f.Seek(0, io.SeekStart); err != nil {
		return pluginsdk.ErrCodeInternal
	}
	commitCtx := WithCallContext(context.Background(), CallContext{UserID: s.user})
	if s.plugin != nil {
		commitCtx = withCall(commitCtx, s.plugin, false)
	}
	// Quota backstop against the actual spooled bytes (ADR-0061); the
	// create-time check only saw the declared size. Best-effort: usage can
	// move between this check and the DAV.Write below, which has no
	// transactional quota of its own.
	if code := h.checkUserStorageQuota(commitCtx, storageTarget{path: s.path, user: s.user}, s.written); code != pluginsdk.ErrCodeOK {
		return code
	}
	if _, _, err := h.cfg.Files.Write(commitCtx, s.user, s.path, s.f, nil); err != nil {
		return h.mapStorageErr(ctx, "stream_close", err)
	}
	return pluginsdk.ErrCodeOK
}

// systemQuotaWriter wraps a system-scope create stream, counting written
// bytes so stream_close can enforce the plugin tree quota against the
// actual (not declared) size. tree and old are the quota inputs captured at
// create time, before the backend started the write.
type systemQuotaWriter struct {
	wc      io.WriteCloser
	full    string
	tree    int64
	old     int64
	written int64
}

func (w *systemQuotaWriter) Write(p []byte) (int, error) {
	n, err := w.wc.Write(p)
	w.written += int64(n)
	return n, err
}

// Close delegates to the wrapped stream. storageStreamClose intercepts the
// wrapper (commitSystemWrite) before this is reached, so a Close here is
// the instance-cleanup path, which commits the partial content exactly like
// an unwrapped stream.
func (w *systemQuotaWriter) Close() error { return w.wc.Close() }

// commitSystemWrite closes a system-scope create stream, enforcing the
// plugin tree quota against the actual written bytes (ADR-0061). The
// refusal removes the just-committed content on a best-effort basis:
// backend creates are atomic (localfs renames on Close), so by the time the
// quota is known the write has already replaced any pre-existing target.
// Best-effort like the user scope: the tree can move between the create-time
// capture and this check.
func (h *Host) commitSystemWrite(ctx context.Context, w *systemQuotaWriter) int32 {
	if err := w.wc.Close(); err != nil {
		return h.mapStorageErr(ctx, "stream_close", err)
	}
	quota := h.cfg.PluginSystemQuotaBytes
	if w.tree-w.old+w.written > quota {
		if err := h.cfg.SystemStorage.Delete(ctx, w.full); err != nil && !errors.Is(err, storage.ErrNotFound) {
			if h.logger != nil {
				h.logger.WarnContext(ctx, "plugins: quota-refused system write cleanup failed",
					slog.String("path", w.full), slog.String("error", err.Error()))
			}
		}
		h.warnStorageQuota(ctx, "system", w.full, quota, w.tree, w.written)
		return pluginsdk.ErrCodeQuotaExceeded
	}
	return pluginsdk.ErrCodeOK
}

func (h *Host) storageStat(ctx context.Context, mod api.Module, pathPtr, pathLen, outPtr, outMax int32) int32 {
	t, code := h.resolveStorage(ctx, mod, pathPtr, pathLen, false)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	var e storageEntry
	if t.system {
		info, err := h.cfg.SystemStorage.Stat(ctx, t.full)
		if err != nil {
			return h.mapStorageErr(ctx, "stat", err)
		}
		e = storageEntry{Path: t.path, Size: info.Size, MtimeUnixMS: info.ModTime.UnixMilli(), IsDir: info.IsDir}
	} else {
		ent, err := h.cfg.Files.Stat(ctx, t.user, t.path)
		if err != nil {
			return h.mapStorageErr(ctx, "stat", err)
		}
		e = entryFromDAV(ent, t.path)
	}
	raw, err := msgpack.Marshal(e)
	if err != nil {
		return pluginsdk.ErrCodeInternal
	}
	return writeBytes(mod, outPtr, outMax, raw)
}

func (h *Host) storageOpen(ctx context.Context, mod api.Module, pathPtr, pathLen int32) int64 {
	t, code := h.resolveStorage(ctx, mod, pathPtr, pathLen, false)
	if code != pluginsdk.ErrCodeOK {
		return packI64(code, 0)
	}
	if t.path == "/" {
		return packI64(pluginsdk.ErrCodeInvalidArgument, 0)
	}
	var rc io.ReadCloser
	var err error
	if t.system {
		rc, err = h.cfg.SystemStorage.Open(ctx, t.full)
	} else {
		rc, _, err = h.cfg.Files.Read(ctx, t.user, t.path)
	}
	if err != nil {
		return packI64(h.mapStorageErr(ctx, "open", err), 0)
	}
	tabs := h.handlesFor(mod)
	if tabs == nil {
		_ = rc.Close()
		return packI64(pluginsdk.ErrCodeInvalidArgument, 0)
	}
	handle, err := tabs.add(handleStream, rc)
	if err != nil {
		_ = rc.Close()
		return packI64(pluginsdk.ErrCodeUnavailable, 0)
	}
	return packI64(pluginsdk.ErrCodeOK, handle)
}

func (h *Host) storageCreate(ctx context.Context, mod api.Module, pathPtr, pathLen int32, size int64) int64 {
	t, code := h.resolveStorage(ctx, mod, pathPtr, pathLen, true)
	if code != pluginsdk.ErrCodeOK {
		return packI64(code, 0)
	}
	if t.path == "/" {
		return packI64(pluginsdk.ErrCodeInvalidArgument, 0)
	}
	tabs := h.handlesFor(mod)
	if tabs == nil {
		return packI64(pluginsdk.ErrCodeInvalidArgument, 0)
	}
	if t.system {
		// System-scope quota (ADR-0061): capture the tree size and the
		// pre-existing target size once, refuse early when the declared size
		// alone would overflow, and carry the pair to the stream_close
		// backstop via the counting writer. A declared size <= 0 means the
		// guest does not know it; only the close-time check applies then.
		tree, old, err := systemTreeUsage(ctx, h.cfg.SystemStorage, t.root, t.full)
		if err != nil {
			return packI64(h.mapStorageErr(ctx, "quota_tree", err), 0)
		}
		if size > 0 && tree-old+size > h.cfg.PluginSystemQuotaBytes {
			h.warnStorageQuota(ctx, "system", t.path, h.cfg.PluginSystemQuotaBytes, tree, size)
			return packI64(pluginsdk.ErrCodeQuotaExceeded, 0)
		}
		wc, err := h.cfg.SystemStorage.Create(ctx, t.full, size)
		if err != nil {
			return packI64(h.mapStorageErr(ctx, "create", err), 0)
		}
		handle, err := tabs.add(handleStream, &systemQuotaWriter{wc: wc, full: t.full, tree: tree, old: old})
		if err != nil {
			_ = wc.Close()
			return packI64(pluginsdk.ErrCodeUnavailable, 0)
		}
		return packI64(pluginsdk.ErrCodeOK, handle)
	}
	// User-scope quota (ADR-0061): refuse early when the declared size alone
	// would overflow; commitSpool re-checks against the actual spooled bytes.
	if size > 0 {
		if code := h.checkUserStorageQuota(ctx, t, size); code != pluginsdk.ErrCodeOK {
			return packI64(code, 0)
		}
	}
	// User scope spools to a temp file; stream_close commits via DAV.Write.
	f, err := os.CreateTemp("", "ncgo-plugin-spool-*")
	if err != nil {
		return packI64(pluginsdk.ErrCodeInternal, 0)
	}
	spool := &storageWriteSpool{f: f, user: t.user, path: t.path, plugin: callFromCtx(ctx).plugin}
	handle, err := tabs.add(handleStream, spool)
	if err != nil {
		if cerr := spool.Close(); cerr != nil {
			h.logHandleCleanup(cerr)
		}
		return packI64(pluginsdk.ErrCodeUnavailable, 0)
	}
	return packI64(pluginsdk.ErrCodeOK, handle)
}

func (h *Host) storageStreamRead(_ context.Context, mod api.Module, handle, bufPtr, bufMax int32) int32 {
	tabs := h.handlesFor(mod)
	if tabs == nil {
		return pluginsdk.ErrCodeInvalidArgument
	}
	v, ok := tabs.get(handle, handleStream)
	if !ok {
		return pluginsdk.ErrCodeNotFound
	}
	rc, ok := v.(io.ReadCloser)
	if !ok {
		return pluginsdk.ErrCodeInvalidArgument
	}
	if bufMax < 0 || bufMax > maxPayloadArg {
		return pluginsdk.ErrCodeTooLarge
	}
	if bufMax == 0 {
		return pluginsdk.ErrCodeOK
	}
	buf := make([]byte, bufMax)
	n, err := rc.Read(buf)
	if n > 0 {
		return writeBytes(mod, bufPtr, bufMax, buf[:n])
	}
	if errors.Is(err, io.EOF) {
		return pluginsdk.ErrCodeOK // EOF
	}
	if err != nil {
		return pluginsdk.ErrCodeInternal
	}
	return pluginsdk.ErrCodeOK
}

func (h *Host) storageStreamWrite(_ context.Context, mod api.Module, handle, bufPtr, bufLen int32) int32 {
	tabs := h.handlesFor(mod)
	if tabs == nil {
		return pluginsdk.ErrCodeInvalidArgument
	}
	v, ok := tabs.get(handle, handleStream)
	if !ok {
		return pluginsdk.ErrCodeNotFound
	}
	data, code := readBytes(mod, bufPtr, bufLen)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	if spool, ok := v.(*storageWriteSpool); ok {
		if spool.written+int64(len(data)) > h.cfg.MaxSpoolBytes {
			return pluginsdk.ErrCodeTooLarge
		}
		n, err := spool.f.Write(data)
		spool.written += int64(n)
		if err != nil {
			return pluginsdk.ErrCodeInternal
		}
		return int32(n) //nolint:gosec // G115: bounded by maxPayloadArg
	}
	wc, ok := v.(io.WriteCloser)
	if !ok {
		return pluginsdk.ErrCodeInvalidArgument
	}
	n, err := wc.Write(data)
	if err != nil {
		return pluginsdk.ErrCodeInternal
	}
	return int32(n) //nolint:gosec // G115: bounded by maxPayloadArg
}

func (h *Host) storageStreamClose(ctx context.Context, mod api.Module, handle int32) int32 {
	tabs := h.handlesFor(mod)
	if tabs == nil {
		return pluginsdk.ErrCodeInvalidArgument
	}
	v, ok := tabs.remove(handle, handleStream)
	if !ok {
		return pluginsdk.ErrCodeNotFound
	}
	if spool, ok := v.(*storageWriteSpool); ok {
		return h.commitSpool(ctx, spool)
	}
	if qw, ok := v.(*systemQuotaWriter); ok {
		return h.commitSystemWrite(ctx, qw)
	}
	closer, ok := v.(io.Closer)
	if !ok {
		return pluginsdk.ErrCodeInvalidArgument
	}
	if err := closer.Close(); err != nil {
		return h.mapStorageErr(ctx, "stream_close", err)
	}
	return pluginsdk.ErrCodeOK
}

func (h *Host) storageDelete(ctx context.Context, mod api.Module, pathPtr, pathLen int32) int32 {
	t, code := h.resolveStorage(ctx, mod, pathPtr, pathLen, true)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	if t.path == "/" {
		return pluginsdk.ErrCodeInvalidArgument
	}
	var err error
	if t.system {
		err = h.cfg.SystemStorage.Delete(ctx, t.full)
	} else {
		err = h.cfg.Files.Remove(ctx, t.user, t.path) // moves to trash
	}
	if err != nil {
		return h.mapStorageErr(ctx, "delete", err)
	}
	return pluginsdk.ErrCodeOK
}

func (h *Host) storageList(ctx context.Context, mod api.Module, pathPtr, pathLen, outPtr, outMax int32) int32 {
	t, code := h.resolveStorage(ctx, mod, pathPtr, pathLen, false)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	var entries []storageEntry
	if t.system {
		infos, err := h.cfg.SystemStorage.List(ctx, t.full)
		if err != nil {
			return h.mapStorageErr(ctx, "list", err)
		}
		entries = make([]storageEntry, 0, len(infos))
		for _, info := range infos {
			entries = append(entries, storageEntry{
				Path:        path.Join(t.path, path.Base(info.Path)),
				Size:        info.Size,
				MtimeUnixMS: info.ModTime.UnixMilli(),
				IsDir:       info.IsDir,
			})
		}
	} else {
		ents, err := h.cfg.Files.List(ctx, t.user, t.path)
		if err != nil {
			return h.mapStorageErr(ctx, "list", err)
		}
		entries = make([]storageEntry, 0, len(ents))
		for _, ent := range ents {
			entries = append(entries, entryFromDAV(ent, t.path))
		}
	}
	raw, err := msgpack.Marshal(entries)
	if err != nil {
		return pluginsdk.ErrCodeInternal
	}
	return writeBytes(mod, outPtr, outMax, raw)
}

func (h *Host) storageRename(ctx context.Context, mod api.Module, srcPtr, srcLen, dstPtr, dstLen int32) int32 {
	src, code := h.resolveStorage(ctx, mod, srcPtr, srcLen, true)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	dst, code := h.resolveStorage(ctx, mod, dstPtr, dstLen, true)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	if src.system != dst.system || src.path == "/" || dst.path == "/" {
		return pluginsdk.ErrCodeInvalidArgument
	}
	var err error
	if src.system {
		err = h.cfg.SystemStorage.Rename(ctx, src.full, dst.full)
	} else {
		_, _, err = h.cfg.Files.Move(ctx, src.user, src.path, src.user, dst.path, true)
	}
	if err != nil {
		return h.mapStorageErr(ctx, "rename", err)
	}
	return pluginsdk.ErrCodeOK
}
