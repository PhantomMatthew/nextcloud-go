package calendar

import (
	"errors"
	"testing"
	"time"
)

func TestParseICS_Event(t *testing.T) {
	raw := []byte("BEGIN:VCALENDAR\nVERSION:2.0\nBEGIN:VEVENT\nUID:ncgo-event-001\nDTSTART:20250501T140000Z\nDTEND:20250501T150000Z\nSUMMARY:Phase 3a\nEND:VEVENT\nEND:VCALENDAR\n")
	got, err := parseICS(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.UID != "ncgo-event-001" || got.Component != ComponentVEVENT {
		t.Fatalf("%+v", got)
	}
	if !got.FirstOccur.Equal(time.Date(2025, 5, 1, 14, 0, 0, 0, time.UTC)) {
		t.Errorf("start %v", got.FirstOccur)
	}
	if !got.LastOccur.Equal(time.Date(2025, 5, 1, 15, 0, 0, 0, time.UTC)) {
		t.Errorf("end %v", got.LastOccur)
	}
}

func TestParseICS_Todo(t *testing.T) {
	raw := []byte("BEGIN:VCALENDAR\nBEGIN:VTODO\nUID:t1\nDTSTART:20250501T140000Z\nEND:VTODO\nEND:VCALENDAR\n")
	if _, err := parseICS(raw); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v", err)
	}
}

func TestParseICS_MissingUID(t *testing.T) {
	raw := []byte("BEGIN:VCALENDAR\nBEGIN:VEVENT\nDTSTART:20250501T140000Z\nEND:VEVENT\nEND:VCALENDAR\n")
	if _, err := parseICS(raw); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
}

func TestParseICS_RRULE(t *testing.T) {
	raw := []byte("BEGIN:VCALENDAR\nBEGIN:VEVENT\nUID:r1\nDTSTART:20250501T140000Z\nRRULE:FREQ=DAILY\nEND:VEVENT\nEND:VCALENDAR\n")
	got, err := parseICS(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastOccur.UnixMilli() != RecurUntilMS {
		t.Errorf("last = %d", got.LastOccur.UnixMilli())
	}
}

func TestParseICS_Folded(t *testing.T) {
	raw := []byte("BEGIN:VCALENDAR\nBEGIN:VEVENT\nUID:ncgo-\n event-001\nDTSTART:20250501T140000Z\nEND:VEVENT\nEND:VCALENDAR\n")
	got, err := parseICS(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.UID != "ncgo-event-001" {
		t.Errorf("uid %q", got.UID)
	}
}

func TestParseICSDuration(t *testing.T) {
	d, err := parseICSDuration("PT1H30M")
	if err != nil || d != 90*time.Minute {
		t.Fatalf("%v %v", d, err)
	}
}
