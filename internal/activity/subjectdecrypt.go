package activity

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
)

// SubjectDecryptor is the ADR-0104 §9 render seam: activity rows whose
// subject_rich_parameters carry the "ncgoNameScheme" marker store a name
// TOKEN in place of the plaintext file name, and the OCS render decrypts it
// in the viewer's ctx. *files.NameTranslator satisfies it; nil keeps every
// row verbatim (filename encryption off — bit-identical).
type SubjectDecryptor interface {
	DecryptSubjectName(ctx context.Context, keyUUIDHex, token string) (string, error)
}

// subjectSchemeParam is the marker key the producer writes into
// subject_rich_parameters for tokenized rows; its rich param carries the
// sealing key's UUID hex as the id and the name scheme as the name ("1" =
// NCGOFN1). It is meta, never referenced by the rich template, and stripped
// before render. The exact key seals the "file" param; a suffixed
// "ncgoNameScheme:<param>" marker seals that param instead (the rename
// event's "oldfile" — the §9 rule applied per sealed param).
const subjectSchemeParam = "ncgoNameScheme"

// EncryptedNamePlaceholder substitutes a name token that cannot be decrypted
// at render time (locked keys, tampered token, pruned wraps) — matching how
// upstream renders dead file references, and the same string the
// notifications render uses. Degradation is per item: the list never fails
// and the token never reaches the wire.
const EncryptedNamePlaceholder = "encrypted file"

// subjectRichRef matches one {key} reference in a SubjectRich template.
var subjectRichRef = regexp.MustCompile(`\{([^{}]+)\}`)

// subjectRichParam is one stored rich-object parameter (type/id/name),
// mirroring the producer's shape.
type subjectRichParam struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

// decryptSubject rewrites a marked activity event to its plaintext render in
// the viewer's ctx: every marker param is stripped from the emitted params,
// each sealed param's Name is decrypted (a failure degrades THAT name to
// EncryptedNamePlaceholder — never the token, never a failed list), Subject
// is rebuilt from the SubjectRich template by substituting each {key} with
// the (post-decryption) param name, and ObjectName takes the decrypted
// "file" name (it stores the token, which §9 keeps off the wire). Unmarked
// rows, unparseable params, and malformed markers pass through verbatim.
func (h Handler) decryptSubject(ctx context.Context, e *Event) {
	if h.Subjects == nil || e == nil || e.SubjectRichParameters == "" {
		return
	}
	params := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(e.SubjectRichParameters), &params); err != nil {
		return // malformed rows keep the payload-time error behavior
	}
	// Collect the markers: the exact key seals "file"; a "<marker>:<param>"
	// key seals <param>. ANY malformed marker leaves the row verbatim.
	sealed := map[string]subjectRichParam{}
	for key, raw := range params {
		var param string
		if key == subjectSchemeParam {
			param = "file"
		} else if rest, ok := strings.CutPrefix(key, subjectSchemeParam+":"); ok {
			param = rest
		} else {
			continue
		}
		var meta subjectRichParam
		if err := json.Unmarshal(raw, &meta); err != nil || meta.Name != "1" || param == "" {
			return // unknown or malformed marker: verbatim
		}
		sealed[param] = meta
	}
	if len(sealed) == 0 {
		return // unmarked: verbatim
	}
	delete(params, subjectSchemeParam)
	for param := range sealed {
		delete(params, subjectSchemeParam+":"+param)
	}

	// Decrypt each sealed param's name token in the viewer's ctx; a failure
	// degrades that one name to the placeholder.
	fileName := ""
	for param, meta := range sealed {
		rawParam, ok := params[param]
		if !ok {
			continue
		}
		var rp subjectRichParam
		if err := json.Unmarshal(rawParam, &rp); err != nil || rp.Name == "" {
			continue
		}
		name, err := h.Subjects.DecryptSubjectName(ctx, meta.ID, rp.Name)
		if err != nil {
			name = EncryptedNamePlaceholder
		}
		rp.Name = name
		if buf, err := json.Marshal(rp); err == nil {
			params[param] = buf
		}
		if param == "file" {
			fileName = name
		}
	}
	if buf, err := json.Marshal(params); err == nil {
		e.SubjectRichParameters = string(buf)
	}
	if fileName != "" {
		e.ObjectName = fileName
	}
	if e.SubjectRich != "" {
		e.Subject = subjectRichRef.ReplaceAllStringFunc(e.SubjectRich, func(ref string) string {
			key := ref[1 : len(ref)-1]
			var rp subjectRichParam
			if raw, ok := params[key]; ok {
				if err := json.Unmarshal(raw, &rp); err == nil {
					return rp.Name
				}
			}
			return ref
		})
	}
}
