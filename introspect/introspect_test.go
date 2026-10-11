package introspect

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ariga.io/atlas/sql/schema"
	dbml "github.com/aidenappl/go-dbml"
)

func TestValidate(t *testing.T) {
	sqliteFile := filepath.Join(t.TempDir(), "x.db")
	db, err := sql.Open("sqlite", "file:"+sqliteFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	base := Options{Driver: "mysql", Host: "db.example.com", User: "ro", Database: "app"}
	cases := []struct {
		name    string
		mut     func(*Options)
		wantErr string
		check   func(*testing.T, Options)
	}{
		{"defaults", func(*Options) {}, "", func(t *testing.T, o Options) {
			if o.Port != 3306 || o.TLS != TLS_REQUIRE || o.StatementTimeout != DEFAULT_STATEMENT_TIMEOUT {
				t.Fatalf("defaults = %+v", o)
			}
		}},
		{"postgres port", func(o *Options) { o.Driver = "Postgres" }, "", func(t *testing.T, o Options) {
			if o.Port != 5432 || o.Driver != "postgres" {
				t.Fatalf("postgres defaults = %+v", o)
			}
		}},
		{"no driver", func(o *Options) { o.Driver = "" }, "--driver is required", nil},
		{"unknown driver", func(o *Options) { o.Driver = "oracle" }, "must be one of", nil},
		{"no host", func(o *Options) { o.Host = "" }, "--host is required", nil},
		{"dsn in host", func(o *Options) { o.Host = "x/db?allowAllFiles=true" }, "not a hostname", nil},
		{"user@host", func(o *Options) { o.Host = "root:pw@evil" }, "not a hostname", nil},
		{"ipv6", func(o *Options) { o.Host = "::1" }, "", nil},
		{"bad port", func(o *Options) { o.Port = 70000 }, "out of range", nil},
		{"no user", func(o *Options) { o.User = "" }, "--user is required", nil},
		{"no database", func(o *Options) { o.Database = "" }, "--database is required", nil},
		{"bad tls", func(o *Options) { o.TLS = "prefer" }, "--tls", nil},
		{"sqlite ok", func(o *Options) { *o = Options{Driver: "sqlite", Database: sqliteFile} }, "", nil},
		{"sqlite missing", func(o *Options) { *o = Options{Driver: "sqlite", Database: sqliteFile + ".nope"} }, "SQLite database", nil},
		{"sqlite uri chars", func(o *Options) { *o = Options{Driver: "sqlite", Database: "x.db?mode=rw"} }, "must not contain", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := base
			c.mut(&o)
			err := o.Validate()
			if c.wantErr == "" && err != nil {
				t.Fatalf("unexpected: %v", err)
			}
			if c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
				t.Fatalf("err = %v, want %q", err, c.wantErr)
			}
			if c.check != nil {
				c.check(t, o)
			}
		})
	}
}

func TestMySQLConfigIsLockedDown(t *testing.T) {
	for _, mode := range []string{TLS_REQUIRE, TLS_VERIFY_FULL, TLS_DISABLE} {
		t.Run(mode, func(t *testing.T) {
			o := Options{Driver: "mysql", Host: "db.example.com", Port: 3306, User: "ro", Password: "p@ss/w?rd", Database: "app", TLS: mode}
			if err := o.Validate(); err != nil {
				t.Fatal(err)
			}
			cfg, err := MySQLConfig(o)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.AllowAllFiles || cfg.MultiStatements || cfg.AllowCleartextPasswords || cfg.AllowOldPasswords || cfg.InterpolateParams || cfg.AllowFallbackToPlaintext {
				t.Fatalf("unsafe option on: %+v", cfg)
			}
			if cfg.Addr != "db.example.com:3306" || cfg.Passwd != "p@ss/w?rd" || cfg.DBName != "app" {
				t.Fatalf("fields = %q %q", cfg.Addr, cfg.DBName)
			}
			switch mode {
			case TLS_DISABLE:
				if cfg.TLS != nil {
					t.Fatal("disable must not set TLS")
				}
			case TLS_REQUIRE:
				if cfg.TLS == nil || !cfg.TLS.InsecureSkipVerify {
					t.Fatal("require must encrypt without verification")
				}
			case TLS_VERIFY_FULL:
				if cfg.TLS == nil || cfg.TLS.InsecureSkipVerify || cfg.TLS.ServerName != "db.example.com" {
					t.Fatal("verify-full must verify the host")
				}
			}
			if strings.Contains(cfg.FormatDSN(), "allowAllFiles") || strings.Contains(cfg.FormatDSN(), "multiStatements") {
				t.Fatalf("DSN carries an unsafe option: %s", cfg.FormatDSN())
			}
		})
	}
}

func TestNetJoinIPv6(t *testing.T) {
	if netJoin("::1", 3306) != "[::1]:3306" || netJoin("h", 1) != "h:1" {
		t.Fatal("netJoin")
	}
}

func TestPostgresConfigIsStructured(t *testing.T) {
	// Hostile environment: none of it may reach the connection.
	t.Setenv("PGSSLMODE", "disable")
	t.Setenv("PGHOST", "evil.example.com")
	t.Setenv("PGOPTIONS", "-c default_transaction_read_only=off")
	t.Setenv("PGSERVICE", "evil")
	o := Options{Driver: "postgres", Host: "db.example.com", User: "ro user", Password: `pa'ss\ sslmode=disable`, Database: "app", TLS: TLS_VERIFY_FULL, StatementTimeout: 15 * time.Second}
	if err := o.Validate(); err != nil {
		t.Fatal(err)
	}
	c := PostgresConfig(o)
	if c.Host != "db.example.com" || c.Port != 5432 || c.User != "ro user" || c.Password != o.Password || c.Database != "app" {
		t.Fatalf("fields = %+v", c)
	}
	if c.SSLMode != "verify-full" || c.Options != "" || c.Service != "" || c.Passfile != "" || c.ApplicationName != APPLICATION_NAME {
		t.Fatalf("unsafe config: %+v", c)
	}
	if c.Runtime["default_transaction_read_only"] != "on" || c.Runtime["statement_timeout"] != "15000" {
		t.Fatalf("runtime = %v", c.Runtime)
	}
	o.TLS = TLS_REQUIRE
	if PostgresConfig(o).SSLMode != "require" {
		t.Fatal("require")
	}
}

func TestStatementTimeoutFloor(t *testing.T) {
	for _, c := range []struct {
		d  time.Duration
		ok bool
	}{{0, true}, {time.Second, true}, {999 * time.Millisecond, false}, {-time.Second, false}, {time.Millisecond, false}} {
		o := Options{Driver: "mysql", Host: "h", User: "u", Database: "d", StatementTimeout: c.d}
		if err := o.Validate(); (err == nil) != c.ok {
			t.Errorf("timeout %s: err = %v, want ok=%v", c.d, err, c.ok)
		}
	}
}

func TestSQLiteDSNIsReadOnly(t *testing.T) {
	d := SQLiteDSN("/data/app.db")
	if !strings.Contains(d, "mode=ro") || !strings.Contains(d, "query_only(1)") || !strings.HasPrefix(d, "file:/data/app.db?") {
		t.Fatalf("dsn = %q", d)
	}
}

// TestSQLiteInspect runs the real Atlas SQLite inspector (pure Go, no
// Docker): every foreign key must become a Ref and the DBML must compile.
func TestSQLiteInspect(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT NOT NULL UNIQUE, created_at TEXT DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE orgs (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`,
		`CREATE TABLE memberships (user_id INTEGER NOT NULL REFERENCES users(id), org_id INTEGER NOT NULL REFERENCES orgs(id), role TEXT CHECK (role IN ('owner','member')), PRIMARY KEY (user_id, org_id))`,
		`CREATE TABLE posts (id INTEGER PRIMARY KEY, author_id INTEGER REFERENCES users(id), title TEXT NOT NULL)`,
		`CREATE INDEX posts_title ON posts (title)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	r, err := DBML(context.Background(), Options{Driver: "sqlite", Database: path})
	if err != nil {
		t.Fatal(err)
	}
	out := r.DBML
	res := dbml.ParseFile("inspect.dbml", out)
	if res.Database == nil || res.Diagnostics.HasErrors() {
		t.Fatalf("DBML does not compile: %v\n%s", res.Diagnostics, out)
	}
	if len(res.Database.Tables) != 4 {
		t.Fatalf("tables = %d\n%s", len(res.Database.Tables), out)
	}
	if len(res.Database.Refs) != 3 {
		t.Fatalf("refs = %d, want 3 (one per FK)\n%s", len(res.Database.Refs), out)
	}

	// The session is read-only: a write through our connection fails.
	ro, err := Open(context.Background(), Options{Driver: "sqlite", Database: path})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, err := ro.Exec(`INSERT INTO orgs (name) VALUES ('x')`); err == nil {
		t.Fatal("a write succeeded on the read-only connection")
	}
}

// TestRealmDBMLBacktickExpressions: MySQL/MariaDB report CHECK, default and
// generated expressions with backtick identifiers, which DBML cannot carry;
// go-dbml v0.1.1's atlasrealm.Import normalises them so emission succeeds.
func TestRealmDBMLBacktickExpressions(t *testing.T) {
	col := &schema.Column{Name: "age", Type: &schema.ColumnType{Type: &schema.IntegerType{T: "int"}, Raw: "int", Null: true},
		Default: &schema.RawExpr{X: "(`base` + 1)"}}
	gen := &schema.Column{Name: "age2", Type: &schema.ColumnType{Type: &schema.IntegerType{T: "int"}, Raw: "int", Null: true},
		Attrs: []schema.Attr{&schema.GeneratedExpr{Expr: "`age` * 2", Type: "VIRTUAL"}}}
	base := &schema.Column{Name: "base", Type: &schema.ColumnType{Type: &schema.IntegerType{T: "int"}, Raw: "int"}}
	tbl := &schema.Table{Name: "people", Columns: []*schema.Column{base, col, gen},
		Attrs: []schema.Attr{&schema.Check{Name: "age_ok", Expr: "`age` >= 0"}}}
	s := &schema.Schema{Name: "app", Tables: []*schema.Table{tbl}}
	tbl.Schema = s
	out, _, err := RealmDBML(&schema.Realm{Schemas: []*schema.Schema{s}}, Options{Driver: DRIVER_MYSQL, Database: "app"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "`age`") || !strings.Contains(out, "age >= 0") {
		t.Fatalf("DBML:\n%s", out)
	}
	if res := dbml.ParseFile("x.dbml", out); res.Diagnostics.HasErrors() {
		t.Fatalf("does not compile: %v\n%s", res.Diagnostics, out)
	}
}

// TestRealmDBMLEnumsAndExternalFKs (behaviour now provided by go-dbml v0.1.1 atlasrealm.Import): MySQL inline enums keep distinct
// names, and an FK to a table outside the realm is dropped with a warning.
func TestRealmDBMLEnumsAndExternalFKs(t *testing.T) {
	enumCol := func(name string, vals ...string) *schema.Column {
		return &schema.Column{Name: name, Type: &schema.ColumnType{Type: &schema.EnumType{T: "enum", Values: vals}, Raw: "enum"}}
	}
	id := &schema.Column{Name: "id", Type: &schema.ColumnType{Type: &schema.IntegerType{T: "int"}, Raw: "int"}}
	orgID := &schema.Column{Name: "org_id", Type: &schema.ColumnType{Type: &schema.IntegerType{T: "int"}, Raw: "int"}}
	users := &schema.Table{Name: "users", Columns: []*schema.Column{id, orgID, enumCol("status", "active", "disabled")}}
	members := &schema.Table{Name: "members", Columns: []*schema.Column{enumCol("role", "owner", "member")}}
	s := &schema.Schema{Name: "app", Tables: []*schema.Table{users, members}}
	users.Schema, members.Schema = s, s
	external := &schema.Table{Name: "orgs", Schema: &schema.Schema{Name: "other"}, Columns: []*schema.Column{{Name: "id"}}}
	users.ForeignKeys = []*schema.ForeignKey{{Symbol: "fk_org", Table: users, Columns: []*schema.Column{orgID}, RefTable: external, RefColumns: external.Columns}}

	out, warnings, err := RealmDBML(&schema.Realm{Schemas: []*schema.Schema{s}}, Options{Driver: DRIVER_MYSQL, Database: "app"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "users_status_enum") || !strings.Contains(out, "members_role_enum") || !strings.Contains(out, "disabled") || !strings.Contains(out, "owner") {
		t.Fatalf("enums collapsed:\n%s", out)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "other.orgs") {
		t.Fatalf("warnings = %v", warnings)
	}
	if strings.Contains(out, "Ref") {
		t.Fatalf("external ref kept:\n%s", out)
	}
}
