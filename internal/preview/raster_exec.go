package preview

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// ExecRasterizer is the production Rasterizer: an external office suite's
// headless converter (LibreOffice/OpenOffice `soffice`) renders a document's
// first page to PNG. The command path comes from admin config and is exec'd
// directly — never through a shell; input travels via a 0600 temp file in a
// private per-call directory; the rendered output is size-capped before it
// re-enters the builtin pipeline's own bomb guards.
type ExecRasterizer struct {
	Command string
	Timeout time.Duration // <=0 selects defaultExecTimeout
}

const (
	defaultExecTimeout = 30 * time.Second
	// maxRasterBytes caps the converter's rendered output before the builtin
	// pipeline's dimension guards apply.
	maxRasterBytes = 64 << 20
)

// officeExtensions maps each canonical officeMime type the exec converter
// handles to the temp-file extension it sniffs best with.
var officeExtensions = map[string]string{
	mimePDF:        ".pdf",
	mimeOOXMLWord:  ".docx",
	mimeOOXMLSheet: ".xlsx",
	mimeOOXMLSlide: ".pptx",
	mimeODFText:    ".odt",
	mimeODFSheet:   ".ods",
	mimeODFSlides:  ".odp",
}

func (r *ExecRasterizer) Handles(mime string) bool {
	_, ok := officeExtensions[mime]
	return ok
}

func (r *ExecRasterizer) Rasterize(ctx context.Context, data []byte) ([]byte, error) {
	if r == nil || r.Command == "" {
		return nil, errors.New("preview: exec rasterizer without command")
	}
	mime, ok := officeMime(data)
	if !ok {
		return nil, errNotPreviewable
	}
	ext, ok := officeExtensions[mime]
	if !ok {
		return nil, errNotPreviewable
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = defaultExecTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dir, err := os.MkdirTemp("", "ncgo-office-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	in := filepath.Join(dir, "source"+ext)
	if err := os.WriteFile(in, data, 0o600); err != nil {
		return nil, err
	}
	// The command path is admin-owned config (validated as an absolute path
	// at startup), never request-derived; arguments are fixed.
	cmd := exec.CommandContext(ctx, r.Command, "--headless", "--convert-to", "png", "--outdir", dir, in) // #nosec G204 -- admin-configured converter path, fixed args
	// The converter's chatter is never useful on the wire path; a missing
	// binary, a nonzero exit, or a timeout all collapse to one error.
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("preview: office convert: %w", err)
	}
	f, err := os.Open(filepath.Join(dir, "source.png"))
	if err != nil {
		return nil, fmt.Errorf("preview: office convert produced no raster: %w", err)
	}
	defer f.Close()
	buf, err := io.ReadAll(io.LimitReader(f, maxRasterBytes+1))
	if err != nil {
		return nil, err
	}
	if len(buf) == 0 || len(buf) > maxRasterBytes {
		return nil, errNotPreviewable
	}
	return buf, nil
}
