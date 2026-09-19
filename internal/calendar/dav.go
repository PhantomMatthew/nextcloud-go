package calendar

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
	calendarURIRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	objectURIRe   = regexp.MustCompile(`^[A-Za-z0-9._-]+\.ics$`)
)

// DAV implements webdav.FS for /remote.php/dav/calendars/{user}/.
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

func (d *DAV) Stat(ctx context.Context, user, p string) (*webdav.Entry, error) {
	u, err := d.resolveUser(ctx, user)
	if err != nil {
		return nil, err
	}
	calURI, objURI, err := splitCalPath(p)
	if err != nil {
		return nil, err
	}
	if calURI == "" {
		return d.homeEntry(ctx, u), nil
	}
	cal, err := d.Store.GetCalendarByURI(ctx, u.ID, calURI)
	if err != nil {
		return nil, mapErr(err)
	}
	if objURI == "" {
		return calendarEntry(cal), nil
	}
	obj, err := d.Store.GetObject(ctx, u.ID, calURI, objURI)
	if err != nil {
		return nil, mapErr(err)
	}
	return objectEntry(calURI, obj), nil
}

func (d *DAV) List(ctx context.Context, user, p string) ([]*webdav.Entry, error) {
	u, err := d.resolveUser(ctx, user)
	if err != nil {
		return nil, err
	}
	calURI, objURI, err := splitCalPath(p)
	if err != nil {
		return nil, err
	}
	if objURI != "" {
		return nil, webdav.ErrNotDir
	}
	if calURI == "" {
		cals, err := d.Store.ListCalendars(ctx, u.ID)
		if err != nil {
			return nil, err
		}
		out := make([]*webdav.Entry, 0, len(cals))
		for i := range cals {
			out = append(out, calendarEntry(&cals[i]))
		}
		return out, nil
	}
	objs, err := d.Store.ListObjects(ctx, u.ID, calURI)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]*webdav.Entry, 0, len(objs))
	for i := range objs {
		out = append(out, objectEntry(calURI, &objs[i]))
	}
	return out, nil
}

func (d *DAV) Read(ctx context.Context, user, p string) (io.ReadCloser, *webdav.Entry, error) {
	u, err := d.resolveUser(ctx, user)
	if err != nil {
		return nil, nil, err
	}
	calURI, objURI, err := splitCalPath(p)
	if err != nil {
		return nil, nil, err
	}
	if objURI == "" {
		return nil, nil, webdav.ErrMethodNotAllowed
	}
	obj, err := d.Store.GetObject(ctx, u.ID, calURI, objURI)
	if err != nil {
		return nil, nil, mapErr(err)
	}
	return io.NopCloser(strings.NewReader(string(obj.Data))), objectEntry(calURI, obj), nil
}

func (d *DAV) Write(ctx context.Context, user, p string, r io.Reader, _ *time.Time) (*webdav.Entry, bool, error) {
	u, err := d.resolveUser(ctx, user)
	if err != nil {
		return nil, false, err
	}
	calURI, objURI, err := splitCalPath(p)
	if err != nil {
		return nil, false, err
	}
	if objURI == "" {
		return nil, false, webdav.ErrMethodNotAllowed
	}
	data, err := io.ReadAll(io.LimitReader(r, 4<<20))
	if err != nil {
		return nil, false, err
	}
	obj := &Object{URI: objURI, Data: data}
	created, err := d.Store.PutObject(ctx, u.ID, calURI, obj)
	if err != nil {
		return nil, false, mapErr(err)
	}
	return objectEntry(calURI, obj), created, nil
}

func (d *DAV) Mkdir(ctx context.Context, user, p string) (*webdav.Entry, error) {
	return d.MkCalendar(ctx, user, p, nil)
}

func (d *DAV) MkCalendar(ctx context.Context, user, p string, props map[string]string) (*webdav.Entry, error) {
	u, err := d.resolveUser(ctx, user)
	if err != nil {
		return nil, err
	}
	calURI, objURI, err := splitCalPath(p)
	if err != nil {
		return nil, err
	}
	if calURI == "" || objURI != "" {
		return nil, webdav.ErrMethodNotAllowed
	}
	c := &Calendar{
		UserID:      u.ID,
		URI:         calURI,
		DisplayName: calURI,
		Color:       DefaultCalendarColor,
		Enabled:     true,
	}
	if props != nil {
		if v := props["displayname"]; v != "" {
			c.DisplayName = v
		}
		if v := props["calendar-color"]; v != "" {
			c.Color = v
		}
	}
	if err := d.Store.CreateCalendar(ctx, c); err != nil {
		return nil, mapErr(err)
	}
	return calendarEntry(c), nil
}

func (d *DAV) Remove(ctx context.Context, user, p string) error {
	u, err := d.resolveUser(ctx, user)
	if err != nil {
		return err
	}
	calURI, objURI, err := splitCalPath(p)
	if err != nil {
		return err
	}
	if calURI == "" {
		return webdav.ErrForbidden
	}
	if objURI == "" {
		return mapErr(d.Store.DeleteCalendar(ctx, u.ID, calURI))
	}
	return mapErr(d.Store.DeleteObject(ctx, u.ID, calURI, objURI))
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
	calURI, objURI, err := splitCalPath(p)
	if err != nil {
		return nil, err
	}
	if objURI != "" || calURI == "" {
		return nil, webdav.ErrForbidden
	}
	cal, err := d.Store.GetCalendarByURI(ctx, u.ID, calURI)
	if err != nil {
		return nil, mapErr(err)
	}
	results := make([]webdav.PropPatchResult, 0, len(ops))
	for _, op := range ops {
		st := 200
		if op.Remove {
			switch op.Name {
			case "displayname":
				cal.DisplayName = cal.URI
			case "calendar-color":
				cal.Color = DefaultCalendarColor
			case "calendar-description":
				cal.Description = ""
			case "calendar-order":
				cal.Order = 0
			case "calendar-enabled":
				cal.Enabled = true
			default:
				st = 403
			}
		} else {
			switch op.Name {
			case "displayname":
				cal.DisplayName = op.Value
			case "calendar-color":
				cal.Color = op.Value
			case "calendar-description":
				cal.Description = op.Value
			case "calendar-order":
				n, perr := strconv.Atoi(op.Value)
				if perr != nil {
					st = 400
				} else {
					cal.Order = n
				}
			case "calendar-enabled":
				cal.Enabled = op.Value != "0" && !strings.EqualFold(op.Value, "false")
			default:
				st = 403
			}
		}
		results = append(results, webdav.PropPatchResult{Space: op.Space, Name: op.Name, Status: st})
	}
	if err := d.Store.UpdateCalendar(ctx, cal); err != nil {
		return nil, err
	}
	return results, nil
}

func (d *DAV) homeEntry(ctx context.Context, u *users.User) *webdav.Entry {
	now := d.now()
	etag := "0"
	if cals, err := d.Store.ListCalendars(ctx, u.ID); err == nil && len(cals) > 0 {
		etag = strconv.FormatInt(cals[0].CTag, 10)
		now = cals[0].UpdatedAt
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

func calendarEntry(c *Calendar) *webdav.Entry {
	return &webdav.Entry{
		Path:                "/" + c.URI,
		IsDir:               true,
		IsCalendar:          true,
		Size:                0,
		ETag:                strconv.FormatInt(c.CTag, 10),
		ModTime:             c.UpdatedAt,
		NumericID:           numericID(c.ID),
		Permissions:         webdav.PermAll,
		ContentType:         "httpd/unix-directory",
		DisplayName:         c.DisplayName,
		CTag:                strconv.FormatInt(c.CTag, 10),
		CalendarColor:       c.Color,
		CalendarOrder:       c.Order,
		CalendarEnabled:     c.Enabled,
		CalendarDescription: c.Description,
	}
}

func objectEntry(calURI string, o *Object) *webdav.Entry {
	return &webdav.Entry{
		Path:         "/" + calURI + "/" + o.URI,
		IsDir:        false,
		Size:         o.Size,
		ETag:         o.ETag,
		ModTime:      o.UpdatedAt,
		NumericID:    numericID(o.ID),
		Permissions:  webdav.PermAll &^ webdav.PermShare,
		ContentType:  "text/calendar; charset=utf-8",
		CalendarData: string(o.Data),
	}
}

func splitCalPath(p string) (calURI, objURI string, err error) {
	np := path.Clean("/" + strings.Trim(p, "/"))
	if np == "/" || np == "." {
		return "", "", nil
	}
	rel := strings.TrimPrefix(np, "/")
	calURI, rest, found := strings.Cut(rel, "/")
	if !calendarURIRe.MatchString(calURI) {
		return "", "", webdav.ErrBadRequest
	}
	if !found || rest == "" {
		return calURI, "", nil
	}
	if strings.Contains(rest, "/") || !objectURIRe.MatchString(rest) {
		return "", "", webdav.ErrBadRequest
	}
	return calURI, rest, nil
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
	case errors.Is(err, ErrUnsupported):
		return webdav.ErrUnsupportedMedia
	case errors.Is(err, ErrForbidden):
		return webdav.ErrForbidden
	case errors.Is(err, ErrNotSupported):
		return webdav.ErrMethodNotAllowed
	default:
		return err
	}
}
