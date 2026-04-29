package goldentest

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
)

var ErrNotImplemented = errors.New("goldentest: not implemented")

func Load(dir string) (*Case, error) {
	if dir == "" {
		return nil, fmt.Errorf("goldentest: empty case directory")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("goldentest: resolve %q: %w", dir, err)
	}
	reqRaw, err := os.ReadFile(filepath.Join(abs, "request.http"))
	if err != nil {
		return nil, fmt.Errorf("goldentest: read request.http: %w", err)
	}
	respRaw, err := os.ReadFile(filepath.Join(abs, "response.http"))
	if err != nil {
		return nil, fmt.Errorf("goldentest: read response.http: %w", err)
	}
	if _, err := os.Stat(filepath.Join(abs, "case.yaml")); err != nil {
		return nil, fmt.Errorf("goldentest: stat case.yaml: %w", err)
	}
	return &Case{
		ID:            filepath.Base(abs),
		SchemaVersion: SchemaVersion,
		Dir:           abs,
		RequestRaw:    reqRaw,
		ResponseRaw:   respRaw,
	}, nil
}

func ParseRequest(raw []byte) (*ParsedRequest, error) {
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(raw)))
	if err != nil {
		return nil, fmt.Errorf("goldentest: parse request: %w", err)
	}
	defer req.Body.Close()
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, fmt.Errorf("goldentest: read request body: %w", err)
	}
	return &ParsedRequest{
		Method:  req.Method,
		Path:    req.URL.RequestURI(),
		Headers: req.Header.Clone(),
		Body:    body,
	}, nil
}

func ParseResponse(raw []byte) (*ParsedResponse, error) {
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(raw)), nil)
	if err != nil {
		return nil, fmt.Errorf("goldentest: parse response: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("goldentest: read response body: %w", err)
	}
	return &ParsedResponse{
		Status:  resp.StatusCode,
		Headers: resp.Header.Clone(),
		Body:    body,
	}, nil
}

func DumpResponse(resp *http.Response) ([]byte, error) {
	return httputil.DumpResponse(resp, true)
}
