//go:build tinygo

package pluginsdk

import (
	"github.com/vmihailenco/msgpack/v5"
)

//go:wasmimport ncgo storage_stat
func hostStorageStat(pathPtr, pathLen, outPtr, outMax int32) int32

//go:wasmimport ncgo storage_open
func hostStorageOpen(pathPtr, pathLen int32) int64

//go:wasmimport ncgo storage_create
func hostStorageCreate(pathPtr, pathLen int32, size int64) int64

//go:wasmimport ncgo storage_stream_read
func hostStorageStreamRead(handle, bufPtr, bufMax int32) int32

//go:wasmimport ncgo storage_stream_write
func hostStorageStreamWrite(handle, bufPtr, bufLen int32) int32

//go:wasmimport ncgo storage_stream_close
func hostStorageStreamClose(handle int32) int32

//go:wasmimport ncgo storage_delete
func hostStorageDelete(pathPtr, pathLen int32) int32

//go:wasmimport ncgo storage_list
func hostStorageList(pathPtr, pathLen, outPtr, outMax int32) int32

//go:wasmimport ncgo storage_rename
func hostStorageRename(srcPtr, srcLen, dstPtr, dstLen int32) int32

//go:wasmimport ncgo storage_mkdir
func hostStorageMkdir(pathPtr, pathLen int32) int32

// storageOutCap bounds stat/list responses decoded through the bindings.
const storageOutCap = 1 << 20

// StorageFileInfo mirrors the host's MessagePack entry for storage_stat and
// storage_list.
type StorageFileInfo struct {
	Path        string `msgpack:"path"`
	Size        int64  `msgpack:"size"`
	MtimeUnixMS int64  `msgpack:"mtime_unix_ms"`
	IsDir       bool   `msgpack:"is_dir"`
}

// StorageStat returns metadata for path ("user:/…", "system:/…", or bare
// for the calling user's files).
func StorageStat(path string) (*StorageFileInfo, int32) {
	pathPtr, pathLen := bytesPtr([]byte(path))
	outPtr := alloc(storageOutCap)
	n := hostStorageStat(pathPtr, pathLen, outPtr, storageOutCap)
	if n < 0 {
		return nil, n
	}
	var info StorageFileInfo
	if err := msgpack.Unmarshal(readAt(outPtr, n), &info); err != nil {
		return nil, ErrCodeInternal
	}
	return &info, ErrCodeOK
}

// StorageList returns the entries directly under path.
func StorageList(path string) ([]StorageFileInfo, int32) {
	pathPtr, pathLen := bytesPtr([]byte(path))
	outPtr := alloc(storageOutCap)
	n := hostStorageList(pathPtr, pathLen, outPtr, storageOutCap)
	if n < 0 {
		return nil, n
	}
	var infos []StorageFileInfo
	if err := msgpack.Unmarshal(readAt(outPtr, n), &infos); err != nil {
		return nil, ErrCodeInternal
	}
	return infos, ErrCodeOK
}

// StorageStream is an open storage stream (read via StorageOpen, write via
// StorageCreate). Closing a create stream commits the content.
type StorageStream struct {
	handle int32
}

// StorageOpen opens path for reading.
func StorageOpen(path string) (*StorageStream, int32) {
	pathPtr, pathLen := bytesPtr([]byte(path))
	errCode, handle := unpackI64(hostStorageOpen(pathPtr, pathLen))
	if errCode != ErrCodeOK {
		return nil, errCode
	}
	return &StorageStream{handle: handle}, ErrCodeOK
}

// StorageCreate opens path for writing; size is a hint (negative = unknown).
// Close commits the content.
func StorageCreate(path string, size int64) (*StorageStream, int32) {
	pathPtr, pathLen := bytesPtr([]byte(path))
	errCode, handle := unpackI64(hostStorageCreate(pathPtr, pathLen, size))
	if errCode != ErrCodeOK {
		return nil, errCode
	}
	return &StorageStream{handle: handle}, ErrCodeOK
}

// Read fills buf from the stream; 0 with ErrCodeOK means EOF.
func (s *StorageStream) Read(buf []byte) (int, int32) {
	if len(buf) == 0 {
		return 0, ErrCodeOK
	}
	ptr := alloc(int32(len(buf)))
	n := hostStorageStreamRead(s.handle, ptr, int32(len(buf)))
	if n < 0 {
		return 0, n
	}
	copy(buf, readAt(ptr, n))
	return int(n), ErrCodeOK
}

// Write appends buf to the stream.
func (s *StorageStream) Write(buf []byte) (int, int32) {
	ptr, length := bytesPtr(buf)
	n := hostStorageStreamWrite(s.handle, ptr, length)
	if n < 0 {
		return 0, n
	}
	return int(n), ErrCodeOK
}

// Close releases the stream; create streams commit their content.
func (s *StorageStream) Close() int32 { return hostStorageStreamClose(s.handle) }

// StorageDelete removes path (user scope moves to trash).
func StorageDelete(path string) int32 {
	pathPtr, pathLen := bytesPtr([]byte(path))
	return hostStorageDelete(pathPtr, pathLen)
}

// StorageRename moves src to dst (same scope, overwrite).
func StorageRename(src, dst string) int32 {
	srcPtr, srcLen := bytesPtr([]byte(src))
	dstPtr, dstLen := bytesPtr([]byte(dst))
	return hostStorageRename(srcPtr, srcLen, dstPtr, dstLen)
}

// StorageMkdir creates a single directory at path; parents must already
// exist.
func StorageMkdir(path string) int32 {
	pathPtr, pathLen := bytesPtr([]byte(path))
	return hostStorageMkdir(pathPtr, pathLen)
}
