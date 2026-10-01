package activity

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// stubSubjectDecryptor resolves keyHex|token pairs, failing everything else.
type stubSubjectDecryptor struct {
	names map[string]string
}

func (s stubSubjectDecryptor) DecryptSubjectName(_ context.Context, keyUUIDHex, token string) (string, error) {
	if n, ok := s.names[keyUUIDHex+"|"+token]; ok {
		return n, nil
	}
	return "", errors.New("stub: unresolvable token")
}

func decryptorHandler() Handler {
	return Handler{Subjects: stubSubjectDecryptor{names: map[string]string{
		"aaaa|tok-file": "hello.txt",
		"bbbb|tok-old":  "old.txt",
	}}}
}

func TestDecryptSubjectNilSeamVerbatim(t *testing.T) {
	e := &Event{Subject: "s", SubjectRichParameters: `{"ncgoNameScheme":{"type":"ncgo","id":"aaaa","name":"1"}}`}
	Handler{}.decryptSubject(context.Background(), e)
	if e.Subject != "s" || !strings.Contains(e.SubjectRichParameters, "ncgoNameScheme") {
		t.Errorf("nil seam rewrote the row: %+v", e)
	}
}

func TestDecryptSubjectUnmarkedVerbatim(t *testing.T) {
	raw := `{"file":{"type":"file","id":"2","name":"plain.txt"}}`
	e := &Event{Subject: "alice created plain.txt", SubjectRich: "{actor} created {file}", SubjectRichParameters: raw, ObjectName: "plain.txt"}
	decryptorHandler().decryptSubject(context.Background(), e)
	if e.SubjectRichParameters != raw || e.Subject != "alice created plain.txt" || e.ObjectName != "plain.txt" {
		t.Errorf("unmarked row rewritten: %+v", e)
	}
}

func TestDecryptSubjectMalformedVerbatim(t *testing.T) {
	h := decryptorHandler()
	// Unparseable params JSON.
	e := &Event{Subject: "s", SubjectRichParameters: `{not json`}
	h.decryptSubject(context.Background(), e)
	if e.Subject != "s" || e.SubjectRichParameters != `{not json` {
		t.Errorf("malformed JSON row rewritten: %+v", e)
	}
	// A marker with the wrong scheme name leaves the row verbatim too.
	raw := `{"ncgoNameScheme":{"type":"ncgo","id":"aaaa","name":"2"},"file":{"type":"file","id":"2","name":"tok-file"}}`
	e = &Event{Subject: "s", SubjectRichParameters: raw, ObjectName: "tok-file"}
	h.decryptSubject(context.Background(), e)
	if e.Subject != "s" || e.SubjectRichParameters != raw || e.ObjectName != "tok-file" {
		t.Errorf("unknown-scheme marker row rewritten: %+v", e)
	}
}

func TestDecryptSubjectSingleMarker(t *testing.T) {
	e := &Event{
		Subject:               "alice created tok-file",
		SubjectRich:           "{actor} created {file}",
		SubjectRichParameters: `{"actor":{"type":"user","id":"alice","name":"alice"},"file":{"type":"file","id":"2","name":"tok-file"},"ncgoNameScheme":{"type":"ncgo","id":"aaaa","name":"1"}}`,
		ObjectName:            "tok-file",
	}
	decryptorHandler().decryptSubject(context.Background(), e)
	if e.Subject != "alice created hello.txt" {
		t.Errorf("subject = %q, want rebuilt from the template", e.Subject)
	}
	if e.ObjectName != "hello.txt" {
		t.Errorf("object name = %q, want the decrypted file name", e.ObjectName)
	}
	if strings.Contains(e.SubjectRichParameters, "ncgoNameScheme") || strings.Contains(e.SubjectRichParameters, "tok-file") {
		t.Errorf("params keep the marker or the token: %q", e.SubjectRichParameters)
	}
	if !strings.Contains(e.SubjectRichParameters, "hello.txt") {
		t.Errorf("params lost the decrypted name: %q", e.SubjectRichParameters)
	}
}

func TestDecryptSubjectDoubleMarkerRename(t *testing.T) {
	e := &Event{
		Subject:               "alice renamed tok-old to tok-file",
		SubjectRich:           "{actor} renamed {oldfile} to {file}",
		SubjectRichParameters: `{"actor":{"type":"user","id":"alice","name":"alice"},"file":{"type":"file","id":"2","name":"tok-file"},"oldfile":{"type":"file","id":"2","name":"tok-old"},"ncgoNameScheme":{"type":"ncgo","id":"aaaa","name":"1"},"ncgoNameScheme:oldfile":{"type":"ncgo","id":"bbbb","name":"1"}}`,
		ObjectName:            "tok-file",
	}
	decryptorHandler().decryptSubject(context.Background(), e)
	if e.Subject != "alice renamed old.txt to hello.txt" {
		t.Errorf("subject = %q, want both params decrypted", e.Subject)
	}
	if e.ObjectName != "hello.txt" {
		t.Errorf("object name = %q", e.ObjectName)
	}
	if strings.Contains(e.SubjectRichParameters, "ncgoNameScheme") || strings.Contains(e.SubjectRichParameters, "tok-") {
		t.Errorf("params keep markers or tokens: %q", e.SubjectRichParameters)
	}
}

func TestDecryptSubjectFailurePlaceholder(t *testing.T) {
	e := &Event{
		Subject:               "alice created tok-unknown",
		SubjectRich:           "{actor} created {file}",
		SubjectRichParameters: `{"actor":{"type":"user","id":"alice","name":"alice"},"file":{"type":"file","id":"2","name":"tok-unknown"},"ncgoNameScheme":{"type":"ncgo","id":"aaaa","name":"1"}}`,
		ObjectName:            "tok-unknown",
	}
	decryptorHandler().decryptSubject(context.Background(), e)
	if e.Subject != "alice created "+EncryptedNamePlaceholder {
		t.Errorf("subject = %q, want the placeholder form", e.Subject)
	}
	if e.ObjectName != EncryptedNamePlaceholder {
		t.Errorf("object name = %q, want the placeholder (never the token)", e.ObjectName)
	}
	if strings.Contains(e.SubjectRichParameters, "tok-unknown") {
		t.Errorf("params keep the token: %q", e.SubjectRichParameters)
	}
}

func TestDecryptSubjectMissingParamKeepsRef(t *testing.T) {
	// A template key with no param stays a literal {key} (the notifications
	// precedent); the sealed file param still decrypts.
	e := &Event{
		Subject:               "alice created tok-file in {folder}",
		SubjectRich:           "{actor} created {file} in {folder}",
		SubjectRichParameters: `{"actor":{"type":"user","id":"alice","name":"alice"},"file":{"type":"file","id":"2","name":"tok-file"},"ncgoNameScheme":{"type":"ncgo","id":"aaaa","name":"1"}}`,
		ObjectName:            "tok-file",
	}
	decryptorHandler().decryptSubject(context.Background(), e)
	if e.Subject != "alice created hello.txt in {folder}" {
		t.Errorf("subject = %q, want the missing param kept as a literal ref", e.Subject)
	}
}
