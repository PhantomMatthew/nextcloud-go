package calendar

import (
	"bytes"
	"context"
	"regexp"
	"strings"

	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

// Attendee is one ATTENDEE of a scheduled event.
type Attendee struct {
	Email    string
	Partstat string
}

// SchedulingInfo is the ORGANIZER/ATTENDEE view of a VEVENT.
type SchedulingInfo struct {
	Organizer string
	Attendees []Attendee
}

var partstatRe = regexp.MustCompile(`PARTSTAT=[^;:]+`)

// parseScheduling extracts ORGANIZER and ATTENDEE emails (mailto stripped)
// from a VCALENDAR payload. Returns nil when there is no ORGANIZER.
func parseScheduling(data []byte) *SchedulingInfo {
	text := unfoldICS(string(data))
	var info *SchedulingInfo
	for _, line := range strings.Split(text, "\n") {
		name, params, value, ok := splitProp(strings.TrimSpace(line))
		if !ok {
			continue
		}
		email := mailtoEmail(value)
		if email == "" {
			continue
		}
		switch name {
		case "ORGANIZER":
			if info == nil {
				info = &SchedulingInfo{}
			}
			info.Organizer = email
		case "ATTENDEE":
			if info == nil {
				info = &SchedulingInfo{}
			}
			ps := strings.ToUpper(params["PARTSTAT"])
			if ps == "" {
				ps = "NEEDS-ACTION"
			}
			info.Attendees = append(info.Attendees, Attendee{Email: email, Partstat: ps})
		}
	}
	return info
}

func mailtoEmail(value string) string {
	if strings.HasPrefix(strings.ToLower(value), "mailto:") {
		return value[len("mailto:"):]
	}
	return ""
}

// updateAttendeePartstat rewrites the PARTSTAT of the ATTENDEE line for
// email, inserting the parameter when absent. Per-line \r style is
// preserved. Returns data unchanged when no ATTENDEE line matches.
// Folded ATTENDEE lines are not matched.
func updateAttendeePartstat(data []byte, email, partstat string) []byte {
	needle := strings.ToLower("mailto:" + email)
	lines := strings.Split(string(data), "\n")
	changed := false
	for i, line := range lines {
		body := strings.TrimSuffix(line, "\r")
		if !strings.HasPrefix(body, "ATTENDEE") {
			continue
		}
		if !strings.Contains(strings.ToLower(body), needle) {
			continue
		}
		if partstatRe.MatchString(body) {
			body = partstatRe.ReplaceAllString(body, "PARTSTAT="+partstat)
		} else {
			body = strings.Replace(body, "ATTENDEE", "ATTENDEE;PARTSTAT="+partstat, 1)
		}
		if strings.HasSuffix(line, "\r") {
			body += "\r"
		}
		lines[i] = body
		changed = true
	}
	if !changed {
		return data
	}
	return []byte(strings.Join(lines, "\n"))
}

// scheduleWrite applies iTIP delivery for a write to an own calendar:
// an organizer write delivers invitations (REQUEST/UPDATE), an attendee
// write with a local organizer propagates the PARTSTAT reply (REPLY).
func (d *DAV) scheduleWrite(ctx context.Context, u *users.User, data []byte) error {
	if u.Email == "" {
		return nil
	}
	info := parseScheduling(data)
	if info == nil || len(info.Attendees) == 0 {
		return nil
	}
	ev, err := parseICS(data)
	if err != nil {
		return err // unreachable: the store already accepted the payload
	}
	if strings.EqualFold(info.Organizer, u.Email) {
		return d.deliverInvites(ctx, u, ev, info, data)
	}
	return d.applyReply(ctx, u, ev, info)
}

// scheduleCancel removes a deleted event from local attendees when the
// writer organized it (CANCEL).
func (d *DAV) scheduleCancel(ctx context.Context, u *users.User, rc *resolvedCalendar, objURI string) error {
	if u.Email == "" {
		return nil
	}
	obj, err := d.Store.GetObject(ctx, rc.cal.UserID, rc.cal.URI, objURI)
	if err != nil {
		return nil //nolint:nilerr // nothing to cancel; the delete reports the error
	}
	info := parseScheduling(obj.Data)
	if info == nil || !strings.EqualFold(info.Organizer, u.Email) {
		return nil
	}
	return d.cancelInvites(ctx, u, obj, info)
}

// deliverInvites stores a copy of a scheduled event in each local
// attendee's default calendar. External or unknown emails are skipped
// (no iMIP).
func (d *DAV) deliverInvites(ctx context.Context, writer *users.User, ev *parsedEvent, info *SchedulingInfo, data []byte) error {
	for _, att := range info.Attendees {
		if strings.EqualFold(att.Email, writer.Email) {
			continue
		}
		target, err := d.Users.GetByEmail(ctx, att.Email)
		if err != nil {
			continue
		}
		if err := d.Store.EnsureHome(ctx, target.ID); err != nil {
			return err
		}
		cal, err := d.Store.GetCalendarByURI(ctx, target.ID, DefaultCalendarURI)
		if err != nil {
			return err
		}
		uri := ev.UID + ".ics"
		if old, err := d.Store.GetByUID(ctx, cal.ID, ev.UID); err == nil {
			uri = old.URI
		}
		if _, err := d.Store.PutObject(ctx, target.ID, DefaultCalendarURI, &Object{URI: uri, Data: data}); err != nil {
			return err
		}
	}
	return nil
}

// applyReply propagates an attendee's PARTSTAT change to the organizer's
// copy of the event.
func (d *DAV) applyReply(ctx context.Context, writer *users.User, ev *parsedEvent, info *SchedulingInfo) error {
	partstat := ""
	for _, att := range info.Attendees {
		if strings.EqualFold(att.Email, writer.Email) {
			partstat = att.Partstat
			break
		}
	}
	if partstat == "" {
		return nil
	}
	organizer, err := d.Users.GetByEmail(ctx, info.Organizer)
	if err != nil {
		return nil //nolint:nilerr // external organizer: nothing to update
	}
	cal, err := d.Store.GetCalendarByURI(ctx, organizer.ID, DefaultCalendarURI)
	if err != nil {
		return nil //nolint:nilerr // organizer has no default calendar here
	}
	obj, err := d.Store.GetByUID(ctx, cal.ID, ev.UID)
	if err != nil {
		return nil //nolint:nilerr // organizer copy missing: nothing to update
	}
	updated := updateAttendeePartstat(obj.Data, writer.Email, partstat)
	if bytes.Equal(updated, obj.Data) {
		return nil
	}
	_, err = d.Store.PutObject(ctx, organizer.ID, DefaultCalendarURI, &Object{URI: obj.URI, Data: updated})
	return err
}

// cancelInvites removes a deleted scheduled event from each local
// attendee's default calendar.
func (d *DAV) cancelInvites(ctx context.Context, writer *users.User, obj *Object, info *SchedulingInfo) error {
	for _, att := range info.Attendees {
		if strings.EqualFold(att.Email, writer.Email) {
			continue
		}
		target, err := d.Users.GetByEmail(ctx, att.Email)
		if err != nil {
			continue
		}
		cal, err := d.Store.GetCalendarByURI(ctx, target.ID, DefaultCalendarURI)
		if err != nil {
			continue
		}
		attObj, err := d.Store.GetByUID(ctx, cal.ID, obj.UID)
		if err != nil {
			continue
		}
		if err := d.Store.DeleteObject(ctx, target.ID, DefaultCalendarURI, attObj.URI); err != nil {
			return err
		}
	}
	return nil
}
