package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
)

var (
	ErrNoCredentials      = errors.New("auth: no credentials")
	ErrInvalidCredentials = errors.New("auth: invalid credentials")
)

type Verifier interface {
	Verify(ctx context.Context, user, password string) (*Principal, error)
}

func ParseBasicHeader(h string) (user, password string, ok bool) {
	const prefix = "Basic "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(h[len(prefix):]))
	if err != nil {
		return "", "", false
	}
	parts := strings.SplitN(string(raw), ":", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}
