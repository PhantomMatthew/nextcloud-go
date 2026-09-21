package plugins

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Archive layout limits guarding against zip bombs.
const (
	maxArchiveBytes   = 128 << 20 // compressed archive size
	maxModuleBytes    = 64 << 20  // single wasm module
	maxArchiveEntries = 256
)

// SignatureFile is the archive member carrying the ed25519 signature.
const SignatureFile = "signature.sig"

var (
	ErrArchiveInvalid = errors.New("plugins: invalid archive")
	ErrArchiveTooBig  = errors.New("plugins: archive too large")
)

// Archive is a validated .ncplugin archive.
type Archive struct {
	Manifest    *Manifest
	ManifestRaw []byte
	Module      []byte
	// Files holds every non-module, non-manifest, non-signature member
	// (i18n, README, settings schema) keyed by member name.
	Files map[string][]byte
	// Signature is the raw signature.sig member, nil when unsigned.
	Signature []byte
}

// Members returns the signed member set: plugin.toml, the module, and all
// extra files. signature.sig is never part of it.
func (a *Archive) Members() map[string][]byte {
	members := make(map[string][]byte, len(a.Files)+2)
	for name, data := range a.Files {
		members[name] = data
	}
	members["plugin.toml"] = a.ManifestRaw
	members[a.Manifest.EntryPoints.Module] = a.Module
	return members
}

func readZipMember(f *zip.File, limit int64) ([]byte, error) {
	if f.UncompressedSize64 > uint64(limit) { //nolint:gosec // G115: max is positive
		return nil, fmt.Errorf("%w: %s", ErrArchiveTooBig, f.Name)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrArchiveInvalid, f.Name, err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrArchiveInvalid, f.Name, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: %s", ErrArchiveTooBig, f.Name)
	}
	return data, nil
}

// ReadArchive validates and extracts a .ncplugin archive.
func ReadArchive(raw []byte) (*Archive, error) {
	if len(raw) > maxArchiveBytes {
		return nil, fmt.Errorf("%w: archive", ErrArchiveTooBig)
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrArchiveInvalid, err)
	}
	if len(zr.File) > maxArchiveEntries {
		return nil, fmt.Errorf("%w: %d entries", ErrArchiveTooBig, len(zr.File))
	}

	var manifestRaw []byte
	a := &Archive{Files: make(map[string][]byte)}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || strings.Contains(f.Name, "..") {
			continue
		}
		switch f.Name {
		case "plugin.toml":
			manifestRaw, err = readZipMember(f, 1<<20)
		case SignatureFile:
			a.Signature, err = readZipMember(f, 1<<20)
		default:
			var data []byte
			data, err = readZipMember(f, maxModuleBytes)
			a.Files[f.Name] = data
		}
		if err != nil {
			return nil, err
		}
	}
	if manifestRaw == nil {
		return nil, fmt.Errorf("%w: missing plugin.toml", ErrArchiveInvalid)
	}
	m, err := ParseManifest(bytes.NewReader(manifestRaw))
	if err != nil {
		return nil, err
	}
	module, ok := a.Files[m.EntryPoints.Module]
	if !ok {
		return nil, fmt.Errorf("%w: missing module %s", ErrArchiveInvalid, m.EntryPoints.Module)
	}
	delete(a.Files, m.EntryPoints.Module)
	a.Manifest = m
	a.ManifestRaw = manifestRaw
	a.Module = module
	return a, nil
}

// WriteArchive packs members into a .ncplugin zip with stable ordering.
func WriteArchive(members map[string][]byte) ([]byte, error) {
	names := make([]string, 0, len(members))
	for name := range members {
		names = append(names, name)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range names {
		w, err := zw.Create(name)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrArchiveInvalid, name, err)
		}
		if _, err := w.Write(members[name]); err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrArchiveInvalid, name, err)
		}
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrArchiveInvalid, err)
	}
	return buf.Bytes(), nil
}
