//go:build !wasm

package pluginsdk

// StorageFileInfo mirrors the host's MessagePack entry for storage_stat and
// storage_list.
type StorageFileInfo struct {
	Path        string `msgpack:"path"`
	Size        int64  `msgpack:"size"`
	MtimeUnixMS int64  `msgpack:"mtime_unix_ms"`
	IsDir       bool   `msgpack:"is_dir"`
}

// StorageStat is a no-op on non-wasm builds.
func StorageStat(string) (*StorageFileInfo, int32) { return nil, ErrCodeUnsupported }

// StorageList is a no-op on non-wasm builds.
func StorageList(string) ([]StorageFileInfo, int32) { return nil, ErrCodeUnsupported }

// StorageStream is a no-op on non-wasm builds so the host can import this
// package.
type StorageStream struct{}

// StorageOpen is a no-op on non-wasm builds.
func StorageOpen(string) (*StorageStream, int32) { return nil, ErrCodeUnsupported }

// StorageCreate is a no-op on non-wasm builds.
func StorageCreate(string, int64) (*StorageStream, int32) { return nil, ErrCodeUnsupported }

// Read is a no-op on non-wasm builds.
func (*StorageStream) Read([]byte) (int, int32) { return 0, ErrCodeUnsupported }

// Write is a no-op on non-wasm builds.
func (*StorageStream) Write([]byte) (int, int32) { return 0, ErrCodeUnsupported }

// Close is a no-op on non-wasm builds.
func (*StorageStream) Close() int32 { return ErrCodeUnsupported }

// StorageDelete is a no-op on non-wasm builds.
func StorageDelete(string) int32 { return ErrCodeUnsupported }

// StorageRename is a no-op on non-wasm builds.
func StorageRename(string, string) int32 { return ErrCodeUnsupported }

// StorageMkdir is a no-op on non-wasm builds.
func StorageMkdir(string) int32 { return ErrCodeUnsupported }
