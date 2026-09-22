package cache

import (
	"context"
	"errors"
	"testing"
)

func TestRedisMatchPattern(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"plain", "plain"},
		{"plugin:com.example:", "plugin:com.example:"},
		{"*", `\*`},
		{`a*b?c[d]\e`, `a\*b\?c\[d\]\\e`},
		{"100%", "100%"},
	}
	for _, tc := range cases {
		if got := redisMatchPattern(tc.in); got != tc.want {
			t.Errorf("redisMatchPattern(%q) = %q want %q", tc.in, got, tc.want)
		}
	}
}

// TestRedisDeleteByPrefixEmpty covers the guard before any Redis I/O, so it
// runs without a server.
func TestRedisDeleteByPrefixEmpty(t *testing.T) {
	r := &Redis{}
	if _, err := r.DeleteByPrefix(context.Background(), ""); !errors.Is(err, ErrEmptyPrefix) {
		t.Fatalf("empty prefix = %v", err)
	}
}
