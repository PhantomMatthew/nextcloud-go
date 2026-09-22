package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/migrations"
)

func testDB(t *testing.T) database.DB {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, database.Config{
		Driver: database.DialectSQLite,
		DSN:    "file:" + t.Name() + "?mode=memory&cache=shared",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	std, ok := database.Unwrap(db)
	if !ok {
		t.Fatal("unwrap")
	}
	if _, err := migrations.Up(ctx, std, database.DialectSQLite, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestRegistryCRUD(t *testing.T) {
	ctx := context.Background()
	reg := NewRegistry(testDB(t))

	row := &RegistryRow{
		ID:             "com.example.a",
		Version:        "1.0.0",
		Enabled:        true,
		Capabilities:   json.RawMessage(`{"db":{"read":["t"]}}`),
		SignatureKeyID: "abc123",
		ArchivePath:    "/plugins/com.example.a/1.0.0.ncplugin",
	}
	if err := reg.Upsert(ctx, row); err != nil {
		t.Fatal(err)
	}
	got, err := reg.Get(ctx, "com.example.a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "1.0.0" || !got.Enabled || got.SignatureKeyID != "abc123" {
		t.Fatalf("got = %+v", got)
	}

	// Upsert updates the version and preserves the record.
	row.Version = "1.1.0"
	row.Enabled = false
	if err := reg.Upsert(ctx, row); err != nil {
		t.Fatal(err)
	}
	got, err = reg.Get(ctx, "com.example.a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "1.1.0" || got.Enabled {
		t.Fatalf("got = %+v", got)
	}

	// Enabled filtering.
	if err := reg.Upsert(ctx, &RegistryRow{ID: "com.example.b", Version: "0.1.0", Enabled: true, ArchivePath: "/x"}); err != nil {
		t.Fatal(err)
	}
	enabled, err := reg.ListEnabled(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(enabled) != 1 || enabled[0].ID != "com.example.b" {
		t.Fatalf("enabled = %+v", enabled)
	}

	// SetEnabled.
	if err := reg.SetEnabled(ctx, "com.example.a", true); err != nil {
		t.Fatal(err)
	}
	enabled, err = reg.ListEnabled(ctx)
	if err != nil || len(enabled) != 2 {
		t.Fatalf("enabled = %+v %v", enabled, err)
	}
	if err := reg.SetEnabled(ctx, "com.example.missing", true); !errors.Is(err, ErrPluginNotFound) {
		t.Fatalf("err = %v", err)
	}

	// List ordering.
	all, err := reg.List(ctx)
	if err != nil || len(all) != 2 || all[0].ID != "com.example.a" {
		t.Fatalf("all = %+v %v", all, err)
	}

	// Delete.
	if err := reg.Delete(ctx, "com.example.a"); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Get(ctx, "com.example.a"); !errors.Is(err, ErrPluginNotFound) {
		t.Fatalf("err = %v", err)
	}
	if err := reg.Delete(ctx, "com.example.a"); !errors.Is(err, ErrPluginNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestRegistryRoutes(t *testing.T) {
	ctx := context.Background()
	reg := NewRegistry(testDB(t))

	rec := &RouteRecord{PluginID: "com.example.a", Kind: "route", Method: "GET", Path: "/apps/com.example.a/api", HandlerName: "handleV1"}
	if err := reg.UpsertRoute(ctx, rec); err != nil {
		t.Fatal(err)
	}
	recs, err := reg.RoutesForPlugin(ctx, "com.example.a")
	if err != nil || len(recs) != 1 || recs[0].HandlerName != "handleV1" {
		t.Fatalf("recs = %+v %v", recs, err)
	}

	// Same key upserts the handler name.
	rec.HandlerName = "handleV2"
	if err := reg.UpsertRoute(ctx, rec); err != nil {
		t.Fatal(err)
	}
	recs, err = reg.RoutesForPlugin(ctx, "com.example.a")
	if err != nil || len(recs) != 1 || recs[0].HandlerName != "handleV2" {
		t.Fatalf("recs = %+v %v", recs, err)
	}

	// Second kind and second plugin.
	if err := reg.UpsertRoute(ctx, &RouteRecord{PluginID: "com.example.a", Kind: "ocs", Method: "POST", Path: "/apps/com.example.a/ocs", HandlerName: "handleOCS"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.UpsertRoute(ctx, &RouteRecord{PluginID: "com.example.b", Kind: "route", Method: "GET", Path: "/apps/com.example.b/x", HandlerName: "h"}); err != nil {
		t.Fatal(err)
	}
	recs, err = reg.RoutesForPlugin(ctx, "com.example.a")
	if err != nil || len(recs) != 2 || recs[0].Kind != "ocs" || recs[1].Kind != "route" {
		t.Fatalf("recs = %+v %v", recs, err)
	}
	all, err := reg.AllRoutes(ctx)
	if err != nil || len(all) != 3 || all[0].PluginID != "com.example.a" || all[2].PluginID != "com.example.b" {
		t.Fatalf("all = %+v %v", all, err)
	}

	// Delete is scoped to one plugin and tolerates none.
	if err := reg.DeleteRoutesForPlugin(ctx, "com.example.a"); err != nil {
		t.Fatal(err)
	}
	recs, err = reg.RoutesForPlugin(ctx, "com.example.a")
	if err != nil || len(recs) != 0 {
		t.Fatalf("recs = %+v %v", recs, err)
	}
	all, err = reg.AllRoutes(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("all = %+v %v", all, err)
	}
	if err := reg.DeleteRoutesForPlugin(ctx, "com.example.missing"); err != nil {
		t.Fatal(err)
	}
}
