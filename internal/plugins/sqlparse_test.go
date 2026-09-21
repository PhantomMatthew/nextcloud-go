package plugins

import (
	"reflect"
	"testing"
)

func TestParsePluginSQL(t *testing.T) {
	cases := []struct {
		name   string
		sql    string
		kind   stmtKind
		tables []string
	}{
		{"select", "SELECT id, name FROM users WHERE id = ?", stmtSelect, []string{"users"}},
		{"select join", "SELECT * FROM a JOIN b ON a.id = b.id", stmtSelect, []string{"a", "b"}},
		{"select subquery", "SELECT * FROM a WHERE id IN (SELECT id FROM b)", stmtSelect, []string{"a", "b"}},
		{"select alias", "SELECT a.id FROM users a", stmtSelect, []string{"users"}},
		{"insert", "INSERT INTO tags (file, tag) VALUES (?, ?)", stmtWrite, []string{"tags"}},
		{"insert select", "INSERT INTO dst SELECT * FROM src", stmtWrite, []string{"dst", "src"}},
		{"update", "UPDATE files SET mtime = 1 WHERE id = ?", stmtWrite, []string{"files"}},
		{"delete", "DELETE FROM sessions WHERE id = ?", stmtWrite, []string{"sessions"}},
		{"create table", "CREATE TABLE pt_items (path TEXT)", stmtDDL, []string{"pt_items"}},
		{"drop table", "DROP TABLE pt_items", stmtDDL, []string{"pt_items"}},
		{"set", "SET NAMES utf8", stmtOther, nil},
		{"show", "SHOW TABLES", stmtOther, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, tables, err := parsePluginSQL(tc.sql)
			if err != nil {
				t.Fatal(err)
			}
			if kind != tc.kind {
				t.Errorf("kind = %d, want %d", kind, tc.kind)
			}
			if !reflect.DeepEqual(tables, tc.tables) {
				t.Errorf("tables = %v, want %v", tables, tc.tables)
			}
		})
	}
}

func TestParsePluginSQLRejected(t *testing.T) {
	for _, sql := range []string{
		"SELECT 1; DROP TABLE users",
		"NOT SQL AT ALL",
		"",
	} {
		if _, _, err := parsePluginSQL(sql); err == nil {
			t.Errorf("%q: expected error", sql)
		}
	}
}

func TestCheckDBTables(t *testing.T) {
	caps := &Capabilities{DB: DBCapabilities{
		Read:  []string{"calendar_*", "tags"},
		Write: []string{"tags"},
	}}
	if !checkDBTables(caps, stmtSelect, []string{"calendar_events"}) {
		t.Error("calendar read should pass")
	}
	if checkDBTables(caps, stmtSelect, []string{"users"}) {
		t.Error("users read should be denied")
	}
	if !checkDBTables(caps, stmtWrite, []string{"tags"}) {
		t.Error("tags write should pass")
	}
	if checkDBTables(caps, stmtWrite, []string{"calendar_events"}) {
		t.Error("calendar write should be denied (read-only grant)")
	}
	if checkDBTables(caps, stmtSelect, []string{"tags", "users"}) {
		t.Error("any denied table fails the whole statement")
	}
}
