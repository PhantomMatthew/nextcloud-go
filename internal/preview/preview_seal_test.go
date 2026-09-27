package preview

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/migrations"
	"github.com/PhantomMatthew/nextcloud-go/internal/sharing"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/encrypt"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/localfs"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

// sealFixture wires the ADR-0105 §2 stack the way app.New does in per-user
// mode: a v3-sealing encrypt FS over a raw localfs backend, the SQL
// resolver, the filecache, share hooks, and a Generator with all three
// self-sealing seams (CacheRaw, SourceKeys, Keys).
type sealFixture struct {
	db   database.DB
	dav  *files.DAV
	raw  storage.Storage
	enc  *encrypt.FS
	res  *encrypt.SQLResolver
	meta *files.SQLStore
	svc  *sharing.Service
	src  *countingSource
	gen  *Generator
}

func newSealFixture(t *testing.T, passwordWrapped bool, uids ...string) *sealFixture {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, database.Config{
		Driver: database.DialectSQLite,
		DSN:    "file:" + t.Name() + "?mode=memory&cache=shared",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	std, ok := database.Unwrap(db)
	if !ok {
		t.Fatal("unwrap")
	}
	if _, err := migrations.Up(ctx, std, database.DialectSQLite, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	us := users.NewSQLStore(db)
	for _, uid := range uids {
		u := &users.User{UID: uid, DisplayName: uid, PasswordHash: "x", Enabled: true}
		if err := us.Create(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, encrypt.MasterKeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}
	res, err := encrypt.NewSQLResolver(db, [][]byte{key})
	if err != nil {
		t.Fatal(err)
	}
	if passwordWrapped {
		res.PasswordWrapped = true
		res.KDF = encrypt.KeyDerivationParams{MemoryKB: 1024, Iterations: 1, Parallelism: 1}
	}
	enc, err := encrypt.NewWithResolver(key, nil, raw, res)
	if err != nil {
		t.Fatal(err)
	}
	meta := files.NewSQLStore(db)
	dav := files.NewDAV(enc, meta, us)
	shareStore := sharing.NewSQLShareStore(db)
	dav.Shares = shareStore
	ks := &files.KeySharer{Meta: meta, Wrapper: res, Shares: shareStore, Users: us, Logger: slog.New(slog.DiscardHandler)}
	dav.KeySharer = ks
	dav.Logger = slog.New(slog.DiscardHandler)
	svc := &sharing.Service{Store: shareStore, Files: dav, Users: us, Keys: ks, Logger: slog.New(slog.DiscardHandler)}
	dav.Incoming = files.MultiIncoming{svc}
	src := &countingSource{inner: dav}
	gen := NewGenerator(src, enc, "appdata_ocTestInstance/previews", 0, slog.New(slog.DiscardHandler))
	gen.CacheRaw = raw
	gen.SourceKeys = dav
	gen.Keys = res
	return &sealFixture{db: db, dav: dav, raw: raw, enc: enc, res: res, meta: meta, svc: svc, src: src, gen: gen}
}

// uploadAs writes as alice (the fixture's owner) with a principal ctx
// carrying an unlocked session key (nil = principal-less — fine for
// master-wrapped users) — the shape an enrolled user's client request has.
func (f *sealFixture) uploadAs(t *testing.T, unlockedKey []byte, p string, data []byte) {
	t.Helper()
	ctx := auth.WithUser(context.Background(), &auth.Principal{UID: "alice", Enabled: true, UnlockedKey: unlockedKey})
	if _, _, err := f.dav.Write(ctx, "alice", p, bytes.NewReader(data), nil); err != nil {
		t.Fatalf("upload %s: %v", p, err)
	}
}

// fileKeyUUID returns the filecache row's v3 key UUID for alice's path.
func (f *sealFixture) fileKeyUUID(t *testing.T, p string) [16]byte {
	t.Helper()
	u, err := f.users().GetByUID(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	row, err := f.meta.GetByPath(context.Background(), u.ID, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(row.KeyUUID) != 16 {
		t.Fatalf("%s key_uuid = %d bytes, want 16 (v3 write)", p, len(row.KeyUUID))
	}
	var uuid [16]byte
	copy(uuid[:], row.KeyUUID)
	return uuid
}

func (f *sealFixture) users() *users.SQLStore {
	us, ok := f.dav.Users.(*users.SQLStore)
	if !ok {
		panic("fixture users store is not SQL")
	}
	return us
}

// rowETag reads alice's etag from the filecache row directly — no content
// open, so it works for locked enrolled files too, and equals the etag
// serve computes (toEntry carries the row's ETag).
func (f *sealFixture) rowETag(t *testing.T, p string) string {
	t.Helper()
	u, err := f.users().GetByUID(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	row, err := f.meta.GetByPath(context.Background(), u.ID, p)
	if err != nil {
		t.Fatal(err)
	}
	return row.ETag
}

// encBlobPaths lists the object names under the raw previews_enc prefix.
func (f *sealFixture) encBlobPaths(t *testing.T) []string {
	t.Helper()
	infos, err := f.raw.List(context.Background(), f.gen.EncPrefix())
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(infos))
	for _, i := range infos {
		out = append(out, i.Path)
	}
	return out
}

func (f *sealFixture) legacyBlobPaths(t *testing.T) []string {
	t.Helper()
	infos, err := f.raw.List(context.Background(), f.gen.CachePrefix)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(infos))
	for _, i := range infos {
		out = append(out, i.Path)
	}
	return out
}

func readRawBlob(t *testing.T, st storage.Storage, p string) []byte {
	t.Helper()
	rc, err := st.Open(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	blob, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return blob
}

// TestSealPreviewUnit pins the NCGOPV1 construction (ADR-0105 §2): round
// trip, wrong key, tamper, variant-swap (cacheKey AD), truncation, and the
// size overhead.
func TestSealPreviewUnit(t *testing.T) {
	fk := bytes.Repeat([]byte{0x11}, 32)
	var uuid [16]byte
	copy(uuid[:], "0123456789abcdef")
	plain := []byte("rendered preview bytes")
	const key = "cache-key-fit"

	blob, err := sealPreview(fk, uuid, key, plain)
	if err != nil {
		t.Fatal(err)
	}
	if len(blob) != len(plain)+SealOverhead {
		t.Fatalf("blob = %d bytes, want plaintext + %d overhead", len(blob), SealOverhead)
	}
	if string(blob[:len(sealMagic)]) != sealMagic {
		t.Fatal("blob missing NCGOPV1 magic")
	}
	resolve := func(context.Context, [16]byte) ([]byte, error) { return fk, nil }
	got, err := openPreview(context.Background(), blob, key, resolve)
	if err != nil {
		t.Fatalf("openPreview: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatal("round trip mismatch")
	}

	wrongFK := bytes.Repeat([]byte{0x22}, 32)
	if _, err := openPreview(context.Background(), blob, key, func(context.Context, [16]byte) ([]byte, error) { return wrongFK, nil }); !errors.Is(err, encrypt.ErrIntegrity) {
		t.Errorf("wrong FK err = %v, want ErrIntegrity", err)
	}
	tampered := append([]byte(nil), blob...)
	tampered[len(tampered)-1] ^= 0xFF
	if _, err := openPreview(context.Background(), tampered, key, resolve); !errors.Is(err, encrypt.ErrIntegrity) {
		t.Errorf("tampered blob err = %v, want ErrIntegrity", err)
	}
	if _, err := openPreview(context.Background(), blob, key+"\nfill", resolve); !errors.Is(err, encrypt.ErrIntegrity) {
		t.Errorf("wrong-variant cacheKey err = %v, want ErrIntegrity", err)
	}
	if _, err := openPreview(context.Background(), blob[:SealOverhead-1], key, resolve); !errors.Is(err, encrypt.ErrIntegrity) {
		t.Errorf("truncated blob err = %v, want ErrIntegrity", err)
	}
	if _, _, _, ok := sealKeyUUID([]byte("plain legacy bytes, no magic")); ok {
		t.Error("magic-less blob parsed as sealed")
	}
	if _, err := openPreview(context.Background(), blob, key, func(context.Context, [16]byte) ([]byte, error) {
		return nil, encrypt.ErrKeyLocked
	}); !errors.Is(err, encrypt.ErrKeyLocked) {
		t.Errorf("resolve failure err = %v, want ErrKeyLocked passthrough", err)
	}
}

// TestSealedPreviewServeAndCacheHit: a v3 source renders into a
// previews_enc blob sealed under the source FK; the second request serves
// from the sealed cache without re-reading the source content.
func TestSealedPreviewServeAndCacheHit(t *testing.T) {
	fx := newSealFixture(t, false, "alice")
	fx.uploadAs(t, nil, "/photo.png", makePNG(t, 800, 600))
	uuid := fx.fileKeyUUID(t, "/photo.png")

	rr := getPreview(t, fx.gen, "alice", "file=/photo.png&x=100&y=100")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rr.Code, rr.Body.String())
	}
	body := append([]byte(nil), rr.Body.Bytes()...)
	decodeDims(t, body)

	encBlobs := fx.encBlobPaths(t)
	if len(encBlobs) != 1 {
		t.Fatalf("previews_enc blobs = %v, want exactly 1", encBlobs)
	}
	wantKey := cacheKey("alice", "/photo.png", fx.rowETag(t, "/photo.png"), 100, 100, false)
	if !strings.HasSuffix(encBlobs[0], wantKey+".png") {
		t.Errorf("enc blob path %s does not end with the expected cache key", encBlobs[0])
	}
	blob := readRawBlob(t, fx.raw, encBlobs[0])
	blobUUID, _, _, ok := sealKeyUUID(blob)
	if !ok {
		t.Fatal("enc blob lacks NCGOPV1 magic")
	}
	if blobUUID != uuid {
		t.Error("enc blob names a different key UUID than the source file")
	}
	if len(fx.legacyBlobPaths(t)) != 0 {
		t.Error("v3 source wrote into the decorated previews/ prefix")
	}

	// The blob opens only with the source FK.
	fk, err := fx.res.Resolve(context.Background(), uuid)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := openPreview(context.Background(), blob, wantKey, func(context.Context, [16]byte) ([]byte, error) { return fk, nil })
	if err != nil || !bytes.Equal(plain, body) {
		t.Fatalf("blob round trip err = %v, match = %v", err, bytes.Equal(plain, body))
	}

	// Second request: sealed cache hit, zero source content bytes.
	readBytes := fx.src.readBytes
	rr = getPreview(t, fx.gen, "alice", "file=/photo.png&x=100&y=100")
	if rr.Code != http.StatusOK || !bytes.Equal(rr.Body.Bytes(), body) {
		t.Fatalf("cache-hit status = %d, body match = %v", rr.Code, bytes.Equal(rr.Body.Bytes(), body))
	}
	if fx.src.readBytes != readBytes {
		t.Errorf("cache hit re-read %d source bytes", fx.src.readBytes-readBytes)
	}
}

// TestSealedBlobSizeCap pins the read-side cap: an over-32MiB blob is
// rejected with ErrIntegrity before any decrypt attempt.
func TestSealedBlobSizeCap(t *testing.T) {
	fx := newSealFixture(t, false, "alice")
	const key = "oversized"
	p := fx.gen.EncPrefix() + "/" + key + extPNG
	wc, err := fx.raw.Create(context.Background(), p, maxSealedBlob+1)
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 1<<20)
	for i := 0; i <= maxSealedBlob/(1<<20); i++ {
		if _, err := wc.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := wc.Close(); err != nil {
		t.Fatal(err)
	}
	if f, _, _, err := fx.gen.openCached(context.Background(), key); f != nil || !errors.Is(err, encrypt.ErrIntegrity) {
		t.Errorf("openCached = %v, %v, want nil + ErrIntegrity", f != nil, err)
	}
}

// TestSealedPreviewShareeCtxOpens pins the sharee boundary (ADR-0105 §2):
// the FK wrap created at share time resolves in the sharee's ctx, so bob's
// preview request renders and seals, and his second request opens the blob.
func TestSealedPreviewShareeCtxOpens(t *testing.T) {
	fx := newSealFixture(t, false, "alice", "bob")
	fx.uploadAs(t, nil, "/photo.png", makePNG(t, 800, 600))
	uuid := fx.fileKeyUUID(t, "/photo.png")
	if _, err := fx.svc.Create(context.Background(), "alice", "/photo.png", files.ShareTypeUser, webdav.PermRead, "bob", "", "", "", ""); err != nil {
		t.Fatal(err)
	}

	rr := getPreview(t, fx.gen, "bob", "file=/photo.png&x=64&y=64")
	if rr.Code != http.StatusOK {
		t.Fatalf("sharee status = %d, body %q", rr.Code, rr.Body.String())
	}
	blobs := fx.encBlobPaths(t)
	if len(blobs) != 1 {
		t.Fatalf("enc blobs = %v, want 1", blobs)
	}
	blobUUID, _, _, ok := sealKeyUUID(readRawBlob(t, fx.raw, blobs[0]))
	if !ok || blobUUID != uuid {
		t.Errorf("sharee preview blob key UUID = %x (ok=%v), want source FK %x", blobUUID, ok, uuid)
	}
	// Cache hit in the sharee ctx.
	readBytes := fx.src.readBytes
	if rr := getPreview(t, fx.gen, "bob", "file=/photo.png&x=64&y=64"); rr.Code != http.StatusOK {
		t.Fatalf("sharee cache-hit status = %d", rr.Code)
	}
	if fx.src.readBytes != readBytes {
		t.Error("sharee cache hit re-read source bytes")
	}
}

// TestSealedPreviewAnonymousMasterCtx pins the master-mode anonymous
// boundary at the seam: a principal-less ctx (the identity a public link
// would carry; no public preview route exists) resolves a master-wrapped FK.
func TestSealedPreviewAnonymousMasterCtx(t *testing.T) {
	fx := newSealFixture(t, false, "alice")
	fx.uploadAs(t, nil, "/photo.png", makePNG(t, 800, 600))
	if rr := getPreview(t, fx.gen, "alice", "file=/photo.png&x=32&y=32"); rr.Code != http.StatusOK {
		t.Fatalf("seed status = %d", rr.Code)
	}
	key := cacheKey("alice", "/photo.png", fx.rowETag(t, "/photo.png"), 32, 32, false)
	f, info, _, err := fx.gen.openCached(context.Background(), key)
	if err != nil || f == nil {
		t.Fatalf("anonymous openCached = %v, %v", f != nil, err)
	}
	_ = f.Close()
	if info.Size <= 0 {
		t.Errorf("plaintext size = %d", info.Size)
	}
}

// getPreviewWithKey is getPreview with a principal carrying an unlocked
// session key (nil = keyless, the app-password shape).
func getPreviewWithKey(t *testing.T, gen *Generator, uid string, unlockedKey []byte, rawQuery string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/index.php/core/preview?"+rawQuery, nil)
	req = req.WithContext(auth.WithUser(req.Context(), &auth.Principal{UID: uid, Enabled: true, UnlockedKey: unlockedKey}))
	rr := httptest.NewRecorder()
	gen.ServeHTTP(rr, req)
	return rr
}

// TestSealedPreviewEnrolledLocked pins the ADR-0101 boundary on the preview
// path: an enrolled user's keyless request fails exactly like the content
// read (404), writes nothing, and a principal-less open of a seeded blob
// fails with ErrKeyLocked; the unlocked session works.
func TestSealedPreviewEnrolledLocked(t *testing.T) {
	fx := newSealFixture(t, true, "alice")
	priv, err := fx.res.UnlockForLogin(context.Background(), "alice", "alice-pw")
	if err != nil {
		t.Fatal(err)
	}
	fx.uploadAs(t, priv, "/secret.png", makePNG(t, 400, 300))

	// Keyless ctx: the source read is locked, so the preview 404s like the
	// content read and nothing reaches either cache prefix.
	rr := getPreviewWithKey(t, fx.gen, "alice", nil, "file=/secret.png&x=32&y=32")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("locked status = %d, want 404 (content-equivalent)", rr.Code)
	}
	if n := len(fx.encBlobPaths(t)) + len(fx.legacyBlobPaths(t)); n != 0 {
		t.Errorf("locked request wrote %d cache entries", n)
	}

	// Unlocked session: renders and seals.
	rr = getPreviewWithKey(t, fx.gen, "alice", priv, "file=/secret.png&x=32&y=32")
	if rr.Code != http.StatusOK {
		t.Fatalf("unlocked status = %d, body %q", rr.Code, rr.Body.String())
	}
	if len(fx.encBlobPaths(t)) != 1 {
		t.Fatal("unlocked request did not seal into previews_enc")
	}

	// The seeded blob stays locked principal-less.
	key := cacheKey("alice", "/secret.png", fx.rowETag(t, "/secret.png"), 32, 32, false)
	if f, _, _, err := fx.gen.openCached(context.Background(), key); f != nil || !errors.Is(err, encrypt.ErrKeyLocked) {
		t.Errorf("principal-less openCached = %v, %v, want nil + ErrKeyLocked", f != nil, err)
	}
}

// TestNonV3SourceDecoratedPathBitIdentical: a source without a filecache
// key_uuid (a v1-sealed legacy write) keeps the decorated previews/ path;
// previews_enc stays empty and the cached bytes open through the decorator.
func TestNonV3SourceDecoratedPathBitIdentical(t *testing.T) {
	fx := newSealFixture(t, false, "alice")
	// A resolver-less FS over the same backend writes v1 — the pre-per-user
	// shape. Its DAV inserts the filecache row without a key_uuid.
	key := make([]byte, encrypt.MasterKeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}
	fsV1, err := encrypt.New(key, fx.raw)
	if err != nil {
		t.Fatal(err)
	}
	davV1 := files.NewDAV(fsV1, fx.meta, fx.dav.Users)
	png := makePNG(t, 200, 100)
	if _, _, err := davV1.Write(context.Background(), "alice", "/old.png", bytes.NewReader(png), nil); err != nil {
		t.Fatal(err)
	}

	rr := getPreview(t, fx.gen, "alice", "file=/old.png&x=50&y=50")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rr.Code, rr.Body.String())
	}
	if len(fx.encBlobPaths(t)) != 0 {
		t.Error("non-v3 source wrote into previews_enc")
	}
	legacy := fx.legacyBlobPaths(t)
	if len(legacy) != 1 {
		t.Fatalf("decorated blobs = %v, want 1", legacy)
	}
	// The decorated entry is decorator-sealed (v1) and opens through Cache.
	rc, err := fx.gen.Cache.Open(context.Background(), legacy[0])
	if err != nil {
		t.Fatal(err)
	}
	cached, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || !bytes.Equal(cached, rr.Body.Bytes()) {
		t.Fatalf("decorated cache round trip err = %v, match = %v", err, bytes.Equal(cached, rr.Body.Bytes()))
	}
}

// TestLegacyDecoratedEntryStillServes + the rollback pin: a legacy
// previews/ entry for a now-v3 source keeps serving, and an NCGOPV1 blob is
// invisible to a seams-less (old-binary) generator, which regenerates into
// the decorated path.
func TestLegacyDecoratedEntryStillServes(t *testing.T) {
	fx := newSealFixture(t, false, "alice")
	fx.uploadAs(t, nil, "/photo.png", makePNG(t, 800, 600))
	etag := fx.rowETag(t, "/photo.png")
	key := cacheKey("alice", "/photo.png", etag, 100, 100, false)

	// Plant a legacy entry through the decorated cache (ownerless appdata
	// key → v2/v1 master seal; reads auto-detect).
	legacy := []byte("legacy rendered preview")
	wc, err := fx.gen.Cache.Create(context.Background(), fx.gen.CachePrefix+"/"+key+extPNG, int64(len(legacy)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wc.Write(legacy); err != nil {
		t.Fatal(err)
	}
	if err := wc.Close(); err != nil {
		t.Fatal(err)
	}

	rr := getPreview(t, fx.gen, "alice", "file=/photo.png&x=100&y=100")
	if rr.Code != http.StatusOK || !bytes.Equal(rr.Body.Bytes(), legacy) {
		t.Fatalf("legacy entry status = %d, body = %q", rr.Code, rr.Body.Bytes())
	}

	// Rollback pin: seal a blob into previews_enc, then confirm a
	// seams-less generator (the old binary) never sees it — clean miss,
	// regenerate into the decorated path.
	blob, err := sealPreview(bytes.Repeat([]byte{0x33}, 32), [16]byte{}, key, []byte("sealed"))
	if err != nil {
		t.Fatal(err)
	}
	encPath := fx.gen.EncPrefix() + "/" + key + extPNG
	wc, err = fx.raw.Create(context.Background(), encPath, int64(len(blob)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wc.Write(blob); err != nil {
		t.Fatal(err)
	}
	if err := wc.Close(); err != nil {
		t.Fatal(err)
	}
	oldGen := NewGenerator(fx.src, fx.enc, fx.gen.CachePrefix, 0, slog.New(slog.DiscardHandler))
	if f, _, _, err := oldGen.openCached(context.Background(), key); f != nil || err != nil {
		// The legacy decorated entry IS visible — that's fine and pinned
		// above; what must never happen is the sealed blob surfacing. Drop
		// the legacy entry and re-check the miss.
		_ = f.Close()
	}
	if err := fx.enc.Delete(context.Background(), fx.gen.CachePrefix+"/"+key+extPNG); err != nil {
		t.Fatal(err)
	}
	f, _, _, err := oldGen.openCached(context.Background(), key)
	if f != nil || err != nil {
		t.Fatalf("old-binary openCached = %v, %v, want clean miss", f != nil, err)
	}
	if _, err := fx.raw.Stat(context.Background(), encPath); err != nil {
		t.Error("old-binary path touched the previews_enc blob")
	}
}

// TestPregenerateSealed: a resolvable (master-wrapped) v3 source
// pregenerates into previews_enc; the interactive request then hits it.
func TestPregenerateSealed(t *testing.T) {
	fx := newSealFixture(t, false, "alice")
	fx.uploadAs(t, nil, "/photo.png", makePNG(t, 800, 600))
	if err := fx.gen.Pregenerate(context.Background(), "alice", "/photo.png", []int{64}); err != nil {
		t.Fatal(err)
	}
	blobs := fx.encBlobPaths(t)
	if len(blobs) != 1 {
		t.Fatalf("pregenerated enc blobs = %v, want 1", blobs)
	}
	wantKey := cacheKey("alice", "/photo.png", fx.rowETag(t, "/photo.png"), 64, 64, false)
	if !strings.HasSuffix(blobs[0], wantKey+".png") {
		t.Errorf("pregenerated blob %s does not match the fit-box cache key", blobs[0])
	}
	readBytes := fx.src.readBytes
	if rr := getPreview(t, fx.gen, "alice", "file=/photo.png&x=64&y=64"); rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if fx.src.readBytes != readBytes {
		t.Error("pregenerated cache was not used")
	}
}

// failKeys resolves nothing, standing in for an enrolled user's FK in the
// job's principal-less ctx.
type failKeys struct{}

func (failKeys) Resolve(context.Context, [16]byte) ([]byte, error) {
	return nil, encrypt.ErrKeyLocked
}

// TestPregenerateSkipsUnresolvable pins ADR-0105 §2: a v3 source whose FK
// won't resolve in the job ctx is debug-skipped — never an error, and
// nothing is cached under either prefix.
func TestPregenerateSkipsUnresolvable(t *testing.T) {
	fx := newSealFixture(t, false, "alice")
	fx.uploadAs(t, nil, "/photo.png", makePNG(t, 800, 600))
	fx.gen.Keys = failKeys{}
	if err := fx.gen.Pregenerate(context.Background(), "alice", "/photo.png", []int{64, 128}); err != nil {
		t.Fatalf("Pregenerate = %v, want nil (skip, not error)", err)
	}
	if n := len(fx.encBlobPaths(t)) + len(fx.legacyBlobPaths(t)); n != 0 {
		t.Errorf("unresolvable pregeneration wrote %d cache entries", n)
	}
}

// TestSealedPreviewEnrolledPregenerateSkipped: the enrolled case end to
// end — the job's principal-less source read is itself locked, so
// Pregenerate quietly skips and the first unlocked interactive request pays
// the render.
func TestSealedPreviewEnrolledPregenerateSkipped(t *testing.T) {
	fx := newSealFixture(t, true, "alice")
	priv, err := fx.res.UnlockForLogin(context.Background(), "alice", "alice-pw")
	if err != nil {
		t.Fatal(err)
	}
	fx.uploadAs(t, priv, "/secret.png", makePNG(t, 400, 300))
	if err := fx.gen.Pregenerate(context.Background(), "alice", "/secret.png", []int{64}); err != nil {
		t.Fatalf("Pregenerate = %v, want nil", err)
	}
	if n := len(fx.encBlobPaths(t)) + len(fx.legacyBlobPaths(t)); n != 0 {
		t.Errorf("enrolled pregeneration wrote %d cache entries", n)
	}
	if rr := getPreviewWithKey(t, fx.gen, "alice", priv, "file=/secret.png&x=64&y=64"); rr.Code != http.StatusOK {
		t.Fatalf("interactive unlocked status = %d", rr.Code)
	}
	if len(fx.encBlobPaths(t)) != 1 {
		t.Error("interactive request did not seal into previews_enc")
	}
}
