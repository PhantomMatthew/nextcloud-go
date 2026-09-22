package files

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

// fakeLiveProps is a scripted webdav.LivePropProvider for the files-level
// tests (the wasm-backed provider is exercised in internal/plugins).
type fakeLiveProps struct {
	props      []webdav.CustomProp
	setHandled bool
	setStatus  int
	setUser    string
	setPath    string
	setNS      string
	setName    string
	setValue   string
}

func (f *fakeLiveProps) PropsFor(_ context.Context, _, _ string) []webdav.CustomProp {
	return f.props
}

func (f *fakeLiveProps) SetProp(_ context.Context, user, path, ns, name, value string) (bool, int) {
	f.setUser, f.setPath, f.setNS, f.setName, f.setValue = user, path, ns, name, value
	return f.setHandled, f.setStatus
}

func TestDAVLivePropsStatList(t *testing.T) {
	ctx := t.Context()
	dav := newPropsDAV(t)
	fp := &fakeLiveProps{props: []webdav.CustomProp{{
		NS: "http://ncgo.local/ns/plugin/com.example.probe", Name: "tags", Value: "blue",
	}}}
	dav.LiveProps = fp
	if _, _, err := dav.Write(ctx, "alice", "/a.txt", strings.NewReader("hello"), nil); err != nil {
		t.Fatal(err)
	}
	st, err := dav.Stat(ctx, "alice", "/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.ExtraProps) != 1 || st.ExtraProps[0].Name != "tags" || st.ExtraProps[0].Value != "blue" {
		t.Fatalf("stat extra = %+v", st.ExtraProps)
	}
	children, err := dav.List(ctx, "alice", "/")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, c := range children {
		if c.Path == "/a.txt" {
			found = true
			if len(c.ExtraProps) != 1 {
				t.Fatalf("list extra = %+v", c.ExtraProps)
			}
		}
	}
	if !found {
		t.Fatalf("children = %+v", children)
	}
	rc, re, err := dav.Read(ctx, "alice", "/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	if len(re.ExtraProps) != 1 {
		t.Fatalf("read extra = %+v", re.ExtraProps)
	}
}

func TestDAVPatchPropsLiveProps(t *testing.T) {
	ctx := t.Context()
	dav := newPropsDAV(t)
	fp := &fakeLiveProps{setHandled: true, setStatus: http.StatusOK}
	dav.LiveProps = fp
	if _, _, err := dav.Write(ctx, "alice", "/a.txt", strings.NewReader("hello"), nil); err != nil {
		t.Fatal(err)
	}
	ns := "http://ncgo.local/ns/plugin/com.example.probe"
	results, err := dav.PatchProps(ctx, "alice", "/a.txt", []webdav.PropPatchOp{{
		Space: ns, Name: "tags", Value: "blue",
	}})
	if err != nil || len(results) != 1 || results[0].Status != http.StatusOK {
		t.Fatalf("patch = %+v %v", results, err)
	}
	if fp.setUser != "alice" || fp.setPath != "/a.txt" || fp.setNS != ns || fp.setName != "tags" || fp.setValue != "blue" {
		t.Fatalf("set = %+v", fp)
	}

	// A remove op reaches the provider as an empty value.
	results, err = dav.PatchProps(ctx, "alice", "/a.txt", []webdav.PropPatchOp{{
		Remove: true, Space: ns, Name: "tags",
	}})
	if err != nil || len(results) != 1 || results[0].Status != http.StatusOK {
		t.Fatalf("remove = %+v %v", results, err)
	}
	if fp.setValue != "" {
		t.Fatalf("remove value = %q", fp.setValue)
	}

	// Not handled: the favorite logic still applies.
	fp.setHandled = false
	results, err = dav.PatchProps(ctx, "alice", "/a.txt", []webdav.PropPatchOp{{
		Space: PropNSOwnCloud, Name: PropFavorite, Value: "1",
	}})
	if err != nil || len(results) != 1 || results[0].Status != http.StatusOK {
		t.Fatalf("favorite = %+v %v", results, err)
	}
	st, err := dav.Stat(ctx, "alice", "/a.txt")
	if err != nil || st.Favorite != 1 {
		t.Fatalf("stat favorite = %+v err=%v", st, err)
	}

	// Read-only live prop: the provider's 403 is reported.
	fp.setHandled = true
	fp.setStatus = http.StatusForbidden
	results, err = dav.PatchProps(ctx, "alice", "/a.txt", []webdav.PropPatchOp{{
		Space: ns, Name: "tags", Value: "red",
	}})
	if err != nil || len(results) != 1 || results[0].Status != http.StatusForbidden {
		t.Fatalf("read-only = %+v %v", results, err)
	}
}
