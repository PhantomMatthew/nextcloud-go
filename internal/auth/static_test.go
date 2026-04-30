package auth

import (
	"context"
	"errors"
	"testing"
)

func TestStaticVerifier(t *testing.T) {
	v := NewStaticVerifier("admin", "secret", "Administrator")
	ctx := context.Background()

	t.Run("happy path", func(t *testing.T) {
		p, err := v.Verify(ctx, "admin", "secret")
		if err != nil {
			t.Fatalf("Verify error: %v", err)
		}
		if p.UID != "admin" || p.DisplayName != "Administrator" || !p.Enabled {
			t.Fatalf("unexpected principal: %+v", p)
		}
	})

	t.Run("wrong password", func(t *testing.T) {
		_, err := v.Verify(ctx, "admin", "wrong")
		if !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("want ErrInvalidCredentials, got %v", err)
		}
	})

	t.Run("wrong user", func(t *testing.T) {
		_, err := v.Verify(ctx, "root", "secret")
		if !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("want ErrInvalidCredentials, got %v", err)
		}
	})

	t.Run("empty credentials", func(t *testing.T) {
		_, err := v.Verify(ctx, "", "")
		if !errors.Is(err, ErrNoCredentials) {
			t.Fatalf("want ErrNoCredentials, got %v", err)
		}
	})
}
