package contacts

import (
	"context"
	"errors"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/users"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

var (
	bookURIRe   = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	objectURIRe = regexp.MustCompile(`^[A-Za-z0-9._-]+\.vcf$`)
)

// DAV implements webdav.FS for /remote.php/dav/addressbooks/users/{user}/.
type DAV struct {
	Store Store
	Users users.Store
	Clock func() time.Time
}

func (d *DAV) now() time.Time {
	if d.Clock != nil {
		return d.Clock().UTC()
	}
	if s, ok := d.Store.(*SQLStore); ok && s.Clock != nil {
		return s.Clock().UTC()
	}
	return time.Now().UTC()
}

func (d *DAV) resolveUser(ctx context.Context, uid string) (*users.User, error) {
	if uid == "" || strings.Contains(uid, "/") || uid == ".." {
		return nil, webdav.ErrForbidden
	}
	u, err := d.Users.GetByUID(ctx, uid)
	if err != nil {
		if errors.Is(err, users.ErrNotFound) {
			return nil, webdav.ErrForbidden
		}
		return nil, err
	}
	if err := d.Store.EnsureHome(ctx, u.ID); err != nil {
		return nil, err
	}
	return u, nil
}

const accessOwner = "owner"

// resolvedBook is an addressbook visible to the authenticated user,
// either their own or one shared with them.
type resolvedBook struct {
	book     Addressbook // book.UserID is the owner's id; store calls use it
	access   string      // accessOwner | ShareAccessRead | ShareAccessReadWrite
	ownerUID string
	shared   bool
}

// sharedURI is the addressbook URI a sharee sees for a shared addressbook,
// following the Nextcloud "{uri}_shared_by_{owner}" naming.
func sharedURI(sb *SharedAddressbook) string {
	return sb.URI + "_shared_by_" + sb.OwnerUID
}

// resolveBook finds bookURI among the user's own addressbooks first,
// then among addressbooks shared with them (matched by sharedURI).
func (d *DAV) resolveBook(ctx context.Context, u *users.User, bookURI string) (*resolvedBook, error) {
	book, err := d.Store.GetBookByURI(ctx, u.ID, bookURI)
	if err == nil {
		return &resolvedBook{book: *book, access: accessOwner, ownerUID: u.UID}, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	shared, err := d.Store.ListSharedAddressbooks(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	for i := range shared {
		if sharedURI(&shared[i]) == bookURI {
			return &resolvedBook{book: shared[i].Addressbook, access: shared[i].Access, ownerUID: shared[i].OwnerUID, shared: true}, nil
		}
	}
	return nil, ErrNotFound
}

func (d *DAV) Stat(ctx context.Context, user, p string) (*webdav.Entry, error) {
	u, err := d.resolveUser(ctx, user)
	if err != nil {
		return nil, err
	}
	bookURI, objURI, err := splitBookPath(p)
	if err != nil {
		return nil, err
	}
	if bookURI == "" {
		return d.homeEntry(ctx, u), nil
	}
	rb, err := d.resolveBook(ctx, u, bookURI)
	if err != nil {
		return nil, mapErr(err)
	}
	if objURI == "" {
		return resolvedBookEntry(rb), nil
	}
	obj, err := d.Store.GetObject(ctx, rb.book.UserID, rb.book.URI, objURI)
	if err != nil {
		return nil, mapErr(err)
	}
	return objectEntry(bookURI, obj), nil
}

func (d *DAV) List(ctx context.Context, user, p string) ([]*webdav.Entry, error) {
	u, err := d.resolveUser(ctx, user)
	if err != nil {
		return nil, err
	}
	bookURI, objURI, err := splitBookPath(p)
	if err != nil {
		return nil, err
	}
	if objURI != "" {
		return nil, webdav.ErrNotDir
	}
	if bookURI == "" {
		books, err := d.Store.ListBooks(ctx, u.ID)
		if err != nil {
			return nil, err
		}
		shared, err := d.Store.ListSharedAddressbooks(ctx, u.ID)
		if err != nil {
			return nil, err
		}
		out := make([]*webdav.Entry, 0, len(books)+len(shared))
		for i := range books {
			out = append(out, bookEntry(&books[i]))
		}
		for i := range shared {
			out = append(out, sharedBookEntry(&shared[i]))
		}
		return out, nil
	}
	rb, err := d.resolveBook(ctx, u, bookURI)
	if err != nil {
		return nil, mapErr(err)
	}
	objs, err := d.Store.ListObjects(ctx, rb.book.UserID, rb.book.URI)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]*webdav.Entry, 0, len(objs))
	for i := range objs {
		out = append(out, objectEntry(bookURI, &objs[i]))
	}
	return out, nil
}

func (d *DAV) Read(ctx context.Context, user, p string) (io.ReadCloser, *webdav.Entry, error) {
	u, err := d.resolveUser(ctx, user)
	if err != nil {
		return nil, nil, err
	}
	bookURI, objURI, err := splitBookPath(p)
	if err != nil {
		return nil, nil, err
	}
	if objURI == "" {
		return nil, nil, webdav.ErrMethodNotAllowed
	}
	rb, err := d.resolveBook(ctx, u, bookURI)
	if err != nil {
		return nil, nil, mapErr(err)
	}
	obj, err := d.Store.GetObject(ctx, rb.book.UserID, rb.book.URI, objURI)
	if err != nil {
		return nil, nil, mapErr(err)
	}
	return io.NopCloser(strings.NewReader(string(obj.Data))), objectEntry(bookURI, obj), nil
}

func (d *DAV) Write(ctx context.Context, user, p string, r io.Reader, _ *time.Time) (*webdav.Entry, bool, error) {
	u, err := d.resolveUser(ctx, user)
	if err != nil {
		return nil, false, err
	}
	bookURI, objURI, err := splitBookPath(p)
	if err != nil {
		return nil, false, err
	}
	if objURI == "" {
		return nil, false, webdav.ErrMethodNotAllowed
	}
	rb, err := d.resolveBook(ctx, u, bookURI)
	if err != nil {
		return nil, false, mapErr(err)
	}
	if rb.shared && rb.access != ShareAccessReadWrite {
		return nil, false, mapErr(ErrForbidden)
	}
	data, err := io.ReadAll(io.LimitReader(r, 4<<20))
	if err != nil {
		return nil, false, err
	}
	obj := &Object{URI: objURI, Data: data}
	created, err := d.Store.PutObject(ctx, rb.book.UserID, rb.book.URI, obj)
	if err != nil {
		return nil, false, mapErr(err)
	}
	return objectEntry(bookURI, obj), created, nil
}

func (d *DAV) Mkdir(ctx context.Context, user, p string) (*webdav.Entry, error) {
	u, err := d.resolveUser(ctx, user)
	if err != nil {
		return nil, err
	}
	bookURI, objURI, err := splitBookPath(p)
	if err != nil {
		return nil, err
	}
	if bookURI == "" || objURI != "" {
		return nil, webdav.ErrMethodNotAllowed
	}
	b := &Addressbook{
		UserID:      u.ID,
		URI:         bookURI,
		DisplayName: bookURI,
		Enabled:     true,
	}
	if err := d.Store.CreateBook(ctx, b); err != nil {
		return nil, mapErr(err)
	}
	return bookEntry(b), nil
}

func (d *DAV) Remove(ctx context.Context, user, p string) error {
	u, err := d.resolveUser(ctx, user)
	if err != nil {
		return err
	}
	bookURI, objURI, err := splitBookPath(p)
	if err != nil {
		return err
	}
	if bookURI == "" {
		return webdav.ErrForbidden
	}
	rb, err := d.resolveBook(ctx, u, bookURI)
	if err != nil {
		return mapErr(err)
	}
	if objURI == "" {
		if rb.shared {
			return mapErr(ErrForbidden)
		}
		return mapErr(d.Store.DeleteBook(ctx, u.ID, bookURI))
	}
	if rb.shared && rb.access != ShareAccessReadWrite {
		return mapErr(ErrForbidden)
	}
	return mapErr(d.Store.DeleteObject(ctx, rb.book.UserID, rb.book.URI, objURI))
}

func (d *DAV) Move(context.Context, string, string, string, string, bool) (*webdav.Entry, bool, error) {
	return nil, false, webdav.ErrMethodNotAllowed
}

func (d *DAV) Copy(context.Context, string, string, string, string, bool, bool) (*webdav.Entry, bool, error) {
	return nil, false, webdav.ErrMethodNotAllowed
}

func (d *DAV) PatchProps(ctx context.Context, user, p string, ops []webdav.PropPatchOp) ([]webdav.PropPatchResult, error) {
	u, err := d.resolveUser(ctx, user)
	if err != nil {
		return nil, err
	}
	bookURI, objURI, err := splitBookPath(p)
	if err != nil {
		return nil, err
	}
	if objURI != "" || bookURI == "" {
		return nil, webdav.ErrForbidden
	}
	book, err := d.Store.GetBookByURI(ctx, u.ID, bookURI)
	if err != nil {
		return nil, mapErr(err)
	}
	results := make([]webdav.PropPatchResult, 0, len(ops))
	for _, op := range ops {
		st := 200
		if op.Remove {
			switch op.Name {
			case "displayname":
				book.DisplayName = book.URI
			case "description":
				book.Description = ""
			default:
				st = 403
			}
		} else {
			switch op.Name {
			case "displayname":
				book.DisplayName = op.Value
			case "description":
				book.Description = op.Value
			default:
				st = 403
			}
		}
		results = append(results, webdav.PropPatchResult{Space: op.Space, Name: op.Name, Status: st})
	}
	if err := d.Store.UpdateBook(ctx, book); err != nil {
		return nil, err
	}
	return results, nil
}

func (d *DAV) homeEntry(ctx context.Context, u *users.User) *webdav.Entry {
	now := d.now()
	etag := "0"
	if books, err := d.Store.ListBooks(ctx, u.ID); err == nil && len(books) > 0 {
		etag = strconv.FormatInt(books[0].CTag, 10)
		now = books[0].UpdatedAt
	}
	return &webdav.Entry{
		Path:        "/",
		IsDir:       true,
		ETag:        etag,
		ModTime:     now,
		NumericID:   numericID(u.ID),
		Permissions: webdav.PermAll,
		Shareable:   false,
		ContentType: "httpd/unix-directory",
		DisplayName: u.DisplayName,
	}
}

func bookEntry(b *Addressbook) *webdav.Entry {
	return &webdav.Entry{
		Path:          "/" + b.URI,
		IsDir:         true,
		IsAddressbook: true,
		Size:          0,
		ETag:          strconv.FormatInt(b.CTag, 10),
		ModTime:       b.UpdatedAt,
		NumericID:     numericID(b.ID),
		Permissions:   webdav.PermAll,
		ContentType:   "httpd/unix-directory",
		DisplayName:   b.DisplayName,
		CTag:          strconv.FormatInt(b.CTag, 10),
	}
}

// resolvedBookEntry renders a resolved (own or shared) addressbook.
func resolvedBookEntry(rb *resolvedBook) *webdav.Entry {
	e := bookEntry(&rb.book)
	if rb.shared {
		e.Path = "/" + rb.book.URI + "_shared_by_" + rb.ownerUID
		e.Shared = true
		e.ShareAccess = rb.access
		e.OwnerPrincipal = "/remote.php/dav/principals/users/" + rb.ownerUID + "/"
		e.Permissions = webdav.PermRead
		if rb.access == ShareAccessReadWrite {
			e.Permissions = webdav.PermAll
		}
	}
	return e
}

func sharedBookEntry(sb *SharedAddressbook) *webdav.Entry {
	return resolvedBookEntry(&resolvedBook{book: sb.Addressbook, access: sb.Access, ownerUID: sb.OwnerUID, shared: true})
}

func objectEntry(bookURI string, o *Object) *webdav.Entry {
	return &webdav.Entry{
		Path:        "/" + bookURI + "/" + o.URI,
		IsDir:       false,
		Size:        o.Size,
		ETag:        o.ETag,
		ModTime:     o.UpdatedAt,
		NumericID:   numericID(o.ID),
		Permissions: webdav.PermAll &^ webdav.PermShare,
		ContentType: "text/vcard; charset=utf-8",
		AddressData: string(o.Data),
	}
}

func splitBookPath(p string) (bookURI, objURI string, err error) {
	np := path.Clean("/" + strings.Trim(p, "/"))
	if np == "/" || np == "." {
		return "", "", nil
	}
	rel := strings.TrimPrefix(np, "/")
	bookURI, rest, found := strings.Cut(rel, "/")
	if !bookURIRe.MatchString(bookURI) {
		return "", "", webdav.ErrBadRequest
	}
	if !found || rest == "" {
		return bookURI, "", nil
	}
	if strings.Contains(rest, "/") || !objectURIRe.MatchString(rest) {
		return "", "", webdav.ErrBadRequest
	}
	return bookURI, rest, nil
}

func numericID(id int64) uint64 {
	if id < 0 {
		return 0
	}
	return uint64(id)
}

func mapErr(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, ErrNotFound):
		return webdav.ErrNotFound
	case errors.Is(err, ErrExists):
		return webdav.ErrExists
	case errors.Is(err, ErrConflict):
		return webdav.ErrConflict
	case errors.Is(err, ErrInvalid):
		return webdav.ErrBadRequest
	case errors.Is(err, ErrForbidden):
		return webdav.ErrForbidden
	case errors.Is(err, ErrNotSupported):
		return webdav.ErrMethodNotAllowed
	default:
		return err
	}
}
