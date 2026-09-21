package calendar

import (
	"crypto/sha1" //nolint:gosec // non-cryptographic: ETag fingerprint
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type parsedEvent struct {
	UID        string
	Component  string
	FirstOccur time.Time
	LastOccur  time.Time
	Recurring  bool
}

func parseICS(data []byte) (*parsedEvent, error) {
	text := unfoldICS(string(data))
	if !strings.Contains(text, "BEGIN:VCALENDAR") {
		return nil, fmt.Errorf("%w: missing VCALENDAR", ErrInvalid)
	}
	nEvent := strings.Count(text, "BEGIN:VEVENT")
	nTodo := strings.Count(text, "BEGIN:VTODO")
	nJournal := strings.Count(text, "BEGIN:VJOURNAL")
	if nJournal > 0 {
		return nil, ErrUnsupported
	}
	if nEvent > 0 && nTodo > 0 {
		return nil, ErrUnsupported
	}
	if nEvent+nTodo != 1 {
		return nil, fmt.Errorf("%w: want exactly one VEVENT or VTODO", ErrInvalid)
	}
	if nTodo == 1 {
		return parseVTODO(text)
	}
	block := veventBlock(text)
	props := parseProps(block)
	uid := props["UID"]
	if uid == "" {
		return nil, fmt.Errorf("%w: missing UID", ErrInvalid)
	}
	start, ok := parseICSTime(props["DTSTART"], props["DTSTART_TZID"], props["DTSTART_VALUE"])
	if !ok {
		return nil, fmt.Errorf("%w: missing DTSTART", ErrInvalid)
	}
	end := start
	if raw, exists := props["DTEND"]; exists {
		if t, pok := parseICSTime(raw, props["DTEND_TZID"], props["DTEND_VALUE"]); pok {
			end = t
		}
	} else if dur, exists := props["DURATION"]; exists {
		if d, perr := parseICSDuration(dur); perr == nil {
			end = start.Add(d)
		}
	}
	recurring := props["RRULE"] != ""
	if recurring {
		end = time.UnixMilli(RecurUntilMS).UTC()
	}
	return &parsedEvent{
		UID:        uid,
		Component:  ComponentVEVENT,
		FirstOccur: start,
		LastOccur:  end,
		Recurring:  recurring,
	}, nil
}

// parseVTODO parses a single VTODO block. DTSTART is optional; the time
// anchor falls back to DUE, COMPLETED, then CREATED. A todo with no dates
// gets zero FirstOccur/LastOccur and only matches range-unbounded queries.
func parseVTODO(text string) (*parsedEvent, error) {
	block := componentBlock(text, "VTODO")
	props := parseProps(block)
	uid := props["UID"]
	if uid == "" {
		return nil, fmt.Errorf("%w: missing UID", ErrInvalid)
	}
	var first, last time.Time
	if start, ok := parseICSTime(props["DTSTART"], props["DTSTART_TZID"], props["DTSTART_VALUE"]); ok {
		first, last = start, start
		if raw, exists := props["DUE"]; exists {
			if due, dok := parseICSTime(raw, props["DUE_TZID"], props["DUE_VALUE"]); dok {
				last = due
			}
		} else if dur, exists := props["DURATION"]; exists {
			if d, perr := parseICSDuration(dur); perr == nil {
				last = start.Add(d)
			}
		}
	} else if due, ok := parseICSTime(props["DUE"], props["DUE_TZID"], props["DUE_VALUE"]); ok {
		first, last = due, due
	} else if done, ok := parseICSTime(props["COMPLETED"], props["COMPLETED_TZID"], props["COMPLETED_VALUE"]); ok {
		first, last = done, done
	} else if created, ok := parseICSTime(props["CREATED"], props["CREATED_TZID"], props["CREATED_VALUE"]); ok {
		first, last = created, created
	}
	return &parsedEvent{
		UID:        uid,
		Component:  ComponentVTODO,
		FirstOccur: first,
		LastOccur:  last,
	}, nil
}

func objectETag(data []byte) string {
	h := sha1.New() //nolint:gosec // non-cryptographic: ETag fingerprint
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func unfoldICS(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	var b strings.Builder
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if i > 0 && len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
			b.WriteString(line[1:])
			continue
		}
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
	}
	return b.String()
}

func veventBlock(text string) string {
	return componentBlock(text, "VEVENT")
}

func componentBlock(text, name string) string {
	start := strings.Index(text, "BEGIN:"+name)
	end := strings.Index(text, "END:"+name)
	if start < 0 || end < start {
		return ""
	}
	return text[start:end]
}

func parseProps(block string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "BEGIN:") || strings.HasPrefix(line, "END:") {
			continue
		}
		name, params, value, ok := splitProp(line)
		if !ok {
			continue
		}
		out[name] = value
		if tz, ok := params["TZID"]; ok {
			out[name+"_TZID"] = tz
		}
		if v, ok := params["VALUE"]; ok {
			out[name+"_VALUE"] = v
		}
	}
	return out
}

func splitProp(line string) (name string, params map[string]string, value string, ok bool) {
	colon := strings.IndexByte(line, ':')
	if colon < 0 {
		return "", nil, "", false
	}
	head := line[:colon]
	value = line[colon+1:]
	parts := strings.Split(head, ";")
	name = strings.ToUpper(parts[0])
	params = map[string]string{}
	for _, p := range parts[1:] {
		k, v, found := strings.Cut(p, "=")
		if !found {
			continue
		}
		params[strings.ToUpper(k)] = strings.Trim(v, `"`)
	}
	return name, params, value, true
}

func parseICSTime(raw, tzid, value string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if strings.EqualFold(value, "DATE") || len(raw) == 8 {
		t, err := time.Parse("20060102", raw)
		if err != nil {
			return time.Time{}, false
		}
		return t.UTC(), true
	}
	if strings.HasSuffix(raw, "Z") {
		t, err := time.Parse("20060102T150405Z", raw)
		if err != nil {
			return time.Time{}, false
		}
		return t.UTC(), true
	}
	t, err := time.Parse("20060102T150405", raw)
	if err != nil {
		return time.Time{}, false
	}
	_ = tzid // 3a: TZID without VTIMEZONE lookup is stored as UTC
	return t.UTC(), true
}

func parseICSDuration(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw[0] != 'P' {
		return 0, fmt.Errorf("%w: duration", ErrInvalid)
	}
	neg := false
	if strings.HasPrefix(raw, "-") {
		neg = true
		raw = raw[1:]
	}
	raw = strings.TrimPrefix(raw, "P")
	var days, hours, mins, secs int
	inTime := false
	num := ""
	flush := func(unit rune) error {
		if num == "" {
			return fmt.Errorf("%w: duration", ErrInvalid)
		}
		n, err := strconv.Atoi(num)
		if err != nil {
			return fmt.Errorf("%w: duration", ErrInvalid)
		}
		num = ""
		switch unit {
		case 'W':
			days += n * 7
		case 'D':
			days += n
		case 'H':
			hours += n
		case 'M':
			mins += n
		case 'S':
			secs += n
		default:
			return fmt.Errorf("%w: duration", ErrInvalid)
		}
		return nil
	}
	for _, r := range raw {
		if unicode.IsDigit(r) {
			num += string(r)
			continue
		}
		if r == 'T' {
			inTime = true
			continue
		}
		if !inTime && (r == 'H' || r == 'S') {
			return 0, fmt.Errorf("%w: duration", ErrInvalid)
		}
		if err := flush(r); err != nil {
			return 0, err
		}
	}
	if num != "" {
		return 0, fmt.Errorf("%w: duration", ErrInvalid)
	}
	d := time.Duration(days)*24*time.Hour + time.Duration(hours)*time.Hour + time.Duration(mins)*time.Minute + time.Duration(secs)*time.Second
	if neg {
		d = -d
	}
	return d, nil
}
