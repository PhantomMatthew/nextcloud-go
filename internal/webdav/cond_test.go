package webdav

import (
	"errors"
	"slices"
	"testing"
)

func TestParseIfETags(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"plain", `(["e1"])`, []string{"e1"}},
		{"uri tagged", `</remote.php/dav/files/u/f> (["e1"] <opaquelocktoken:x>)`, []string{"e1"}},
		{"uri tagged moved-from-uploads", `</remote.php/dav/files/alice/foo.bin> (["abc123etag"])`, []string{"abc123etag"}},
		{"multiple lists", `(["e1"]) (["e2"])`, []string{"e1", "e2"}},
		{"multiple tags one list", `(["e1" "e2"])`, []string{"e1", "e2"}},
		{"not list skipped", `(Not ["e1"])`, nil},
		{"not list case-insensitive", `(not <opaquelocktoken:x> ["e1"])`, nil},
		{"weak tag stripped", `([W/"e1"])`, []string{"e1"}},
		{"lock token only", `(<opaquelocktoken:abc>)`, nil},
		{"no lists", `</remote.php/dav/files/u/f>`, nil},
		{"unterminated list", `(["e1"`, []string{"e1"}},
		{"unterminated quote", `(["e1`, nil},
		{"garbage", `((( [ ] "x"`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ParseIfETags(tc.in); !slices.Equal(got, tc.want) {
				t.Fatalf("ParseIfETags(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestWriteCondEvaluate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		cond   *WriteCond
		exists bool
		etag   string
		want   error
	}{
		{"nil receiver", nil, false, "", nil},
		{"empty cond", &WriteCond{}, true, "a", nil},
		{"if etag hit", &WriteCond{IfETags: []string{"a", "b"}}, true, "b", nil},
		{"if etag miss", &WriteCond{IfETags: []string{"a", "b"}}, true, "c", ErrPrecondition},
		{"if etag missing file", &WriteCond{IfETags: []string{"a"}}, false, "", ErrPrecondition},
		{"if-match star existing", &WriteCond{IfMatch: []string{"*"}}, true, "a", nil},
		{"if-match star missing", &WriteCond{IfMatch: []string{"*"}}, false, "", ErrPrecondition},
		{"if-match list hit", &WriteCond{IfMatch: []string{"a", "b"}}, true, "b", nil},
		{"if-match list miss", &WriteCond{IfMatch: []string{"a", "b"}}, true, "c", ErrPrecondition},
		{"if-match list missing file", &WriteCond{IfMatch: []string{"a"}}, false, "", ErrPrecondition},
		{"if-none-match star existing", &WriteCond{IfNoneMatch: []string{"*"}}, true, "a", ErrPrecondition},
		{"if-none-match star missing", &WriteCond{IfNoneMatch: []string{"*"}}, false, "", nil},
		{"if-none-match list hit", &WriteCond{IfNoneMatch: []string{"a"}}, true, "a", ErrPrecondition},
		{"if-none-match list miss", &WriteCond{IfNoneMatch: []string{"a"}}, true, "b", nil},
		{"if-none-match list missing file", &WriteCond{IfNoneMatch: []string{"a"}}, false, "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.cond.Evaluate(tc.exists, tc.etag); !errors.Is(err, tc.want) {
				t.Fatalf("Evaluate(%v, %q) = %v, want %v", tc.exists, tc.etag, err, tc.want)
			}
		})
	}
}

func TestSplitETagList(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"  ", nil},
		{`*`, []string{"*"}},
		{`"abc"`, []string{"abc"}},
		{`"abc", "def"`, []string{"abc", "def"}},
		{`W/"abc"`, []string{"abc"}},
		{`"abc", W/"def" , *`, []string{"abc", "def", "*"}},
	}
	for _, tc := range cases {
		if got := splitETagList(tc.in); !slices.Equal(got, tc.want) {
			t.Errorf("splitETagList(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
