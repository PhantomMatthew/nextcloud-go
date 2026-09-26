package files

import (
	"context"
	"errors"
	"testing"
)

type stubKeyWrapper struct {
	wrapped   []string
	unwrapped []string
	rewrapped [][2][16]byte
	err       error
}

func (s *stubKeyWrapper) WrapKeyFor(_ context.Context, keyUUID [16]byte, uid string) error {
	s.wrapped = append(s.wrapped, uid)
	return s.err
}

func (s *stubKeyWrapper) UnwrapKeyFor(_ context.Context, keyUUID [16]byte, uid string) error {
	s.unwrapped = append(s.unwrapped, uid)
	return s.err
}

func (s *stubKeyWrapper) ReWrapSharees(_ context.Context, oldUUID, newUUID [16]byte) error {
	s.rewrapped = append(s.rewrapped, [2][16]byte{oldUUID, newUUID})
	return s.err
}

type stubKeyShareMeta struct {
	file    *File
	subtree []File
}

func (s stubKeyShareMeta) GetByPath(context.Context, int64, string) (*File, error) {
	if s.file == nil {
		return nil, ErrNotFound
	}
	return s.file, nil
}

func (s stubKeyShareMeta) ListSealedSubtree(context.Context, int64, string) ([]File, error) {
	return s.subtree, nil
}

// stubShareLookup replays scripted Covering results in order.
type stubShareLookup struct {
	covering [][]*Share
	calls    int
	forGroup []*Share
	groupErr error
}

func (s *stubShareLookup) Covering(context.Context, int64, string) ([]*Share, error) {
	i := s.calls
	s.calls++
	if i >= len(s.covering) {
		return nil, nil
	}
	return s.covering[i], nil
}

func (s *stubShareLookup) ForGroup(context.Context, string) ([]*Share, error) {
	return s.forGroup, s.groupErr
}

type stubGroupMembers struct {
	members []string
	err     error
}

func (s stubGroupMembers) GroupMembers(context.Context, string, int) ([]string, error) {
	return s.members, s.err
}

func sealedFile(path string) *File {
	return &File{ID: 1, UserID: 7, Path: path, KeyUUID: []byte("0123456789abcdef")}
}

func TestKeySharerWrapForWriteReconcilesVanishedShare(t *testing.T) {
	w := &stubKeyWrapper{}
	share := &Share{ID: 9, OwnerUserID: 7, ShareType: ShareTypeUser, Path: "/docs", ShareWith: "bob"}
	lookup := &stubShareLookup{
		// First Covering sees the share (the wrap pass), the re-read finds
		// it gone (a concurrent unshare landed between): the wrap must be
		// reconciled away.
		covering: [][]*Share{{share}, {}},
	}
	ks := &KeySharer{Meta: stubKeyShareMeta{}, Wrapper: w, Shares: lookup, Users: stubGroupMembers{}}
	var ku [16]byte
	copy(ku[:], "0123456789abcdef")
	if err := ks.WrapForWrite(context.Background(), 7, "/docs/new.txt", ku); err != nil {
		t.Fatal(err)
	}
	if len(w.wrapped) != 1 || w.wrapped[0] != "bob" {
		t.Fatalf("wrapped = %v, want [bob]", w.wrapped)
	}
	if len(w.unwrapped) != 1 || w.unwrapped[0] != "bob" {
		t.Fatalf("unwrapped = %v, want [bob] (share vanished mid-write)", w.unwrapped)
	}
	if lookup.calls != 2 {
		t.Errorf("Covering calls = %d, want 2 (wrap pass + reconciliation)", lookup.calls)
	}
}

func TestKeySharerWrapForWriteNoSharesIsCheap(t *testing.T) {
	w := &stubKeyWrapper{}
	lookup := &stubShareLookup{covering: [][]*Share{{}}}
	ks := &KeySharer{Meta: stubKeyShareMeta{}, Wrapper: w, Shares: lookup, Users: stubGroupMembers{}}
	var ku [16]byte
	if err := ks.WrapForWrite(context.Background(), 7, "/lonely.txt", ku); err != nil {
		t.Fatal(err)
	}
	if len(w.wrapped) != 0 || lookup.calls != 1 {
		t.Errorf("wrapped = %v, Covering calls = %d, want no wraps and no re-read", w.wrapped, lookup.calls)
	}
}

func TestKeySharerWrapForShareTypes(t *testing.T) {
	w := &stubKeyWrapper{}
	meta := stubKeyShareMeta{file: sealedFile("/a.txt")}
	ks := &KeySharer{Meta: meta, Wrapper: w, Shares: &stubShareLookup{}, Users: stubGroupMembers{members: []string{"bob", "carol"}}}

	// Link and remote shares have no wrappable recipient.
	for _, typ := range []int{ShareTypeLink, ShareTypeRemote} {
		if err := ks.WrapForShare(context.Background(), &Share{ID: 1, ShareType: typ, Path: "/a.txt"}); err != nil {
			t.Fatal(err)
		}
	}
	if len(w.wrapped) != 0 {
		t.Fatalf("wrapped after link/remote = %v, want none", w.wrapped)
	}

	// Group share wraps every member.
	if err := ks.WrapForShare(context.Background(), &Share{ID: 2, ShareType: ShareTypeGroup, Path: "/a.txt", ShareWith: "g1"}); err != nil {
		t.Fatal(err)
	}
	if len(w.wrapped) != 2 || w.wrapped[0] != "bob" || w.wrapped[1] != "carol" {
		t.Fatalf("group wrapped = %v, want [bob carol]", w.wrapped)
	}
	if err := ks.UnwrapForShare(context.Background(), &Share{ID: 2, ShareType: ShareTypeGroup, Path: "/a.txt", ShareWith: "g1"}); err != nil {
		t.Fatal(err)
	}
	if len(w.unwrapped) != 2 {
		t.Fatalf("group unwrapped = %v, want 2", w.unwrapped)
	}

	// A nil share is a no-op.
	if err := ks.WrapForShare(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if err := ks.UnwrapForShare(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestKeySharerFolderShareSkipsUnsealed(t *testing.T) {
	w := &stubKeyWrapper{}
	meta := stubKeyShareMeta{
		file: &File{ID: 3, UserID: 7, Path: "/docs", IsDir: true},
		subtree: []File{
			{ID: 4, UserID: 7, Path: "/docs/a.txt", KeyUUID: []byte("0123456789abcdef")},
			{ID: 5, UserID: 7, Path: "/docs/b.txt"}, // NULL key_uuid: v1/v2
		},
	}
	ks := &KeySharer{Meta: meta, Wrapper: w, Shares: &stubShareLookup{}, Users: stubGroupMembers{}}
	sh := &Share{ID: 4, OwnerUserID: 7, ShareType: ShareTypeUser, Path: "/docs", ShareWith: "bob"}
	if err := ks.WrapForShare(context.Background(), sh); err != nil {
		t.Fatal(err)
	}
	if len(w.wrapped) != 1 || w.wrapped[0] != "bob" {
		t.Fatalf("subtree wrapped = %v, want exactly [bob] (unsealed skipped)", w.wrapped)
	}
}

func TestKeySharerMembershipAndOverwrite(t *testing.T) {
	w := &stubKeyWrapper{}
	meta := stubKeyShareMeta{file: sealedFile("/a.txt")}
	lookup := &stubShareLookup{forGroup: []*Share{{ID: 5, OwnerUserID: 7, ShareType: ShareTypeGroup, Path: "/a.txt", ShareWith: "g1"}}}
	ks := &KeySharer{Meta: meta, Wrapper: w, Shares: lookup, Users: stubGroupMembers{}}

	if err := ks.OnGroupMemberAdded(context.Background(), "g1", "dave"); err != nil {
		t.Fatal(err)
	}
	if len(w.wrapped) != 1 || w.wrapped[0] != "dave" {
		t.Fatalf("join wrapped = %v, want [dave]", w.wrapped)
	}
	if err := ks.OnGroupMemberRemoved(context.Background(), "g1", "dave"); err != nil {
		t.Fatal(err)
	}
	if len(w.unwrapped) != 1 || w.unwrapped[0] != "dave" {
		t.Fatalf("leave unwrapped = %v, want [dave]", w.unwrapped)
	}

	// Overwrite carry: zero or unchanged old UUID is a no-op; a real change
	// delegates.
	var zero, oldU, newU [16]byte
	oldU[0], newU[0] = 1, 2
	if err := ks.ReWrapForOverwrite(context.Background(), zero, newU); err != nil {
		t.Fatal(err)
	}
	if err := ks.ReWrapForOverwrite(context.Background(), newU, newU); err != nil {
		t.Fatal(err)
	}
	if len(w.rewrapped) != 0 {
		t.Fatalf("rewrapped = %v, want none for zero/same UUID", w.rewrapped)
	}
	if err := ks.ReWrapForOverwrite(context.Background(), oldU, newU); err != nil {
		t.Fatal(err)
	}
	if len(w.rewrapped) != 1 || w.rewrapped[0] != [2][16]byte{oldU, newU} {
		t.Fatalf("rewrapped = %v, want the (old,new) pair", w.rewrapped)
	}
}

func TestKeySharerErrorsPropagateForCallerToLog(t *testing.T) {
	boom := errors.New("boom")
	w := &stubKeyWrapper{err: boom}
	meta := stubKeyShareMeta{file: sealedFile("/a.txt")}
	ks := &KeySharer{Meta: meta, Wrapper: w, Shares: &stubShareLookup{}, Users: stubGroupMembers{}}
	err := ks.WrapForShare(context.Background(), &Share{ID: 6, ShareType: ShareTypeUser, Path: "/a.txt", ShareWith: "bob"})
	if !errors.Is(err, boom) {
		t.Fatalf("wrap err = %v, want the wrapper error surfaced for the caller to log", err)
	}
}
