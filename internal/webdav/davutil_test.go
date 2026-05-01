package webdav

import "testing"

func TestPermissionString(t *testing.T) {
	tests := []struct {
		name        string
		perms       int
		isDir       bool
		isShareable bool
		isMounted   bool
		isShared    bool
		want        string
	}{
		{name: "owned home root", perms: PermAll, isDir: true, isShareable: true, want: "RGDNVCK"},
		{name: "owned file", perms: PermAll, isDir: false, isShareable: true, want: "RGDNVW"},
		{name: "read-only file", perms: PermRead, isDir: false, want: "G"},
		{name: "read-only dir", perms: PermRead, isDir: true, want: "G"},
		{name: "shared dir not shareable", perms: PermAll &^ PermShare, isDir: true, isShared: true, want: "SGDNVCK"},
		{name: "mounted dir", perms: PermAll, isDir: true, isShareable: true, isMounted: true, want: "RMGDNVCK"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PermissionString(tt.perms, tt.isDir, tt.isShareable, tt.isMounted, tt.isShared)
			if got != tt.want {
				t.Errorf("PermissionString = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFileID(t *testing.T) {
	tests := []struct {
		name       string
		numericID  uint64
		instanceID string
		want       string
	}{
		{name: "id 1", numericID: 1, instanceID: "oc123abc", want: "00000001oc123abc"},
		{name: "id 123", numericID: 123, instanceID: "instanceid", want: "00000123instanceid"},
		{name: "id 99999999", numericID: 99999999, instanceID: "x", want: "99999999x"},
		{name: "id 100000000 overflow keeps full digits", numericID: 100000000, instanceID: "x", want: "100000000x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FileID(tt.numericID, tt.instanceID)
			if got != tt.want {
				t.Errorf("FileID = %q, want %q", got, tt.want)
			}
		})
	}
}
