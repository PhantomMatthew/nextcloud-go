package goldentest

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Discover returns sorted directories that contain a case.yaml.
func Discover(root string) ([]string, error) {
	if root == "" {
		return nil, fmt.Errorf("goldentest: empty root")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	var dirs []string
	err = filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		base := d.Name()
		if base == "_schema" || strings.HasPrefix(base, ".") {
			if path != abs {
				return fs.SkipDir
			}
		}
		if _, err := os.Stat(filepath.Join(path, "case.yaml")); err == nil {
			dirs = append(dirs, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(dirs)
	return dirs, nil
}

type yamlCase struct {
	ID         string   `yaml:"id"`
	Replayable bool     `yaml:"replayable"`
	Synthetic  bool     `yaml:"synthetic"`
	Tags       []string `yaml:"tags"`
	Response   struct {
		Normalize []yamlNorm `yaml:"normalize"`
	} `yaml:"response"`
}

type yamlNorm struct {
	DropHeaders   []string `yaml:"drop_headers"`
	ReplaceHeader *struct {
		Name string `yaml:"name"`
		With string `yaml:"with"`
	} `yaml:"replace_header"`
}

func applyYAML(c *Case, dir string) error {
	raw, err := os.ReadFile(filepath.Join(dir, "case.yaml"))
	if err != nil {
		return err
	}
	var y yamlCase
	if err := yaml.Unmarshal(raw, &y); err != nil {
		return fmt.Errorf("goldentest: parse case.yaml: %w", err)
	}
	if y.ID != "" {
		c.ID = y.ID
	}
	c.Replayable = y.Replayable
	c.Synthetic = y.Synthetic
	c.Tags = y.Tags
	for _, n := range y.Response.Normalize {
		rule := NormalizeRule{DropHeaders: n.DropHeaders}
		if n.ReplaceHeader != nil {
			rule.ReplaceHeader = &ReplaceHeaderRule{Name: n.ReplaceHeader.Name, With: n.ReplaceHeader.With}
		}
		c.Response.Normalize = append(c.Response.Normalize, rule)
	}
	c.Response.Normalize = append(c.Response.Normalize, NormalizeRule{
		DropHeaders: []string{
			"Content-Length",
			"Feature-Policy",
			"Referrer-Policy",
			"X-Content-Type-Options",
			"X-Frame-Options",
			"X-Permitted-Cross-Domain-Policies",
			"X-Robots-Tag",
			"X-Xss-Protection",
			"X-XSS-Protection",
			"Etag",
			"ETag",
			"Access-Control-Allow-Origin",
			"Retry-After",
			"Www-Authenticate",
		},
	})
	return nil
}
