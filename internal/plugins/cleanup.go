package plugins

import (
	"context"
	"errors"
	"fmt"

	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
)

// Bounds for plugin storage tree walks (deleteTree, systemTreeUsage): a
// hostile or corrupt backend must not stall uninstall or a quota check
// forever. Hitting a bound is an error, never silent truncation.
const (
	maxStorageTreeDepth   = 64
	maxStorageTreeEntries = 100_000
)

// deleteTree recursively removes the storage subtree at root using only
// List/Delete (the storage interface has no recursive delete; backends
// reject deleting non-empty directories). A missing root is not an error —
// a plugin that never wrote system storage has nothing to remove.
func deleteTree(ctx context.Context, st storage.Storage, root string) error {
	entries := 0
	var walk func(dir string, depth int) error
	walk = func(dir string, depth int) error {
		if depth > maxStorageTreeDepth {
			return fmt.Errorf("plugins: storage cleanup: depth cap (%d) at %s", maxStorageTreeDepth, dir)
		}
		infos, err := st.List(ctx, dir)
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("plugins: storage cleanup: list %s: %w", dir, err)
		}
		for _, fi := range infos {
			entries++
			if entries > maxStorageTreeEntries {
				return fmt.Errorf("plugins: storage cleanup: entry cap (%d)", maxStorageTreeEntries)
			}
			if fi.IsDir {
				if err := walk(fi.Path, depth+1); err != nil {
					return err
				}
			}
			if err := st.Delete(ctx, fi.Path); err != nil && !errors.Is(err, storage.ErrNotFound) {
				return fmt.Errorf("plugins: storage cleanup: delete %s: %w", fi.Path, err)
			}
		}
		return nil
	}
	if err := walk(root, 1); err != nil {
		return err
	}
	if err := st.Delete(ctx, root); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return fmt.Errorf("plugins: storage cleanup: delete %s: %w", root, err)
	}
	return nil
}
