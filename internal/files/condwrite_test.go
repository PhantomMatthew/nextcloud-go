package files

import (
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

func readBody(t *testing.T, dav *DAV, p string) string {
	t.Helper()
	rc, _, err := dav.Read(t.Context(), "alice", p)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(rc)
	if cerr := rc.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestWriteIfETagMatch(t *testing.T) {
	ctx := t.Context()
	_, dav := newUploads(t)
	if _, _, err := dav.Write(ctx, "alice", "/c.txt", strings.NewReader("v1"), nil); err != nil {
		t.Fatal(err)
	}
	st, err := dav.Stat(ctx, "alice", "/c.txt")
	if err != nil {
		t.Fatal(err)
	}
	ent, created, err := dav.WriteIf(ctx, "alice", "/c.txt", strings.NewReader("v2-longer"), nil, &webdav.WriteCond{IfETags: []string{st.ETag}})
	if err != nil || created {
		t.Fatalf("WriteIf = %+v created=%v err=%v", ent, created, err)
	}
	if got := readBody(t, dav, "/c.txt"); got != "v2-longer" {
		t.Fatalf("body = %q", got)
	}
}

func TestWriteIfETagMismatch(t *testing.T) {
	ctx := t.Context()
	_, dav := newUploads(t)
	if _, _, err := dav.Write(ctx, "alice", "/c.txt", strings.NewReader("v1"), nil); err != nil {
		t.Fatal(err)
	}
	_, _, err := dav.WriteIf(ctx, "alice", "/c.txt", strings.NewReader("v2"), nil, &webdav.WriteCond{IfETags: []string{"stale-etag"}})
	if !errors.Is(err, webdav.ErrPrecondition) {
		t.Fatalf("err = %v, want ErrPrecondition", err)
	}
	if got := readBody(t, dav, "/c.txt"); got != "v1" {
		t.Fatalf("body = %q, want untouched v1", got)
	}
}

func TestWriteIfETagMissingFile(t *testing.T) {
	ctx := t.Context()
	_, dav := newUploads(t)
	_, _, err := dav.WriteIf(ctx, "alice", "/missing.txt", strings.NewReader("x"), nil, &webdav.WriteCond{IfETags: []string{"e1"}})
	if !errors.Is(err, webdav.ErrPrecondition) {
		t.Fatalf("err = %v, want ErrPrecondition", err)
	}
	if _, err := dav.Stat(ctx, "alice", "/missing.txt"); !errors.Is(err, webdav.ErrNotFound) {
		t.Fatalf("file must not be created: %v", err)
	}
}

func TestWriteIfMatchStarAndList(t *testing.T) {
	ctx := t.Context()
	_, dav := newUploads(t)
	if _, _, err := dav.Write(ctx, "alice", "/c.txt", strings.NewReader("v1"), nil); err != nil {
		t.Fatal(err)
	}
	st, err := dav.Stat(ctx, "alice", "/c.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := dav.WriteIf(ctx, "alice", "/c.txt", strings.NewReader("v2-longer"), nil, &webdav.WriteCond{IfMatch: []string{"*"}}); err != nil {
		t.Fatalf("If-Match * on existing: %v", err)
	}
	if _, _, err := dav.WriteIf(ctx, "alice", "/c.txt", strings.NewReader("v3"), nil, &webdav.WriteCond{IfMatch: []string{"other", st.ETag}}); err == nil {
		t.Fatal("If-Match with stale list must fail (etag rotated by the * write)")
	} else if !errors.Is(err, webdav.ErrPrecondition) {
		t.Fatalf("err = %v, want ErrPrecondition", err)
	}
	st2, err := dav.Stat(ctx, "alice", "/c.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := dav.WriteIf(ctx, "alice", "/c.txt", strings.NewReader("v4"), nil, &webdav.WriteCond{IfMatch: []string{"other", st2.ETag}}); err != nil {
		t.Fatalf("If-Match list hit: %v", err)
	}
	if _, _, err := dav.WriteIf(ctx, "alice", "/new.txt", strings.NewReader("x"), nil, &webdav.WriteCond{IfMatch: []string{"*"}}); !errors.Is(err, webdav.ErrPrecondition) {
		t.Fatalf("If-Match * on missing: %v", err)
	}
}

func TestWriteIfNoneMatchStarAndList(t *testing.T) {
	ctx := t.Context()
	_, dav := newUploads(t)
	// Create-only: succeeds while the file is missing.
	if _, created, err := dav.WriteIf(ctx, "alice", "/c.txt", strings.NewReader("v1"), nil, &webdav.WriteCond{IfNoneMatch: []string{"*"}}); err != nil || !created {
		t.Fatalf("create with If-None-Match * = created=%v err=%v", created, err)
	}
	// …and fails once it exists.
	if _, _, err := dav.WriteIf(ctx, "alice", "/c.txt", strings.NewReader("v2"), nil, &webdav.WriteCond{IfNoneMatch: []string{"*"}}); !errors.Is(err, webdav.ErrPrecondition) {
		t.Fatalf("If-None-Match * on existing: %v", err)
	}
	st, err := dav.Stat(ctx, "alice", "/c.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := dav.WriteIf(ctx, "alice", "/c.txt", strings.NewReader("v3"), nil, &webdav.WriteCond{IfNoneMatch: []string{st.ETag}}); !errors.Is(err, webdav.ErrPrecondition) {
		t.Fatalf("If-None-Match list hit: %v", err)
	}
	if _, _, err := dav.WriteIf(ctx, "alice", "/c.txt", strings.NewReader("v4"), nil, &webdav.WriteCond{IfNoneMatch: []string{"different"}}); err != nil {
		t.Fatalf("If-None-Match list miss: %v", err)
	}
}

func TestWriteIfConcurrentExactlyOneWinner(t *testing.T) {
	ctx := t.Context()
	_, dav := newUploads(t)
	if _, _, err := dav.Write(ctx, "alice", "/race.bin", strings.NewReader("init!"), nil); err != nil {
		t.Fatal(err)
	}
	st, err := dav.Stat(ctx, "alice", "/race.bin")
	if err != nil {
		t.Fatal(err)
	}
	stale := st.ETag

	const n = 8
	bodies := make([]string, n)
	for i := range bodies {
		bodies[i] = strings.Repeat(string(rune('a'+i)), 10+i)
	}
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := dav.WriteIf(ctx, "alice", "/race.bin", strings.NewReader(bodies[i]), nil, &webdav.WriteCond{IfETags: []string{stale}})
			errs[i] = err
		}()
	}
	wg.Wait()

	wins, winner := 0, -1
	for i, err := range errs {
		switch {
		case err == nil:
			wins++
			winner = i
		case errors.Is(err, webdav.ErrPrecondition):
		default:
			t.Fatalf("goroutine %d err = %v", i, err)
		}
	}
	if wins != 1 {
		t.Fatalf("wins = %d, want exactly 1 (errs=%v)", wins, errs)
	}
	if got := readBody(t, dav, "/race.bin"); got != bodies[winner] {
		t.Fatalf("final body = %q, want winner %d's %q", got, winner, bodies[winner])
	}
}
