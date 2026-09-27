package notifications

import (
	"context"
	"encoding/json"
	"regexp"
)

// SubjectDecryptor is the ADR-0104 §9 (phase 3b) render seam: notification
// rows whose subject_rich_parameters carry the "ncgoNameScheme" marker store
// a name TOKEN in place of the plaintext share name, and the OCS render
// decrypts it in the viewer's ctx. *files.NameTranslator satisfies it; nil
// keeps every row verbatim (filename encryption off — bit-identical).
type SubjectDecryptor interface {
	DecryptSubjectName(ctx context.Context, keyUUIDHex, token string) (string, error)
}

// subjectSchemeParam is the marker key the producer writes into
// subject_rich_parameters for tokenized rows; its rich param carries the
// sealing key's UUID hex as the id and the name scheme as the name ("1" =
// NCGOFN1). It is meta, never referenced by the rich template, and stripped
// before render.
const subjectSchemeParam = "ncgoNameScheme"

// EncryptedNamePlaceholder substitutes a name token that cannot be decrypted
// at render time (locked keys, tampered token, pruned wraps) — matching how
// upstream renders dead file references. Degradation is per item: the list
// never fails and the token never reaches the wire.
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

// decryptSubject rewrites a marked notification to its plaintext render in
// the viewer's ctx: the marker param is stripped from the emitted params,
// params["share"].Name becomes the decrypted name, and Subject is rebuilt
// from the SubjectRich template by substituting each {key} with the
// (post-decryption) param name. ANY decrypt failure degrades the item to
// EncryptedNamePlaceholder — never the token, never a failed list. Unmarked
// or unparseable rows pass through verbatim.
func (h Handler) decryptSubject(ctx context.Context, n *Notification) {
	if h.Subjects == nil || n == nil || n.SubjectRichParameters == "" {
		return
	}
	params := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(n.SubjectRichParameters), &params); err != nil {
		return // malformed rows keep the payload-time error behavior
	}
	raw, ok := params[subjectSchemeParam]
	if !ok {
		return // unmarked: verbatim
	}
	var meta subjectRichParam
	if err := json.Unmarshal(raw, &meta); err != nil || meta.Name != "1" {
		return // unknown or malformed marker: verbatim
	}
	delete(params, subjectSchemeParam)

	token := ""
	var shareParam subjectRichParam
	if rawShare, ok := params["share"]; ok {
		if err := json.Unmarshal(rawShare, &shareParam); err == nil {
			token = shareParam.Name
		}
	}
	name, err := h.Subjects.DecryptSubjectName(ctx, meta.ID, token)
	if err != nil {
		name = EncryptedNamePlaceholder
	}
	if token != "" {
		shareParam.Name = name
		if buf, err := json.Marshal(shareParam); err == nil {
			params["share"] = buf
		}
	}
	if buf, err := json.Marshal(params); err == nil {
		n.SubjectRichParameters = string(buf)
	}
	if n.SubjectRich != "" {
		n.Subject = subjectRichRef.ReplaceAllStringFunc(n.SubjectRich, func(ref string) string {
			key := ref[1 : len(ref)-1]
			if key == "share" {
				return name
			}
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
