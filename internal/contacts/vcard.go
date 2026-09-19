package contacts

import (
	"crypto/sha1" //nolint:gosec // non-cryptographic: ETag fingerprint
	"encoding/hex"
	"fmt"
	"strings"
)

type parsedCard struct {
	UID string
	FN  string
}

func parseVCard(data []byte) (*parsedCard, error) {
	text := unfoldICS(string(data))
	n := strings.Count(text, "BEGIN:VCARD")
	if n != 1 {
		return nil, fmt.Errorf("%w: want exactly one VCARD", ErrInvalid)
	}
	block := vcardBlock(text)
	props := parseProps(block)
	uid := props["UID"]
	if uid == "" {
		return nil, fmt.Errorf("%w: missing UID", ErrInvalid)
	}
	return &parsedCard{UID: uid, FN: props["FN"]}, nil
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

func vcardBlock(text string) string {
	start := strings.Index(text, "BEGIN:VCARD")
	end := strings.Index(text, "END:VCARD")
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
		name, value, ok := splitProp(line)
		if !ok {
			continue
		}
		if _, exists := out[name]; !exists {
			out[name] = value
		}
	}
	return out
}

func splitProp(line string) (name, value string, ok bool) {
	colon := strings.IndexByte(line, ':')
	if colon < 0 {
		return "", "", false
	}
	head := line[:colon]
	value = line[colon+1:]
	name = strings.ToUpper(strings.Split(head, ";")[0])
	return name, value, true
}
