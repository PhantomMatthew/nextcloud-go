package auth

import (
	"context"
	"errors"
)

type ChainVerifier struct {
	verifiers []Verifier
}

func NewChainVerifier(vs ...Verifier) *ChainVerifier {
	return &ChainVerifier{verifiers: vs}
}

func (c *ChainVerifier) Verify(ctx context.Context, user, password string) (*Principal, error) {
	if len(c.verifiers) == 0 {
		return nil, ErrInvalidCredentials
	}
	var lastErr error = ErrInvalidCredentials
	for _, v := range c.verifiers {
		p, err := v.Verify(ctx, user, password)
		if err == nil {
			return p, nil
		}
		if errors.Is(err, ErrNoCredentials) {
			lastErr = err
			continue
		}
		lastErr = err
	}
	return nil, lastErr
}
