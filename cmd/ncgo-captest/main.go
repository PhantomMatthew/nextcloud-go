package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/PhantomMatthew/nextcloud-go/internal/app"
	"github.com/PhantomMatthew/nextcloud-go/internal/goldentest"
	"github.com/PhantomMatthew/nextcloud-go/internal/observability"
)

func main() {
	if err := newRoot().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "ncgo-captest",
		Short:         "Golden-case replay runner",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newRun(), newVersion())
	return root
}

func newVersion() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version",
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "ncgo-captest %s (commit %s, built %s)\n",
				observability.Version, observability.Commit, observability.BuildDate)
		},
	}
}

func newRun() *cobra.Command {
	var (
		casesDir string
		baseURL  string
		tags     []string
		replay   bool
		asJSON   bool
	)
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Replay golden cases",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCases(cmd, casesDir, baseURL, tags, replay, asJSON)
		},
	}
	cmd.Flags().StringVar(&casesDir, "cases", "", "golden cases root")
	cmd.Flags().StringVar(&baseURL, "base-url", "", "live server URL (empty = in-process)")
	cmd.Flags().StringSliceVar(&tags, "tag", nil, "only cases with this tag (repeatable)")
	cmd.Flags().BoolVar(&replay, "replayable-only", false, "skip non-replayable cases")
	cmd.Flags().BoolVar(&asJSON, "json", false, "JSON report")
	if err := cmd.MarkFlagRequired("cases"); err != nil {
		return cmd
	}
	return cmd
}

type caseResult struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

func runCases(cmd *cobra.Command, casesDir, baseURL string, tags []string, replayOnly, asJSON bool) error {
	dirs, err := goldentest.Discover(casesDir)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	var handler http.Handler
	var maint http.Handler
	if baseURL == "" {
		a, err := app.New(ctx, app.DevConfig(), logger)
		if err != nil {
			return err
		}
		defer func() { _ = a.Close(ctx) }()
		handler = a.Handler()
		mcfg := app.DevConfig()
		mcfg.Database.DSN = "file:ncgo-captest-maint?mode=memory&cache=shared"
		mcfg.Maintenance.Enabled = true
		ma, err := app.New(ctx, mcfg, logger)
		if err != nil {
			return err
		}
		defer func() { _ = ma.Close(ctx) }()
		maint = ma.Handler()
	}
	var results []caseResult
	fail := 0
	for _, dir := range dirs {
		c, err := goldentest.Load(dir)
		if err != nil {
			return err
		}
		if replayOnly && !c.Replayable {
			results = append(results, caseResult{ID: c.ID, Status: "SKIP"})
			continue
		}
		if len(tags) > 0 && !hasAnyTag(c.Tags, tags) {
			results = append(results, caseResult{ID: c.ID, Status: "SKIP"})
			continue
		}
		res := replayOne(ctx, c, baseURL, handler, maint)
		results = append(results, res)
		if res.Status == "FAIL" {
			fail++
		}
	}
	if asJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		if err := enc.Encode(results); err != nil {
			return err
		}
	} else {
		for _, r := range results {
			if r.Error != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "%s %s %s\n", r.Status, r.ID, r.Error)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", r.Status, r.ID)
			}
		}
		fmt.Fprintf(cmd.OutOrStdout(), "summary pass=%d fail=%d skip=%d\n",
			countStatus(results, "PASS"), fail, countStatus(results, "SKIP"))
	}
	if fail > 0 {
		return fmt.Errorf("ncgo-captest: %d failed", fail)
	}
	return nil
}

func replayOne(ctx context.Context, c *goldentest.Case, baseURL string, handler, maint http.Handler) caseResult {
	want, err := goldentest.ParseResponse(c.ResponseRaw)
	if err != nil {
		return caseResult{ID: c.ID, Status: "FAIL", Error: err.Error()}
	}
	var got *goldentest.ParsedResponse
	if baseURL != "" {
		parsed, err := goldentest.ParseRequest(c.RequestRaw)
		if err != nil {
			return caseResult{ID: c.ID, Status: "FAIL", Error: err.Error()}
		}
		got, err = goldentest.Execute(ctx, c, func(_ *http.Request) (*http.Response, error) {
			req, err := http.NewRequestWithContext(ctx, parsed.Method, strings.TrimRight(baseURL, "/")+parsed.Path, strings.NewReader(string(parsed.Body)))
			if err != nil {
				return nil, err
			}
			for k, vs := range parsed.Headers {
				for _, v := range vs {
					req.Header.Add(k, v)
				}
			}
			return http.DefaultClient.Do(req)
		})
		if err != nil {
			return caseResult{ID: c.ID, Status: "FAIL", Error: err.Error()}
		}
	} else {
		h := handler
		if slices.Contains(c.Tags, "maintenance") && maint != nil {
			h = maint
		}
		got, err = goldentest.Execute(ctx, c, func(req *http.Request) (*http.Response, error) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			return rec.Result(), nil
		})
		if err != nil {
			return caseResult{ID: c.ID, Status: "FAIL", Error: err.Error()}
		}
	}
	if err := goldentest.Compare(c, want, got); err != nil {
		return caseResult{ID: c.ID, Status: "FAIL", Error: err.Error()}
	}
	return caseResult{ID: c.ID, Status: "PASS"}
}

func hasAnyTag(have, want []string) bool {
	for _, w := range want {
		if slices.Contains(have, w) {
			return true
		}
	}
	return false
}

func countStatus(rs []caseResult, st string) int {
	n := 0
	for _, r := range rs {
		if r.Status == st {
			n++
		}
	}
	return n
}
