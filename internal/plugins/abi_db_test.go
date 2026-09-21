package plugins

import (
	"context"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/plugins/internal/wasmgen"
)

const (
	dbCreateSQL = "CREATE TABLE pt_items (path TEXT)"
	dbInsertSQL = "INSERT INTO pt_items (path) VALUES ('hello-item')"
	dbSelectSQL = "SELECT path FROM pt_items"
)

func dbManifest() *Manifest {
	m := probeManifest()
	m.Capabilities = Capabilities{DB: DBCapabilities{
		Read:  []string{"pt_items"},
		Write: []string{"pt_items"},
	}}
	return m
}

func TestDBHookDDLAndQuery(t *testing.T) {
	h, buf := testHost(t, HostConfig{DB: testDB(t)})
	ctx := context.Background()
	p, err := h.Load(ctx, dbManifest(), wasmgen.DBModule(dbCreateSQL, dbInsertSQL, dbSelectSQL))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	// Install runs in hook context: DDL + INSERT allowed.
	if err := p.Install(ctx); err != nil {
		t.Fatal(err)
	}
	// probe runs outside hooks: SELECT allowed, logs the row and EOF.
	if _, err := p.Call(ctx, "probe"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "hello-item") {
		t.Fatalf("row missing from log: %q", out)
	}
	if !strings.Contains(out, "eof-ok") {
		t.Fatalf("EOF probe missing: %q", out)
	}
	// ddlprobe outside a hook must be denied.
	if _, err := p.Call(ctx, "ddlprobe"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "ddl-ok") {
		t.Fatalf("ddl denial probe missing: %q", buf.String())
	}
}

func TestDBQueryDeniedTable(t *testing.T) {
	h, buf := testHost(t, HostConfig{DB: testDB(t)})
	installModule(t, h, probeManifest(), wasmgen.DBDeniedModule("SELECT id FROM users", ErrCodePermissionDenied))
	if !strings.Contains(buf.String(), "denied-ok") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestDBUnavailable(t *testing.T) {
	h, buf := testHost(t, HostConfig{})
	installModule(t, h, probeManifest(), wasmgen.DBDeniedModule("SELECT 1", ErrCodeUnavailable))
	if !strings.Contains(buf.String(), "denied-ok") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestDBWriteViaQueryRejected(t *testing.T) {
	h, buf := testHost(t, HostConfig{DB: testDB(t)})
	m := dbManifest()
	// db_query with an INSERT statement → invalid argument (-2).
	installModule(t, h, m, wasmgen.DBDeniedModule("INSERT INTO pt_items (path) VALUES ('x')", ErrCodeInvalidArgument))
	if !strings.Contains(buf.String(), "denied-ok") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestDBUnparseableSQLRejected(t *testing.T) {
	h, buf := testHost(t, HostConfig{DB: testDB(t)})
	m := dbManifest()
	installModule(t, h, m, wasmgen.DBDeniedModule("SELECT 1; DROP TABLE pt_items", ErrCodeInvalidArgument))
	if !strings.Contains(buf.String(), "denied-ok") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestDBTransaction(t *testing.T) {
	db := testDB(t)
	if _, err := db.Exec(context.Background(), dbCreateSQL); err != nil {
		t.Fatal(err)
	}
	h, buf := testHost(t, HostConfig{DB: db})
	installModule(t, h, dbManifest(), wasmgen.DBTxModule(
		"INSERT INTO pt_items (path) VALUES ('tx-item')",
		"SELECT path FROM pt_items",
	))
	out := buf.String()
	if !strings.Contains(out, "tx-item") {
		t.Fatalf("tx row missing from log: %q", out)
	}
	if !strings.Contains(out, "rb-ok") {
		t.Fatalf("rollback probe missing: %q", out)
	}
	// tx1 committed one row; tx2 was rolled back.
	var n int64
	if err := db.QueryRow(context.Background(), "SELECT COUNT(*) FROM pt_items").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("rows = %d, want 1 (tx2 must be rolled back)", n)
	}
}
