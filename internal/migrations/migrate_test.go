package migrations

import (
	"context"
	"log/slog"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
)

func TestSQLiteUpDownUp(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, database.Config{
		Driver: database.DialectSQLite,
		DSN:    "file:wp3?mode=memory&cache=shared",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	std, ok := database.Unwrap(db)
	if !ok {
		t.Fatal("unwrap")
	}

	logger := slog.New(slog.DiscardHandler)
	n, err := Up(ctx, std, database.DialectSQLite, logger)
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	if n != 29 {
		t.Errorf("applied = %d, want 29", n)
	}

	want := []string{
		"users", "groups", "group_members", "sessions",
		"app_passwords", "login_flows", "jobs", "module_config", "files", "uploads", "trash_items", "file_versions", "file_properties", "file_locks", "shares",
		"calendars", "calendar_objects", "addressbooks", "addressbook_objects",
		"notifications", "activities", "ocm_incoming", "calendar_shares", "plugins", "plugin_routes", "appconfig", "plugin_webdav_props", "addressbook_shares",
		"user_keys", "file_keys", "user_key_pw", "app_token_keys", "wopi_tokens", "wopi_token_keys", "mail_accounts",
		"mail_mailboxes", "mail_messages",
	}
	for _, table := range want {
		var name string
		err := db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		if err != nil {
			t.Errorf("table %s: %v", table, err)
		}
	}
	// files gained the nullable key_uuid column.
	var keyUUIDCol string
	err = db.QueryRow(ctx, `SELECT name FROM pragma_table_info('files') WHERE name='key_uuid'`).Scan(&keyUUIDCol)
	if err != nil {
		t.Errorf("files.key_uuid column: %v", err)
	}
	// file_keys gained the scheme column, sessions the sealed_uk column.
	var schemeCol string
	err = db.QueryRow(ctx, `SELECT name FROM pragma_table_info('file_keys') WHERE name='scheme'`).Scan(&schemeCol)
	if err != nil {
		t.Errorf("file_keys.scheme column: %v", err)
	}
	var sealedUKCol string
	err = db.QueryRow(ctx, `SELECT name FROM pragma_table_info('sessions') WHERE name='sealed_uk'`).Scan(&sealedUKCol)
	if err != nil {
		t.Errorf("sessions.sealed_uk column: %v", err)
	}
	// 0023: files and users gained the name_scheme column (ADR-0104).
	var filesSchemeCol string
	err = db.QueryRow(ctx, `SELECT name FROM pragma_table_info('files') WHERE name='name_scheme'`).Scan(&filesSchemeCol)
	if err != nil {
		t.Errorf("files.name_scheme column: %v", err)
	}
	var usersSchemeCol string
	err = db.QueryRow(ctx, `SELECT name FROM pragma_table_info('users') WHERE name='name_scheme'`).Scan(&usersSchemeCol)
	if err != nil {
		t.Errorf("users.name_scheme column: %v", err)
	}
	// 0024: shares gained the share-metadata ciphertext columns (ADR-0104 §7).
	var mountEncCol string
	err = db.QueryRow(ctx, `SELECT name FROM pragma_table_info('shares') WHERE name='mount_name_enc'`).Scan(&mountEncCol)
	if err != nil {
		t.Errorf("shares.mount_name_enc column: %v", err)
	}
	var absEncCol string
	err = db.QueryRow(ctx, `SELECT name FROM pragma_table_info('shares') WHERE name='abs_path_enc'`).Scan(&absEncCol)
	if err != nil {
		t.Errorf("shares.abs_path_enc column: %v", err)
	}
	// 0029: mail_messages gained the sync-time preview columns (ADR-0108 M6).
	var previewCol string
	err = db.QueryRow(ctx, `SELECT name FROM pragma_table_info('mail_messages') WHERE name='preview'`).Scan(&previewCol)
	if err != nil {
		t.Errorf("mail_messages.preview column: %v", err)
	}
	var hasAttCol string
	err = db.QueryRow(ctx, `SELECT name FROM pragma_table_info('mail_messages') WHERE name='has_attachments'`).Scan(&hasAttCol)
	if err != nil {
		t.Errorf("mail_messages.has_attachments column: %v", err)
	}

	v, dirty, err := Version(ctx, std, database.DialectSQLite)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if v != 29 || dirty {
		t.Errorf("version=%d dirty=%v", v, dirty)
	}

	n, err = Up(ctx, std, database.DialectSQLite, logger)
	if err != nil {
		t.Fatalf("up again: %v", err)
	}
	if n != 0 {
		t.Errorf("second up applied = %d, want 0", n)
	}

	if err := Down(ctx, std, database.DialectSQLite, 1, logger); err != nil {
		t.Fatalf("down: %v", err)
	}
	v, dirty, err = Version(ctx, std, database.DialectSQLite)
	if err != nil {
		t.Fatalf("version after down: %v", err)
	}
	if v != 28 || dirty {
		t.Errorf("after down version=%d dirty=%v", v, dirty)
	}
	// 0029's columns are gone at v28; 0028's table remains.
	var previewAt28 string
	err = db.QueryRow(ctx, `SELECT name FROM pragma_table_info('mail_messages') WHERE name='preview'`).Scan(&previewAt28)
	if err == nil {
		t.Error("mail_messages.preview column still present after down to v28")
	}
	var hasAttAt28 string
	err = db.QueryRow(ctx, `SELECT name FROM pragma_table_info('mail_messages') WHERE name='has_attachments'`).Scan(&hasAttAt28)
	if err == nil {
		t.Error("mail_messages.has_attachments column still present after down to v28")
	}
	var messagesAt28 string
	err = db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "mail_messages").Scan(&messagesAt28)
	if err != nil {
		t.Errorf("table mail_messages missing after down to v28: %v", err)
	}

	if err := Down(ctx, std, database.DialectSQLite, 1, logger); err != nil {
		t.Fatalf("down: %v", err)
	}
	v, dirty, err = Version(ctx, std, database.DialectSQLite)
	if err != nil {
		t.Fatalf("version after down: %v", err)
	}
	if v != 27 || dirty {
		t.Errorf("after down version=%d dirty=%v", v, dirty)
	}
	// 0028's tables are gone at v27; 0027's table remains.
	var mailboxesAt27 string
	err = db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "mail_mailboxes").Scan(&mailboxesAt27)
	if err == nil {
		t.Error("table mail_mailboxes still present after down to v27")
	}
	var messagesAt27 string
	err = db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "mail_messages").Scan(&messagesAt27)
	if err == nil {
		t.Error("table mail_messages still present after down to v27")
	}
	var mailAt27 string
	err = db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "mail_accounts").Scan(&mailAt27)
	if err != nil {
		t.Errorf("table mail_accounts missing after down to v27: %v", err)
	}

	if err := Down(ctx, std, database.DialectSQLite, 1, logger); err != nil {
		t.Fatalf("second down: %v", err)
	}
	v, dirty, err = Version(ctx, std, database.DialectSQLite)
	if err != nil {
		t.Fatalf("version after second down: %v", err)
	}
	if v != 26 || dirty {
		t.Errorf("after second down version=%d dirty=%v", v, dirty)
	}
	// 0027's table is gone at v26; 0026's table remains.
	var mailAt26 string
	err = db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "mail_accounts").Scan(&mailAt26)
	if err == nil {
		t.Error("table mail_accounts still present after down to v26")
	}
	var wopiKeysAt26 string
	if err := db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "wopi_token_keys").Scan(&wopiKeysAt26); err != nil {
		t.Errorf("table wopi_token_keys missing after down to v26: %v", err)
	}

	if err := Down(ctx, std, database.DialectSQLite, 1, logger); err != nil {
		t.Fatalf("third down: %v", err)
	}
	v, dirty, err = Version(ctx, std, database.DialectSQLite)
	if err != nil {
		t.Fatalf("version after third down: %v", err)
	}
	if v != 25 || dirty {
		t.Errorf("after second down version=%d dirty=%v", v, dirty)
	}
	// 0026's table is gone at v25; 0025's table remains.
	var wopiKeysAt25 string
	err = db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "wopi_token_keys").Scan(&wopiKeysAt25)
	if err == nil {
		t.Error("table wopi_token_keys still present after down to v25")
	}
	var wopiAt25 string
	if err := db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "wopi_tokens").Scan(&wopiAt25); err != nil {
		t.Errorf("table wopi_tokens missing after down to v25: %v", err)
	}

	if err := Down(ctx, std, database.DialectSQLite, 1, logger); err != nil {
		t.Fatalf("fourth down: %v", err)
	}
	v, dirty, err = Version(ctx, std, database.DialectSQLite)
	if err != nil {
		t.Fatalf("version after fourth down: %v", err)
	}
	if v != 24 || dirty {
		t.Errorf("after fourth down version=%d dirty=%v", v, dirty)
	}
	// 0025's table is gone at v24; 0024's columns remain.
	var wopiAt24 string
	err = db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "wopi_tokens").Scan(&wopiAt24)
	if err == nil {
		t.Error("table wopi_tokens still present after down to v24")
	}
	var mountEncAt24 string
	err = db.QueryRow(ctx, `SELECT name FROM pragma_table_info('shares') WHERE name='mount_name_enc'`).Scan(&mountEncAt24)
	if err != nil {
		t.Errorf("shares.mount_name_enc column missing after down to v24: %v", err)
	}

	if err := Down(ctx, std, database.DialectSQLite, 1, logger); err != nil {
		t.Fatalf("fifth down: %v", err)
	}
	v, dirty, err = Version(ctx, std, database.DialectSQLite)
	if err != nil {
		t.Fatalf("version after fifth down: %v", err)
	}
	if v != 23 || dirty {
		t.Errorf("after fifth down version=%d dirty=%v", v, dirty)
	}
	// 0024's columns are gone at v23; 0023's remain.
	var mountEncAt23 string
	err = db.QueryRow(ctx, `SELECT name FROM pragma_table_info('shares') WHERE name='mount_name_enc'`).Scan(&mountEncAt23)
	if err == nil {
		t.Error("shares.mount_name_enc column still present after down to v23")
	}
	var absEncAt23 string
	err = db.QueryRow(ctx, `SELECT name FROM pragma_table_info('shares') WHERE name='abs_path_enc'`).Scan(&absEncAt23)
	if err == nil {
		t.Error("shares.abs_path_enc column still present after down to v23")
	}
	var filesSchemeAt23 string
	err = db.QueryRow(ctx, `SELECT name FROM pragma_table_info('files') WHERE name='name_scheme'`).Scan(&filesSchemeAt23)
	if err != nil {
		t.Errorf("files.name_scheme column missing after down to v23: %v", err)
	}

	if err := Down(ctx, std, database.DialectSQLite, 1, logger); err != nil {
		t.Fatalf("sixth down: %v", err)
	}
	v, dirty, err = Version(ctx, std, database.DialectSQLite)
	if err != nil {
		t.Fatalf("version after sixth down: %v", err)
	}
	if v != 22 || dirty {
		t.Errorf("after sixth down version=%d dirty=%v", v, dirty)
	}
	// 0023's columns are gone at v22; 0022's table remains.
	var filesSchemeAt22 string
	err = db.QueryRow(ctx, `SELECT name FROM pragma_table_info('files') WHERE name='name_scheme'`).Scan(&filesSchemeAt22)
	if err == nil {
		t.Error("files.name_scheme column still present after down to v22")
	}
	var usersSchemeAt22 string
	err = db.QueryRow(ctx, `SELECT name FROM pragma_table_info('users') WHERE name='name_scheme'`).Scan(&usersSchemeAt22)
	if err == nil {
		t.Error("users.name_scheme column still present after down to v22")
	}
	var tokenKeysAt22 string
	if err := db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "app_token_keys").Scan(&tokenKeysAt22); err != nil {
		t.Errorf("table app_token_keys missing after down to v22: %v", err)
	}

	if err := Down(ctx, std, database.DialectSQLite, 1, logger); err != nil {
		t.Fatalf("seventh down: %v", err)
	}
	v, dirty, err = Version(ctx, std, database.DialectSQLite)
	if err != nil {
		t.Fatalf("version after seventh down: %v", err)
	}
	if v != 21 || dirty {
		t.Errorf("after seventh down version=%d dirty=%v", v, dirty)
	}
	// 0022's object is gone at v21; 0021's remain.
	var tokenKeysName string
	err = db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "app_token_keys").Scan(&tokenKeysName)
	if err == nil {
		t.Error("table app_token_keys still present after down to v21")
	}
	var pwNameAt21 string
	if err := db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "user_key_pw").Scan(&pwNameAt21); err != nil {
		t.Errorf("table user_key_pw missing after down to v21: %v", err)
	}

	if err := Down(ctx, std, database.DialectSQLite, 1, logger); err != nil {
		t.Fatalf("eighth down: %v", err)
	}
	v, dirty, err = Version(ctx, std, database.DialectSQLite)
	if err != nil {
		t.Fatalf("version after eighth down: %v", err)
	}
	if v != 20 || dirty {
		t.Errorf("after eighth down version=%d dirty=%v", v, dirty)
	}
	// 0021's objects are gone at v20; 0020's remain.
	var pwName string
	err = db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "user_key_pw").Scan(&pwName)
	if err == nil {
		t.Error("table user_key_pw still present after down to v20")
	}
	var schemeColAfter string
	err = db.QueryRow(ctx, `SELECT name FROM pragma_table_info('file_keys') WHERE name='scheme'`).Scan(&schemeColAfter)
	if err == nil {
		t.Error("file_keys.scheme column still present after down to v20")
	}
	var sealedUKColAfter string
	err = db.QueryRow(ctx, `SELECT name FROM pragma_table_info('sessions') WHERE name='sealed_uk'`).Scan(&sealedUKColAfter)
	if err == nil {
		t.Error("sessions.sealed_uk column still present after down to v20")
	}
	var userKeysAt20 string
	if err := db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "user_keys").Scan(&userKeysAt20); err != nil {
		t.Errorf("table user_keys missing after down to v20: %v", err)
	}

	if err := Down(ctx, std, database.DialectSQLite, 1, logger); err != nil {
		t.Fatalf("ninth down: %v", err)
	}
	v, dirty, err = Version(ctx, std, database.DialectSQLite)
	if err != nil {
		t.Fatalf("version after ninth down: %v", err)
	}
	if v != 19 || dirty {
		t.Errorf("after ninth down version=%d dirty=%v", v, dirty)
	}
	var userKeysName string
	err = db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "user_keys").Scan(&userKeysName)
	if err == nil {
		t.Error("table user_keys still present after down to v19")
	}
	var fileKeysName string
	err = db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "file_keys").Scan(&fileKeysName)
	if err == nil {
		t.Error("table file_keys still present after down to v19")
	}
	var keyUUIDColAfter string
	err = db.QueryRow(ctx, `SELECT name FROM pragma_table_info('files') WHERE name='key_uuid'`).Scan(&keyUUIDColAfter)
	if err == nil {
		t.Error("files.key_uuid column still present after down to v19")
	}
	var bookSharesName string
	err = db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "addressbook_shares").Scan(&bookSharesName)
	if err != nil {
		t.Errorf("table addressbook_shares missing after down to v19: %v", err)
	}
	var propsName string
	err = db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "plugin_webdav_props").Scan(&propsName)
	if err != nil {
		t.Errorf("table plugin_webdav_props missing after down to v19: %v", err)
	}
	var appconfigName string
	err = db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "appconfig").Scan(&appconfigName)
	if err != nil {
		t.Errorf("table appconfig missing after down to v19: %v", err)
	}
	var pluginRoutesName string
	err = db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "plugin_routes").Scan(&pluginRoutesName)
	if err != nil {
		t.Errorf("table plugin_routes missing after down to v19: %v", err)
	}
	var pluginName string
	err = db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "plugins").Scan(&pluginName)
	if err != nil {
		t.Errorf("table plugins missing after down to v19: %v", err)
	}
	var sharesName string
	if err := db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "calendar_shares").Scan(&sharesName); err != nil {
		t.Errorf("table calendar_shares missing after down to v19: %v", err)
	}

	n, err = Up(ctx, std, database.DialectSQLite, logger)
	if err != nil {
		t.Fatalf("up after down: %v", err)
	}
	if n != 10 {
		t.Errorf("re-up applied = %d, want 10", n)
	}
	v, dirty, err = Version(ctx, std, database.DialectSQLite)
	if err != nil {
		t.Fatalf("version after re-up: %v", err)
	}
	if v != 29 || dirty {
		t.Errorf("after re-up version=%d dirty=%v", v, dirty)
	}
}

func TestUnsupportedDialect(t *testing.T) {
	t.Parallel()
	_, _, err := Version(context.Background(), nil, "oracle")
	if err == nil {
		t.Fatal("expected error")
	}
}
