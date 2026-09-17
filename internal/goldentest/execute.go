package goldentest

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
)

// Execute parses the case request, invokes do, and returns the observed response.
func Execute(ctx context.Context, c *Case, do func(*http.Request) (*http.Response, error)) (*ParsedResponse, error) {
	if c == nil {
		return nil, fmt.Errorf("goldentest: nil case")
	}
	if do == nil {
		return nil, fmt.Errorf("goldentest: nil do")
	}
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(c.RequestRaw)))
	if err != nil {
		return nil, fmt.Errorf("goldentest: parse request: %w", err)
	}
	req = req.WithContext(ctx)
	req.RequestURI = ""
	resp, err := do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("goldentest: read response: %w", err)
	}
	return &ParsedResponse{
		Status:  resp.StatusCode,
		Headers: resp.Header.Clone(),
		Body:    body,
	}, nil
}
