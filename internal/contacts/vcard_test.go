package contacts

import (
	"errors"
	"testing"
)

func TestParseVCard(t *testing.T) {
	raw := []byte("BEGIN:VCARD\nVERSION:3.0\nUID:ncgo-contact-001\nFN:Ada Lovelace\nEND:VCARD\n")
	got, err := parseVCard(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.UID != "ncgo-contact-001" || got.FN != "Ada Lovelace" {
		t.Fatalf("%+v", got)
	}
}

func TestParseVCard_MissingUID(t *testing.T) {
	raw := []byte("BEGIN:VCARD\nVERSION:3.0\nFN:Ada\nEND:VCARD\n")
	if _, err := parseVCard(raw); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
}

func TestParseVCard_TwoCards(t *testing.T) {
	raw := []byte("BEGIN:VCARD\nUID:a\nEND:VCARD\nBEGIN:VCARD\nUID:b\nEND:VCARD\n")
	if _, err := parseVCard(raw); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
}

func TestParseVCard_Folded(t *testing.T) {
	raw := []byte("BEGIN:VCARD\nUID:ncgo-\n contact-001\nFN:Ada Lovelace\nEND:VCARD\n")
	got, err := parseVCard(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.UID != "ncgo-contact-001" {
		t.Errorf("uid %q", got.UID)
	}
}
