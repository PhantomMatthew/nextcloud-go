package webdav

import (
	"fmt"
	"strings"
)

const (
	PermRead    = 1
	PermUpdate  = 2
	PermCreate  = 4
	PermDelete  = 8
	PermShare   = 16
	PermAll     = PermRead | PermUpdate | PermCreate | PermDelete | PermShare
	PermDefault = PermAll
)

func PermissionString(perms int, isDir, isShareable, isMounted, isShared bool) string {
	var b strings.Builder
	if isShared {
		b.WriteByte('S')
	}
	if isShareable && perms&PermShare != 0 {
		b.WriteByte('R')
	}
	if isMounted {
		b.WriteByte('M')
	}
	if perms&PermRead != 0 {
		b.WriteByte('G')
	}
	if perms&PermDelete != 0 {
		b.WriteByte('D')
	}
	if perms&PermUpdate != 0 {
		b.WriteByte('N')
		b.WriteByte('V')
	}
	if isDir {
		if perms&PermCreate != 0 {
			b.WriteByte('C')
			b.WriteByte('K')
		}
	} else {
		if perms&PermUpdate != 0 {
			b.WriteByte('W')
		}
	}
	return b.String()
}

func FileID(numericID uint64, instanceID string) string {
	return fmt.Sprintf("%08d%s", numericID, instanceID)
}
