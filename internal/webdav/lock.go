package webdav

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	defaultLockTimeout = 1800 * time.Second
	maxLockTimeout     = 86400 * time.Second
)

func (h *Handler) lock(w http.ResponseWriter, r *http.Request) {
	user, sub, ok := h.authorizePath(w, r)
	if !ok {
		return
	}
	lf, ok := h.FS.(LockFS)
	if !ok {
		h.methodNotAllowed(w, r)
		return
	}
	depth := strings.TrimSpace(r.Header.Get(HeaderDepth))
	if depth != "" && depth != "0" && !strings.EqualFold(depth, "infinity") {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	ifHeader := r.Header.Get(HeaderIf)
	req := LockRequest{
		Timeout:       parseTimeoutHeader(r.Header.Get(HeaderTimeout)),
		Token:         ifHeader,
		DepthInfinity: strings.EqualFold(depth, "infinity"),
	}
	if len(bytes.TrimSpace(body)) == 0 {
		if len(ExtractLockTokens(ifHeader)) == 0 {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		req.Refresh = true
	} else {
		parsed, perr := parseLockInfo(bytes.NewReader(body))
		if perr != nil {
			writeFSError(w, ErrBadRequest)
			return
		}
		if parsed.shared {
			writeFSError(w, ErrForbidden)
			return
		}
		req.Owner = parsed.owner
	}
	info, err := lf.Lock(r.Context(), user, sub, req)
	if err != nil {
		writeFSError(w, err)
		return
	}
	href := h.lockRootHref(user, info.Path)
	var buf bytes.Buffer
	writeLockProp(&buf, info, href)
	w.Header().Set("Content-Type", contentTypeXML)
	w.Header().Set(HeaderLockToken, "<"+info.Token+">")
	w.Header().Set(HeaderTimeout, timeoutHeaderValue(info.Timeout))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

func (h *Handler) unlock(w http.ResponseWriter, r *http.Request) {
	user, sub, ok := h.authorizePath(w, r)
	if !ok {
		return
	}
	lf, ok := h.FS.(LockFS)
	if !ok {
		h.methodNotAllowed(w, r)
		return
	}
	raw := r.Header.Get(HeaderLockToken)
	if strings.TrimSpace(raw) == "" {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	if err := lf.Unlock(r.Context(), user, sub, StripLockToken(raw)); err != nil {
		writeFSError(w, err)
		return
	}
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) checkLock(w http.ResponseWriter, r *http.Request, user, p string) bool {
	lf, ok := h.FS.(LockFS)
	if !ok {
		return true
	}
	if err := lf.CheckLock(r.Context(), user, p, r.Header.Get(HeaderIf)); err != nil {
		writeFSError(w, err)
		return false
	}
	return true
}

func (h *Handler) lockRootHref(user, p string) string {
	base := h.hrefPrefix(user)
	np := normalizePath(p)
	if np == "/" {
		if !strings.HasSuffix(base, "/") {
			return base + "/"
		}
		return base
	}
	return strings.TrimRight(base, "/") + np
}

type parsedLockInfo struct {
	owner  string
	shared bool
}

func parseLockInfo(r io.Reader) (*parsedLockInfo, error) {
	dec := xml.NewDecoder(r)
	var info parsedLockInfo
	var sawLock, sawScope, sawType bool
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, ErrBadRequest
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		name := strings.ToLower(se.Name.Local)
		switch name {
		case "lockinfo":
			sawLock = true
		case "exclusive":
			sawScope = true
			info.shared = false
		case "shared":
			sawScope = true
			info.shared = true
		case "write":
			sawType = true
		case "owner":
			var owner string
			if err := dec.DecodeElement(&owner, &se); err != nil {
				return nil, ErrBadRequest
			}
			info.owner = strings.TrimSpace(owner)
		}
	}
	if !sawLock || !sawScope || !sawType {
		return nil, ErrBadRequest
	}
	return &info, nil
}

func parseTimeoutHeader(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" || strings.EqualFold(v, "Infinite") {
		return defaultLockTimeout
	}
	first, _, _ := strings.Cut(v, ",")
	first = strings.TrimSpace(first)
	if strings.EqualFold(first, "Infinite") {
		return defaultLockTimeout
	}
	low := strings.ToLower(first)
	if !strings.HasPrefix(low, "second-") {
		return defaultLockTimeout
	}
	n, err := strconv.ParseInt(first[len("Second-"):], 10, 64)
	if err != nil || n <= 0 {
		return defaultLockTimeout
	}
	d := time.Duration(n) * time.Second
	if d > maxLockTimeout {
		return maxLockTimeout
	}
	return d
}

func timeoutHeaderValue(d time.Duration) string {
	sec := int(d / time.Second)
	if sec < 0 {
		sec = 0
	}
	return "Second-" + strconv.Itoa(sec)
}

// StripLockToken trims angle brackets from a Lock-Token header value.
func StripLockToken(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "<")
	v = strings.TrimSuffix(v, ">")
	return strings.TrimSpace(v)
}

// ExtractLockTokens returns opaquelocktoken values from an If or Lock-Token header.
func ExtractLockTokens(header string) []string {
	var out []string
	rest := header
	for {
		start := strings.Index(rest, "<")
		if start < 0 {
			break
		}
		endRel := strings.Index(rest[start:], ">")
		if endRel < 0 {
			break
		}
		end := start + endRel
		tok := strings.TrimSpace(rest[start+1 : end])
		rest = rest[end+1:]
		if strings.HasPrefix(tok, "opaquelocktoken:") {
			out = append(out, tok)
		}
	}
	return out
}

func writeLockProp(buf *bytes.Buffer, info *LockInfo, href string) {
	buf.WriteString(`<?xml version="1.0"?>` + "\n")
	buf.WriteString(`<d:prop xmlns:d="DAV:">`)
	writeLockDiscovery(buf, info, href)
	buf.WriteString(`</d:prop>`)
}

func writeLockDiscovery(buf *bytes.Buffer, info *LockInfo, href string) {
	if info == nil || info.Token == "" {
		buf.WriteString(`<d:lockdiscovery/>`)
		return
	}
	buf.WriteString(`<d:lockdiscovery><d:activelock>`)
	buf.WriteString(`<d:locktype><d:write/></d:locktype>`)
	buf.WriteString(`<d:lockscope><d:exclusive/></d:lockscope>`)
	if info.DepthInfinity {
		buf.WriteString(`<d:depth>infinity</d:depth>`)
	} else {
		buf.WriteString(`<d:depth>0</d:depth>`)
	}
	fmt.Fprintf(buf, `<d:owner>%s</d:owner>`, xmlEscape(info.Owner))
	fmt.Fprintf(buf, `<d:timeout>%s</d:timeout>`, timeoutHeaderValue(info.Timeout))
	fmt.Fprintf(buf, `<d:locktoken><d:href>%s</d:href></d:locktoken>`, xmlEscape(info.Token))
	if href != "" {
		fmt.Fprintf(buf, `<d:lockroot><d:href>%s</d:href></d:lockroot>`, xmlEscape(href))
	}
	buf.WriteString(`</d:activelock></d:lockdiscovery>`)
}

func writeSupportedLock(buf *bytes.Buffer) {
	buf.WriteString(`<d:supportedlock><d:lockentry><d:lockscope><d:exclusive/></d:lockscope><d:locktype><d:write/></d:locktype></d:lockentry></d:supportedlock>`)
}

func (fs *InMemoryFS) Lock(ctx context.Context, user, p string, req LockRequest) (*LockInfo, error) {
	np := normalizePath(p)
	if _, err := fs.Stat(ctx, user, np); err != nil {
		return nil, err
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultLockTimeout
	}
	if timeout > maxLockTimeout {
		timeout = maxLockTimeout
	}
	now := fs.clock().UTC()
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.expireMemLockLocked(user, np, now)
	if existing := fs.memLockLocked(user, np); existing != nil {
		if lockHeaderHasToken(req.Token, existing.Token) {
			existing.Timeout = now.Add(timeout)
			return memLockInfo(np, existing, now), nil
		}
		return nil, ErrLocked
	}
	if req.Refresh {
		return nil, ErrPrecondition
	}
	token, err := randomLockToken()
	if err != nil {
		return nil, err
	}
	lk := &memLock{Token: token, Owner: req.Owner, Timeout: now.Add(timeout), Created: now, DepthInfinity: req.DepthInfinity}
	if fs.locks[user] == nil {
		fs.locks[user] = make(map[string]*memLock)
	}
	fs.locks[user][np] = lk
	return memLockInfo(np, lk, now), nil
}

func (fs *InMemoryFS) Unlock(ctx context.Context, user, p, token string) error {
	np := normalizePath(p)
	if _, err := fs.Stat(ctx, user, np); err != nil {
		return err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	now := fs.clock().UTC()
	fs.expireMemLockLocked(user, np, now)
	existing := fs.memLockLocked(user, np)
	if existing == nil {
		return ErrConflict
	}
	if existing.Token != StripLockToken(token) {
		return ErrConflict
	}
	delete(fs.locks[user], np)
	return nil
}

func (fs *InMemoryFS) CheckLock(_ context.Context, user, p, ifHeader string) error {
	np := normalizePath(p)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	now := fs.clock().UTC()
	for cur := np; ; cur = parentPath(cur) {
		fs.expireMemLockLocked(user, cur, now)
		existing := fs.memLockLocked(user, cur)
		if existing != nil && (cur == np || existing.DepthInfinity) {
			if lockHeaderHasToken(ifHeader, existing.Token) {
				return nil
			}
			return ErrLocked
		}
		if cur == "/" {
			return nil
		}
	}
}

func (fs *InMemoryFS) memLockLocked(user, p string) *memLock {
	m := fs.locks[user]
	if m == nil {
		return nil
	}
	return m[p]
}

func (fs *InMemoryFS) expireMemLockLocked(user, p string, now time.Time) {
	m := fs.locks[user]
	if m == nil {
		return
	}
	lk := m[p]
	if lk == nil {
		return
	}
	if !lk.Timeout.After(now) {
		delete(m, p)
	}
}

func (fs *InMemoryFS) applyMemLockLocked(user, p string, e *Entry) {
	if e == nil {
		return
	}
	lk := fs.memLockLocked(user, p)
	if lk == nil {
		e.LockToken = ""
		e.LockOwner = ""
		e.LockTimeout = 0
		return
	}
	now := fs.clock().UTC()
	if !lk.Timeout.After(now) {
		e.LockToken = ""
		e.LockOwner = ""
		e.LockTimeout = 0
		return
	}
	e.LockToken = lk.Token
	e.LockOwner = lk.Owner
	e.LockTimeout = lk.Timeout.Sub(now)
}

func (fs *InMemoryFS) deleteMemLocksLocked(user, p string) {
	m := fs.locks[user]
	if m == nil {
		return
	}
	delete(m, p)
	prefix := p + "/"
	if p == "/" {
		prefix = "/"
	}
	for fp := range m {
		if p != "/" && strings.HasPrefix(fp, prefix) {
			delete(m, fp)
		}
		if p == "/" && fp != "/" {
			delete(m, fp)
		}
	}
}

func (fs *InMemoryFS) renameMemLocksLocked(user, src, dst string) {
	m := fs.locks[user]
	if m == nil {
		return
	}
	type pair struct {
		from, to string
		lk       *memLock
	}
	var moves []pair
	for fp, lk := range m {
		var next string
		switch {
		case fp == src:
			next = dst
		case strings.HasPrefix(fp, src+"/"):
			next = dst + strings.TrimPrefix(fp, src)
		default:
			continue
		}
		moves = append(moves, pair{from: fp, to: next, lk: lk})
	}
	for _, mv := range moves {
		delete(m, mv.from)
	}
	for _, mv := range moves {
		m[mv.to] = mv.lk
	}
}

func memLockInfo(path string, lk *memLock, now time.Time) *LockInfo {
	rem := lk.Timeout.Sub(now)
	if rem < 0 {
		rem = 0
	}
	return &LockInfo{Token: lk.Token, Owner: lk.Owner, Timeout: rem, Path: path, DepthInfinity: lk.DepthInfinity}
}

func parentPath(p string) string {
	p = strings.TrimSuffix(p, "/")
	if p == "" || p == "/" {
		return "/"
	}
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return "/"
	}
	return p[:i]
}

func randomLockToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "opaquelocktoken:" + hex.EncodeToString(b[:]), nil
}

func lockHeaderHasToken(header, token string) bool {
	token = StripLockToken(token)
	if token == "" {
		return false
	}
	for _, t := range ExtractLockTokens(header) {
		if t == token {
			return true
		}
	}
	return StripLockToken(header) == token
}
