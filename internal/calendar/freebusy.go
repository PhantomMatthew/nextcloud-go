package calendar

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

const maxFreeBusyOccurrences = 1000

const icsTimeFormat = "20060102T150405Z"

// RRule is the RFC 5545 RRULE subset supported for occurrence expansion.
// BY* rule parts (BYDAY, BYMONTH, ...) are parsed over and ignored.
type RRule struct {
	Freq     string
	Interval int
	Count    int
	Until    time.Time
}

func parseRRule(raw string) (*RRule, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("%w: empty RRULE", ErrInvalid)
	}
	r := &RRule{Interval: 1}
	for _, part := range strings.Split(raw, ";") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		switch strings.ToUpper(k) {
		case "FREQ":
			r.Freq = strings.ToUpper(v)
		case "INTERVAL":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return nil, fmt.Errorf("%w: RRULE INTERVAL", ErrInvalid)
			}
			r.Interval = n
		case "COUNT":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return nil, fmt.Errorf("%w: RRULE COUNT", ErrInvalid)
			}
			r.Count = n
		case "UNTIL":
			if t, ok := parseICSTime(v, "", ""); ok {
				r.Until = t
			}
		}
	}
	switch r.Freq {
	case "DAILY", "WEEKLY", "MONTHLY", "YEARLY":
	default:
		return nil, fmt.Errorf("%w: RRULE FREQ %q", ErrUnsupported, r.Freq)
	}
	return r, nil
}

// expandBusy returns the busy intervals of ev within [winStart, winEnd).
// A recurring event whose RRULE cannot be expanded contributes only its
// first occurrence.
func expandBusy(ev *parsedEvent, winStart, winEnd time.Time) [][2]time.Time {
	dur := ev.Dur
	if dur < 0 {
		dur = 0
	}
	clip := func(s time.Time) ([][2]time.Time, bool) {
		e := s.Add(dur)
		if s.Before(winEnd) && e.After(winStart) {
			return [][2]time.Time{{s, e}}, true
		}
		return nil, false
	}
	if !ev.Recurring {
		iv, _ := clip(ev.FirstOccur)
		return iv
	}
	r, err := parseRRule(ev.RRule)
	if err != nil {
		iv, _ := clip(ev.FirstOccur)
		return iv
	}
	var out [][2]time.Time
	t := ev.FirstOccur
	for i := 0; i < maxFreeBusyOccurrences; i++ {
		if r.Count > 0 && i >= r.Count {
			break
		}
		if !r.Until.IsZero() && t.After(r.Until) {
			break
		}
		if !t.Before(winEnd) {
			break
		}
		if iv, ok := clip(t); ok {
			out = append(out, iv...)
		}
		t = stepFreq(t, r.Freq, r.Interval)
	}
	return out
}

func stepFreq(t time.Time, freq string, interval int) time.Time {
	switch freq {
	case "DAILY":
		return t.AddDate(0, 0, interval)
	case "WEEKLY":
		return t.AddDate(0, 0, 7*interval)
	case "MONTHLY":
		return t.AddDate(0, interval, 0)
	case "YEARLY":
		return t.AddDate(interval, 0, 0)
	}
	return t
}

// mergeBusy sorts and coalesces overlapping or adjacent busy intervals.
func mergeBusy(ivs [][2]time.Time) [][2]time.Time {
	if len(ivs) == 0 {
		return nil
	}
	sort.Slice(ivs, func(i, j int) bool { return ivs[i][0].Before(ivs[j][0]) })
	out := [][2]time.Time{ivs[0]}
	for _, iv := range ivs[1:] {
		last := &out[len(out)-1]
		if !iv[0].After(last[1]) {
			if iv[1].After(last[1]) {
				last[1] = iv[1]
			}
			continue
		}
		out = append(out, iv)
	}
	return out
}

// FreeBusy renders a VFREEBUSY document for the user's calendar(s) over
// [start, end). Transparent events are excluded; busy intervals merge.
func (d *DAV) FreeBusy(ctx context.Context, user, calURI string, start, end time.Time) ([]byte, error) {
	u, err := d.resolveUser(ctx, user)
	if err != nil {
		return nil, err
	}
	var cals []Calendar
	if calURI == "" {
		listed, err := d.Store.ListCalendars(ctx, u.ID)
		if err != nil {
			return nil, err
		}
		cals = listed
		shared, err := d.Store.ListSharedCalendars(ctx, u.ID)
		if err != nil {
			return nil, err
		}
		for i := range shared {
			cals = append(cals, shared[i].Calendar)
		}
	} else {
		rc, err := d.resolveCalendar(ctx, u, calURI)
		if err != nil {
			return nil, mapErr(err)
		}
		cals = []Calendar{rc.cal}
	}
	var busy [][2]time.Time
	for i := range cals {
		objs, err := d.Store.ObjectsInRange(ctx, u.ID, cals[i].URI, start, end)
		if err != nil {
			return nil, mapErr(err)
		}
		for j := range objs {
			if objs[j].Component != ComponentVEVENT {
				continue
			}
			ev, perr := parseICS(objs[j].Data)
			if perr != nil || ev.Transparent {
				continue
			}
			busy = append(busy, expandBusy(ev, start, end)...)
		}
	}
	busy = mergeBusy(busy)
	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//nextcloud-go//EN\r\nBEGIN:VFREEBUSY\r\n")
	fmt.Fprintf(&b, "DTSTAMP:%s\r\n", d.now().UTC().Format(icsTimeFormat))
	fmt.Fprintf(&b, "DTSTART:%s\r\n", start.UTC().Format(icsTimeFormat))
	fmt.Fprintf(&b, "DTEND:%s\r\n", end.UTC().Format(icsTimeFormat))
	for _, iv := range busy {
		fmt.Fprintf(&b, "FREEBUSY;FBTYPE=BUSY:%s/%s\r\n", iv[0].UTC().Format(icsTimeFormat), iv[1].UTC().Format(icsTimeFormat))
	}
	b.WriteString("END:VFREEBUSY\r\nEND:VCALENDAR\r\n")
	return []byte(b.String()), nil
}

// RawReport answers free-busy-query with a raw VFREEBUSY body. Other
// REPORT names return ok=false so the multistatus path handles them.
func (d *DAV) RawReport(ctx context.Context, user, p string, req webdav.ReportRequest) ([]byte, string, bool, error) {
	name := req.Name
	if name == "" {
		name = reportName(req.Body)
	}
	if name != "free-busy-query" {
		return nil, "", false, nil
	}
	calURI, objURI, err := splitCalPath(p)
	if err != nil {
		return nil, "", true, err
	}
	if objURI != "" {
		return nil, "", true, webdav.ErrBadRequest
	}
	_, start, end := parseCompFilter(req.Body)
	if start.IsZero() || end.IsZero() {
		return nil, "", true, webdav.ErrBadRequest
	}
	body, err := d.FreeBusy(ctx, user, calURI, start, end)
	if err != nil {
		return nil, "", true, err
	}
	return body, "text/calendar; charset=utf-8", true, nil
}
