package goldentest

import (
	"fmt"
)

// Compare normalizes want and got and returns a diff error when they disagree.
func Compare(c *Case, want, got *ParsedResponse) error {
	wantNorm, err := Normalize(c, want)
	if err != nil {
		return fmt.Errorf("goldentest: normalize want: %w", err)
	}
	gotNorm, err := Normalize(c, got)
	if err != nil {
		return fmt.Errorf("goldentest: normalize got: %w", err)
	}
	if d := Diff(wantNorm, gotNorm); d != "" {
		return fmt.Errorf("goldentest: case %s mismatch:\n%s", c.ID, d)
	}
	return nil
}
