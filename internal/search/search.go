// Package search serves the OCS unified-search API (files and mail
// providers).
package search

import (
	"context"
	"strings"

	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

// Provider is one unified-search backend.
type Provider interface {
	ID() string
	Name() string
	Search(ctx context.Context, uid, term string, limit int) ([]Hit, error)
}

// Hit is one search result. ResourceURL, when set, is an absolute path on
// this server (e.g. the mail provider's message API path) and the handler
// renders it under the request base; when empty the files provider's
// fileID-derived pretty URL (/index.php/f/{id}) is used.
type Hit struct {
	Title        string
	ResourceURL  string
	Subline      string
	ThumbnailURL string
	fileID       int64
}

// FilesProvider searches the current user's filecache by basename.
type FilesProvider struct {
	Meta  files.Store
	Users users.Store
}

// NewFilesProvider returns the files unified-search provider.
func NewFilesProvider(meta files.Store, users users.Store) *FilesProvider {
	return &FilesProvider{Meta: meta, Users: users}
}

func (p *FilesProvider) ID() string   { return "files" }
func (p *FilesProvider) Name() string { return "Files" }

func (p *FilesProvider) Search(ctx context.Context, uid, term string, limit int) ([]Hit, error) {
	if p == nil || p.Meta == nil || p.Users == nil {
		return nil, nil
	}
	if strings.TrimSpace(term) == "" {
		return nil, nil
	}
	u, err := p.Users.GetByUID(ctx, uid)
	if err != nil {
		return nil, err
	}
	found, err := p.Meta.SearchByName(ctx, u.ID, term, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Hit, 0, len(found))
	for i := range found {
		f := found[i]
		out = append(out, Hit{
			Title:   f.Name,
			Subline: f.Path,
			fileID:  f.ID,
		})
	}
	return out, nil
}
