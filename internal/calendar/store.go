package calendar

import (
	"context"
	"errors"
	"time"
)

var (
	ErrNotFound     = errors.New("calendar: not found")
	ErrExists       = errors.New("calendar: exists")
	ErrConflict     = errors.New("calendar: conflict")
	ErrInvalid      = errors.New("calendar: invalid")
	ErrUnsupported  = errors.New("calendar: unsupported")
	ErrForbidden    = errors.New("calendar: forbidden")
	ErrNotSupported = errors.New("calendar: method not allowed")
)

const (
	DefaultCalendarURI   = "personal"
	DefaultDisplayName   = "Personal"
	DefaultCalendarColor = "#0082c9"
	RecurUntilMS         = int64(4102444800000) // 2100-01-01
	ComponentVEVENT      = "VEVENT"
)

// Calendar is a CalDAV calendar collection.
type Calendar struct {
	ID          int64
	UserID      int64
	URI         string
	DisplayName string
	Description string
	Color       string
	Order       int
	Timezone    string
	Enabled     bool
	CTag        int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Object is a calendar object (VEVENT).
type Object struct {
	ID         int64
	CalendarID int64
	URI        string
	UID        string
	ETag       string
	Size       int64
	Component  string
	FirstOccur time.Time
	LastOccur  time.Time
	Data       []byte
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Store persists calendars and objects.
type Store interface {
	EnsureHome(ctx context.Context, userID int64) error
	ListCalendars(ctx context.Context, userID int64) ([]Calendar, error)
	GetCalendarByURI(ctx context.Context, userID int64, uri string) (*Calendar, error)
	CreateCalendar(ctx context.Context, c *Calendar) error
	UpdateCalendar(ctx context.Context, c *Calendar) error
	DeleteCalendar(ctx context.Context, userID int64, uri string) error
	PutObject(ctx context.Context, userID int64, calendarURI string, obj *Object) (created bool, err error)
	GetObject(ctx context.Context, userID int64, calendarURI, uri string) (*Object, error)
	DeleteObject(ctx context.Context, userID int64, calendarURI, uri string) error
	ListObjects(ctx context.Context, userID int64, calendarURI string) ([]Object, error)
	ObjectsInRange(ctx context.Context, userID int64, calendarURI string, start, end time.Time) ([]Object, error)
	GetByUID(ctx context.Context, calendarID int64, uid string) (*Object, error)
}
