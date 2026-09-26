package encrypt

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/curve25519"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/database"
)

// fastKDF keeps the login-path argon2id cheap in tests.
var fastKDF = KeyDerivationParams{MemoryKB: 1024, Iterations: 1, Parallelism: 1}

// pwResolver builds a single-key resolver over db with the given
// password-wrapped flag and fast test KDF.
func pwResolver(t *testing.T, db database.DB, wrapped bool, keys ...[]byte) *SQLResolver {
	t.Helper()
	if len(keys) == 0 {
		keys = [][]byte{testKey(t)}
	}
	res, err := NewSQLResolver(db, keys)
	if err != nil {
		t.Fatal(err)
	}
	res.PasswordWrapped = wrapped
	res.KDF = fastKDF
	return res
}

// identityCtx carries uid's principal with the given unlocked key (nil for
// "authenticated but keyless").
func identityCtx(uid string, unlockedKey []byte) context.Context {
	return auth.WithUser(context.Background(), &auth.Principal{UID: uid, Enabled: true, UnlockedKey: unlockedKey})
}

func rowCount(t *testing.T, db database.DB, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// enrollCalls pins the enrollment state machine at password login
// (ADR-0100 §2): a user with a symmetric UK and v3 wraps converts every wrap
// to a scheme=1 box, loses the master-sealed UK, and resolves only through
// an identity ctx afterwards.
func TestUnlockForLoginEnrollsAtPasswordLogin(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	aliceID := seedResolverUser(t, db, "alice")
	fs, _, res := sqlResolverFS(t, db, testKey(t))
	res.PasswordWrapped = true
	res.KDF = fastKDF

	uuidA := writeV3(t, fs, "alice/a.txt", []byte("alpha"))
	uuidB := writeV3(t, fs, "alice/b.txt", []byte("beta"))
	// Pre-enrollment FKs, resolved the phase 1–3 way.
	fkA, err := res.Resolve(ctx, uuidA)
	if err != nil {
		t.Fatal(err)
	}
	fkB, err := res.Resolve(ctx, uuidB)
	if err != nil {
		t.Fatal(err)
	}

	priv, err := res.UnlockForLogin(ctx, "alice", "wonderland")
	if err != nil {
		t.Fatal(err)
	}
	if len(priv) != x25519KeySize {
		t.Fatalf("unlocked key = %d bytes, want 32", len(priv))
	}
	if n := rowCount(t, db, `SELECT COUNT(*) FROM user_keys WHERE user_id = ?`, aliceID); n != 0 {
		t.Errorf("user_keys rows after enrollment = %d, want 0 (the threat-model pivot)", n)
	}
	if n := rowCount(t, db, `SELECT COUNT(*) FROM user_key_pw WHERE user_id = ?`, aliceID); n != 1 {
		t.Errorf("user_key_pw rows = %d, want 1", n)
	}
	if n := rowCount(t, db, `SELECT COUNT(*) FROM file_keys WHERE user_id = ? AND scheme = 1`, aliceID); n != 2 {
		t.Errorf("scheme=1 rows = %d, want 2 (both wraps converted)", n)
	}
	// The row's public key matches the returned private key.
	var pub, sealed, salt []byte
	var kdf string
	if err := db.QueryRow(ctx, `
SELECT public_key, pw_sealed_uk, pw_kdf, pw_salt FROM user_key_pw WHERE user_id = ?`, aliceID).
		Scan(&pub, &sealed, &kdf, &salt); err != nil {
		t.Fatal(err)
	}
	if kdf != fastKDF.marshalKDF() {
		t.Errorf("pw_kdf = %q, want %q", kdf, fastKDF.marshalKDF())
	}
	wantPub, err := curve25519.X25519(priv, curve25519.Basepoint)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pub, wantPub) {
		t.Error("user_key_pw public key does not match the unlocked private key")
	}
	if len(sealed) != pwSealedUKSize {
		t.Errorf("pw_sealed_uk = %d bytes, want %d", len(sealed), pwSealedUKSize)
	}

	// Post-enrollment: identity ctx resolves the same FKs; no ctx locks.
	got, err := res.Resolve(identityCtx("alice", priv), uuidA)
	if err != nil {
		t.Fatalf("resolve with identity: %v", err)
	}
	if !bytes.Equal(got, fkA) {
		t.Error("FK of a.txt changed across enrollment")
	}
	got, err = res.Resolve(identityCtx("alice", priv), uuidB)
	if err != nil {
		t.Fatalf("resolve with identity: %v", err)
	}
	if !bytes.Equal(got, fkB) {
		t.Error("FK of b.txt changed across enrollment")
	}
	if _, err := res.Resolve(ctx, uuidA); !errors.Is(err, ErrKeyLocked) {
		t.Errorf("resolve without identity err = %v, want ErrKeyLocked", err)
	}

	// Re-login (already enrolled): returns the same private key, changes
	// nothing.
	priv2, err := res.UnlockForLogin(ctx, "alice", "wonderland")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(priv, priv2) {
		t.Error("re-login returned a different private key")
	}
	if n := rowCount(t, db, `SELECT COUNT(*) FROM file_keys WHERE user_id = ?`, aliceID); n != 2 {
		t.Errorf("wrap rows after re-login = %d, want 2", n)
	}

	// Wrong password: loud descriptive error (never ErrKeyLocked), and the
	// enrollment is untouched.
	if _, err := res.UnlockForLogin(ctx, "alice", "wrong"); err == nil ||
		!strings.Contains(err.Error(), "password unwrap failed") {
		t.Fatalf("wrong password err = %v", err)
	} else if errors.Is(err, ErrKeyLocked) {
		t.Error("wrong password must not surface as ErrKeyLocked")
	}
	if n := rowCount(t, db, `SELECT COUNT(*) FROM user_key_pw WHERE user_id = ?`, aliceID); n != 1 {
		t.Error("wrong-password attempt destroyed the enrollment")
	}
	if n := rowCount(t, db, `SELECT COUNT(*) FROM file_keys WHERE user_id = ? AND scheme = 1`, aliceID); n != 2 {
		t.Error("wrong-password attempt touched the wrap rows")
	}
}

// TestUnlockForLoginUnenrollsWhenFlagOff pins the mirror: an enrolled user
// logging in with the flag off returns to the master-wrapped hierarchy.
func TestUnlockForLoginUnenrollsWhenFlagOff(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	aliceID := seedResolverUser(t, db, "alice")
	key := testKey(t)
	fs, _, res := sqlResolverFS(t, db, key)
	res.PasswordWrapped = true
	res.KDF = fastKDF

	uuid := writeV3(t, fs, "alice/a.txt", []byte("alpha"))
	fk, err := res.Resolve(ctx, uuid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := res.UnlockForLogin(ctx, "alice", "wonderland"); err != nil {
		t.Fatal(err)
	}

	// Flag off (a fresh resolver over the same db, as after a restart).
	resOff := pwResolver(t, db, false, key)
	back, err := resOff.UnlockForLogin(ctx, "alice", "wonderland")
	if err != nil {
		t.Fatal(err)
	}
	if back != nil {
		t.Errorf("unenroll returned key material (%d bytes), want nil (master path serves the user)", len(back))
	}
	if n := rowCount(t, db, `SELECT COUNT(*) FROM user_key_pw WHERE user_id = ?`, aliceID); n != 0 {
		t.Errorf("user_key_pw rows after unenroll = %d, want 0", n)
	}
	if n := rowCount(t, db, `SELECT COUNT(*) FROM user_keys WHERE user_id = ?`, aliceID); n != 1 {
		t.Errorf("user_keys rows after unenroll = %d, want 1 (UK re-minted)", n)
	}
	if n := rowCount(t, db, `SELECT COUNT(*) FROM file_keys WHERE user_id = ? AND scheme = 0`, aliceID); n != 1 {
		t.Errorf("scheme=0 rows after unenroll = %d, want 1", n)
	}
	// The master path resolves the same FK again — with no identity.
	got, err := resOff.Resolve(ctx, uuid)
	if err != nil {
		t.Fatalf("master-path resolve after unenroll: %v", err)
	}
	if !bytes.Equal(got, fk) {
		t.Error("FK changed across the enroll/unenroll round trip")
	}

	// A never-enrolled user with the flag off is a no-op.
	seedResolverUser(t, db, "bob")
	if v, err := resOff.UnlockForLogin(ctx, "bob", "hunter2"); err != nil || v != nil {
		t.Errorf("unenrolled+flag-off unlock = %v %v, want both nil", v, err)
	}

	// Wrong password on the unenroll path fails loudly too and keeps the
	// enrollment.
	resOn := pwResolver(t, db, true, key)
	if _, err := resOn.UnlockForLogin(ctx, "alice", "wonderland"); err != nil {
		t.Fatal(err) // re-enroll for the negative check
	}
	if _, err := resOff.UnlockForLogin(ctx, "alice", "wrong"); err == nil {
		t.Fatal("wrong password on unenroll path must fail")
	}
	if n := rowCount(t, db, `SELECT COUNT(*) FROM user_key_pw WHERE user_id = ?`, aliceID); n != 1 {
		t.Error("failed unenroll destroyed the enrollment")
	}
}

// TestUnlockForLoginResumesInterruptedEnrollment fabricates the post-crash
// state — user_key_pw AND user_keys present, wraps half-converted — and
// pins the resume in both flag directions (ADR-0100 §2's restartable pass).
func TestUnlockForLoginResumesCrashStates(t *testing.T) {
	ctx := context.Background()

	fabricate := func(t *testing.T) (database.DB, []byte, int64, [16]byte, [16]byte, []byte, []byte) {
		t.Helper()
		db := resolverDB(t)
		aliceID := seedResolverUser(t, db, "alice")
		key := testKey(t)
		fs, _, res := sqlResolverFS(t, db, key)
		res.PasswordWrapped = true
		res.KDF = fastKDF
		uuidA := writeV3(t, fs, "alice/a.txt", []byte("alpha"))
		uuidB := writeV3(t, fs, "alice/b.txt", []byte("beta"))
		fkA, err := res.Resolve(ctx, uuidA)
		if err != nil {
			t.Fatal(err)
		}
		fkB, err := res.Resolve(ctx, uuidB)
		if err != nil {
			t.Fatal(err)
		}
		// Enroll, then put the crash state back: user_keys restored with a
		// fresh UK, and b.txt's wrap flipped back to scheme=0 under it —
		// both key rows present with a mixed-scheme wrap set.
		if _, err := res.UnlockForLogin(ctx, "alice", "wonderland"); err != nil {
			t.Fatal(err)
		}
		var pub []byte
		if err := db.QueryRow(ctx, `SELECT public_key FROM user_key_pw WHERE user_id = ?`, aliceID).Scan(&pub); err != nil {
			t.Fatal(err)
		}
		resSym := pwResolver(t, db, false, key)
		if err := resSym.OnUserCreated(ctx, "alice"); err != nil {
			t.Fatal(err) // re-mints the UK row (hasUK now true)
		}
		uk, found, err := resSym.loadUK(ctx, aliceID)
		if err != nil || !found {
			t.Fatalf("reload re-minted UK: %v %v", found, err)
		}
		wrappedB, err := wrapSeal(uk, fkB, fkAD(uuidB, aliceID))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, `
UPDATE file_keys SET wrapped_fk = ?, scheme = 0 WHERE key_uuid = ? AND user_id = ?`, wrappedB, uuidB[:], aliceID); err != nil {
			t.Fatal(err)
		}
		// a.txt stays a scheme=1 box under the enrolled public key —
		// re-wrap it from fkA for exactness.
		boxedA, err := boxWrap(pub, fkA, uuidA, aliceID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, `
UPDATE file_keys SET wrapped_fk = ?, scheme = 1 WHERE key_uuid = ? AND user_id = ?`, boxedA, uuidA[:], aliceID); err != nil {
			t.Fatal(err)
		}
		return db, key, aliceID, uuidA, uuidB, fkA, fkB
	}

	t.Run("flag on finishes enrollment", func(t *testing.T) {
		db, key, aliceID, uuidA, uuidB, fkA, fkB := fabricate(t)
		res := pwResolver(t, db, true, key)
		priv, err := res.UnlockForLogin(ctx, "alice", "wonderland")
		if err != nil {
			t.Fatal(err)
		}
		if len(priv) != x25519KeySize {
			t.Fatalf("priv = %d bytes", len(priv))
		}
		if n := rowCount(t, db, `SELECT COUNT(*) FROM user_keys WHERE user_id = ?`, aliceID); n != 0 {
			t.Errorf("user_keys rows = %d, want 0 after resume", n)
		}
		if n := rowCount(t, db, `SELECT COUNT(*) FROM file_keys WHERE user_id = ? AND scheme = 1`, aliceID); n != 2 {
			t.Errorf("scheme=1 rows = %d, want 2 after resume", n)
		}
		for uuid, fk := range map[[16]byte][]byte{uuidA: fkA, uuidB: fkB} {
			got, err := res.Resolve(identityCtx("alice", priv), uuid)
			if err != nil {
				t.Fatalf("resolve after resume: %v", err)
			}
			if !bytes.Equal(got, fk) {
				t.Errorf("FK of %x changed across resume", uuid[:4])
			}
		}
	})

	t.Run("flag off finishes unenrollment", func(t *testing.T) {
		db, key, aliceID, uuidA, uuidB, fkA, fkB := fabricate(t)
		res := pwResolver(t, db, false, key)
		back, err := res.UnlockForLogin(ctx, "alice", "wonderland")
		if err != nil {
			t.Fatal(err)
		}
		if back != nil {
			t.Errorf("unenroll resume returned key material, want nil")
		}
		if n := rowCount(t, db, `SELECT COUNT(*) FROM user_key_pw WHERE user_id = ?`, aliceID); n != 0 {
			t.Errorf("user_key_pw rows = %d, want 0 after resume", n)
		}
		if n := rowCount(t, db, `SELECT COUNT(*) FROM file_keys WHERE user_id = ? AND scheme = 0`, aliceID); n != 2 {
			t.Errorf("scheme=0 rows = %d, want 2 after resume", n)
		}
		for uuid, fk := range map[[16]byte][]byte{uuidA: fkA, uuidB: fkB} {
			got, err := res.Resolve(ctx, uuid)
			if err != nil {
				t.Fatalf("master resolve after resume: %v", err)
			}
			if !bytes.Equal(got, fk) {
				t.Errorf("FK of %x changed across unenroll resume", uuid[:4])
			}
		}
	})
}

// TestEnrolledAndDestroyEnrollment pins the CLI-facing enrollment queries.
func TestEnrolledAndDestroyEnrollment(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	aliceID := seedResolverUser(t, db, "alice")
	bobID := seedResolverUser(t, db, "bob")
	fs, _, res := sqlResolverFS(t, db, testKey(t))
	res.PasswordWrapped = true
	res.KDF = fastKDF

	if enrolled, err := res.Enrolled(ctx, "alice"); err != nil || enrolled {
		t.Fatalf("Enrolled before enrollment = %v %v", enrolled, err)
	}
	uuid := writeV3(t, fs, "alice/a.txt", []byte("alpha"))
	if _, err := res.UnlockForLogin(ctx, "alice", "wonderland"); err != nil {
		t.Fatal(err)
	}
	if enrolled, err := res.Enrolled(ctx, "alice"); err != nil || !enrolled {
		t.Fatalf("Enrolled after enrollment = %v %v", enrolled, err)
	}
	// bob holds a recipient wrap of alice's file; destruction must not
	// touch it.
	if _, err := res.UnlockForLogin(ctx, "bob", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if err := res.WrapKeyFor(identityCtx("alice", mustLogin(t, res, "alice", "wonderland")), uuid, "bob"); err != nil {
		t.Fatal(err)
	}
	if n := rowCount(t, db, `SELECT COUNT(*) FROM file_keys WHERE user_id = ?`, bobID); n != 1 {
		t.Fatalf("bob wrap rows = %d, want 1", n)
	}

	if err := res.DestroyEnrollment(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if enrolled, err := res.Enrolled(ctx, "alice"); err != nil || enrolled {
		t.Fatalf("Enrolled after destroy = %v %v", enrolled, err)
	}
	if n := rowCount(t, db, `SELECT COUNT(*) FROM file_keys WHERE user_id = ?`, aliceID); n != 0 {
		t.Errorf("alice wrap rows after destroy = %d, want 0", n)
	}
	if n := rowCount(t, db, `SELECT COUNT(*) FROM file_keys WHERE user_id = ?`, bobID); n != 1 {
		t.Errorf("bob wrap rows after alice's destroy = %d, want 1 (untouched)", n)
	}
	// Unknown uid: no-op for DestroyEnrollment, false for Enrolled.
	if err := res.DestroyEnrollment(ctx, "ghost"); err != nil {
		t.Errorf("destroy unknown uid: %v", err)
	}
	if enrolled, err := res.Enrolled(ctx, "ghost"); err != nil || enrolled {
		t.Errorf("Enrolled(ghost) = %v %v", enrolled, err)
	}
}

func mustLogin(t *testing.T, res *SQLResolver, uid, password string) []byte {
	t.Helper()
	priv, err := res.UnlockForLogin(context.Background(), uid, password)
	if err != nil {
		t.Fatal(err)
	}
	if len(priv) == 0 {
		t.Fatalf("%s did not unlock", uid)
	}
	return priv
}

// enrolledFixture builds two enrolled users (alice the owner, bob a reader),
// one unenrolled reader (carol), and a file of alice's wrapped for all
// three, returning everything the Resolve matrix needs.
func enrolledFixture(t *testing.T) (res *SQLResolver, uuid [16]byte, fk, alicePriv, bobPriv []byte) {
	t.Helper()
	ctx := context.Background()
	db := resolverDB(t)
	seedResolverUser(t, db, "alice")
	seedResolverUser(t, db, "bob")
	seedResolverUser(t, db, "carol")
	seedResolverUser(t, db, "dave")
	fs, _, r := sqlResolverFS(t, db, testKey(t))
	r.PasswordWrapped = true
	r.KDF = fastKDF

	uuid = writeV3(t, fs, "alice/shared.txt", []byte("shared content"))
	fk, err := r.Resolve(ctx, uuid)
	if err != nil {
		t.Fatal(err)
	}
	alicePriv = mustLogin(t, r, "alice", "alice-pw")
	bobPriv = mustLogin(t, r, "bob", "bob-pw")
	// carol gets a UK (unenrolled reader) via the lifecycle mint.
	if err := r.OnUserCreated(ctx, "carol"); err != nil {
		t.Fatal(err)
	}
	// dave enrolls too (an enrolled NON-recipient in the matrix).
	mustLogin(t, r, "dave", "dave-pw")
	// Wrap for bob (enrolled → box) and carol (unenrolled → symmetric) in
	// alice's authorized ctx.
	if err := r.WrapKeyFor(identityCtx("alice", alicePriv), uuid, "bob"); err != nil {
		t.Fatal(err)
	}
	if err := r.WrapKeyFor(identityCtx("alice", alicePriv), uuid, "carol"); err != nil {
		t.Fatal(err)
	}
	return r, uuid, fk, alicePriv, bobPriv
}

// TestResolveIdentityMatrix pins the ADR-0100 §5 read path: an enrolled
// owner's file key resolves through the READING user's own wrap row, per
// the reader's scheme, and every lock outcome is ErrKeyLocked — never
// ErrIntegrity or ErrUnresolvableKey.
func TestResolveIdentityMatrix(t *testing.T) {
	ctx := context.Background()
	res, uuid, fk, alicePriv, bobPriv := enrolledFixture(t)

	// Schemes landed as designed: owner + bob boxed, carol symmetric.
	schemes := map[string]int64{}
	rows, err := res.db.Query(ctx, `
SELECT u.uid, k.scheme FROM file_keys k JOIN users u ON u.id = k.user_id WHERE k.key_uuid = ? ORDER BY u.uid`, uuid[:])
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var uid string
		var scheme int64
		if err := rows.Scan(&uid, &scheme); err != nil {
			t.Fatal(err)
		}
		schemes[uid] = scheme
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if schemes["alice"] != 1 || schemes["bob"] != 1 || schemes["carol"] != 0 {
		t.Fatalf("schemes = %v, want alice=1 bob=1 carol=0", schemes)
	}

	cases := []struct {
		name string
		ctx  context.Context
		want []byte // nil → expect ErrKeyLocked
	}{
		{"owner with session", identityCtx("alice", alicePriv), fk},
		{"enrolled reader with session", identityCtx("bob", bobPriv), fk},
		// The unenrolled reader's scheme=0 row opens via the master path —
		// the principal carries no key and needs none.
		{"unenrolled reader, no key on ctx", identityCtx("carol", nil), fk},
		{"no principal", ctx, nil},
		{"enrolled reader without key", identityCtx("bob", nil), nil},
		{"enrolled non-recipient with key", identityCtx("dave", bytes.Repeat([]byte{1}, 32)), nil},
		{"unknown principal", identityCtx("ghost", nil), nil},
	}
	for _, tc := range cases {
		got, err := res.Resolve(tc.ctx, uuid)
		if tc.want == nil {
			if !errors.Is(err, ErrKeyLocked) {
				t.Errorf("%s: err = %v, want ErrKeyLocked", tc.name, err)
			}
			if errors.Is(err, ErrIntegrity) || errors.Is(err, ErrUnresolvableKey) {
				t.Errorf("%s: lock case conflated with corruption/config: %v", tc.name, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if !bytes.Equal(got, tc.want) {
			t.Errorf("%s: FK mismatch", tc.name)
		}
	}
}

// TestAllocateForEnrolledOwner pins the write side (ADR-0100 §5): a write
// into an enrolled user's tree boxes the fresh FK with their public key —
// no ctx, no session — and the owner resolves it with their session key.
func TestAllocateForEnrolledOwner(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	seedResolverUser(t, db, "alice")
	fs, _, res := sqlResolverFS(t, db, testKey(t))
	res.PasswordWrapped = true
	res.KDF = fastKDF
	priv := mustLogin(t, res, "alice", "alice-pw")

	// Zero-value ctx (background): Allocate must not need a session.
	uuid, fk, err := res.Allocate(ctx, "alice/new.txt")
	if err != nil {
		t.Fatal(err)
	}
	var scheme int64
	var blob []byte
	if err := db.QueryRow(ctx, `
SELECT scheme, wrapped_fk FROM file_keys WHERE key_uuid = ? AND user_id = (SELECT id FROM users WHERE uid = 'alice')`,
		uuid[:]).Scan(&scheme, &blob); err != nil {
		t.Fatal(err)
	}
	if scheme != 1 {
		t.Errorf("scheme = %d, want 1 (box for the enrolled owner)", scheme)
	}
	if len(blob) != boxWrapSize {
		t.Errorf("box wrap = %d bytes, want %d", len(blob), boxWrapSize)
	}
	got, err := res.Resolve(identityCtx("alice", priv), uuid)
	if err != nil {
		t.Fatalf("owner resolve: %v", err)
	}
	if !bytes.Equal(got, fk) {
		t.Error("owner-resolved FK differs from the allocated one")
	}
	if _, err := res.Resolve(ctx, uuid); !errors.Is(err, ErrKeyLocked) {
		t.Errorf("principal-less resolve err = %v, want ErrKeyLocked", err)
	}

	// End-to-end through the FS: Create seals without a principal; Open
	// reads with the owner's identity and locks without one.
	wc, err := fs.Create(ctx, "alice/e2e.txt", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wc.Write([]byte("enrolled content")); err != nil {
		t.Fatal(err)
	}
	if err := wc.Close(); err != nil {
		t.Fatal(err)
	}
	rc, err := fs.Open(identityCtx("alice", priv), "alice/e2e.txt")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	if string(data) != "enrolled content" {
		t.Errorf("read = %q", data)
	}
	if _, err := fs.Open(ctx, "alice/e2e.txt"); !errors.Is(err, ErrKeyLocked) {
		t.Errorf("principal-less open err = %v, want ErrKeyLocked", err)
	}
}

// TestUnlockForLoginEnrollRace pins the concurrent-enrollment insert race:
// N logins for the same fresh user all succeed, exactly one user_key_pw row
// survives, and the conversion completed.
func TestUnlockForLoginEnrollRace(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	aliceID := seedResolverUser(t, db, "alice")
	fs, _, res := sqlResolverFS(t, db, testKey(t))
	res.PasswordWrapped = true
	res.KDF = fastKDF
	uuid := writeV3(t, fs, "alice/a.txt", []byte("alpha"))
	fk, err := res.Resolve(ctx, uuid)
	if err != nil {
		t.Fatal(err)
	}

	const workers = 8
	privs := make([][]byte, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			privs[i], errs[i] = res.UnlockForLogin(ctx, "alice", "wonderland")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
		if len(privs[i]) != x25519KeySize {
			t.Fatalf("worker %d priv = %d bytes", i, len(privs[i]))
		}
		if !bytes.Equal(privs[i], privs[0]) {
			t.Fatalf("worker %d got a different private key (two keypairs survived?)", i)
		}
	}
	if n := rowCount(t, db, `SELECT COUNT(*) FROM user_key_pw WHERE user_id = ?`, aliceID); n != 1 {
		t.Errorf("user_key_pw rows = %d, want 1", n)
	}
	if n := rowCount(t, db, `SELECT COUNT(*) FROM user_keys WHERE user_id = ?`, aliceID); n != 0 {
		t.Errorf("user_keys rows = %d, want 0", n)
	}
	if n := rowCount(t, db, `SELECT COUNT(*) FROM file_keys WHERE user_id = ? AND scheme = 1`, aliceID); n != 1 {
		t.Errorf("scheme=1 rows = %d, want 1", n)
	}
	got, err := res.Resolve(identityCtx("alice", privs[0]), uuid)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, fk) {
		t.Error("FK changed across the racy enrollment")
	}
}

// TestResolveVsEnrollRace runs principal-less resolves concurrently with the
// enrollment transition: every outcome is one of the three legal states
// (pre-enrollment master resolve, mid-transition ErrUnresolvableKey, or
// post-enrollment ErrKeyLocked), and the final state is consistent.
func TestResolveVsEnrollRace(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	seedResolverUser(t, db, "alice")
	fs, _, res := sqlResolverFS(t, db, testKey(t))
	res.PasswordWrapped = true
	res.KDF = fastKDF
	uuid := writeV3(t, fs, "alice/a.txt", []byte("alpha"))

	var wg sync.WaitGroup
	stop := make(chan struct{})
	errs := make(chan error, 64)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := res.Resolve(ctx, uuid); err != nil &&
					!errors.Is(err, ErrKeyLocked) && !errors.Is(err, ErrUnresolvableKey) {
					errs <- err
					return
				}
			}
		}()
	}
	if _, err := res.UnlockForLogin(ctx, "alice", "wonderland"); err != nil {
		t.Fatal(err)
	}
	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("illegal resolve outcome mid-enrollment: %v", err)
	}
	if _, err := res.Resolve(ctx, uuid); !errors.Is(err, ErrKeyLocked) {
		t.Errorf("post-enrollment background resolve err = %v, want ErrKeyLocked", err)
	}
}
