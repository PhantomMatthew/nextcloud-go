package plugins

import (
	"fmt"
	"sort"

	"github.com/xwb1989/sqlparser"
)

// stmtKind classifies a plugin SQL statement for capability checks.
type stmtKind int

const (
	stmtSelect stmtKind = iota // SELECT — needs db.read
	stmtWrite                  // INSERT/UPDATE/DELETE — needs db.write
	stmtDDL                    // CREATE/ALTER/DROP/RENAME — db.write + hook context
	stmtOther                  // SET/SHOW/USE/... — always rejected
)

// parsePluginSQL parses exactly one statement, classifies it, and extracts
// every referenced table name. Parse failures are rejected (fail closed):
// the table allowlist is the security boundary.
func parsePluginSQL(query string) (stmtKind, []string, error) {
	stmt, err := sqlparser.Parse(query)
	if err != nil {
		return stmtOther, nil, fmt.Errorf("plugins: sql parse: %w", err)
	}
	var kind stmtKind
	switch stmt.(type) {
	case *sqlparser.Select:
		kind = stmtSelect
	case *sqlparser.Insert, *sqlparser.Update, *sqlparser.Delete:
		kind = stmtWrite
	case *sqlparser.DDL:
		kind = stmtDDL
	default:
		// SET/SHOW/USE etc. parse fine but are never allowed.
		return stmtOther, nil, nil
	}

	var tables []string
	seen := make(map[string]bool)
	// Walk the whole statement so subqueries in WHERE/SELECT are covered.
	// ColName nodes are not descended into: their qualifier is a column
	// qualifier (possibly an alias), not a table reference.
	err = sqlparser.Walk(func(node sqlparser.SQLNode) (bool, error) {
		switch n := node.(type) {
		case *sqlparser.ColName:
			return false, nil
		case sqlparser.TableName:
			name := n.Name.String()
			if name != "" && !seen[name] {
				seen[name] = true
				tables = append(tables, name)
			}
		}
		return true, nil
	}, stmt)
	if err != nil {
		return stmtOther, nil, fmt.Errorf("plugins: sql walk: %w", err)
	}
	sort.Strings(tables)
	return kind, tables, nil
}

// checkDBTables verifies every referenced table against the plugin's grants
// for the statement class.
func checkDBTables(caps *Capabilities, kind stmtKind, tables []string) bool {
	for _, table := range tables {
		var ok bool
		switch kind {
		case stmtSelect:
			ok = caps.canDBRead(table)
		case stmtWrite, stmtDDL:
			ok = caps.canDBWrite(table)
		default:
			return false
		}
		if !ok {
			return false
		}
	}
	return true
}
