//go:build integration

package introspect

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"testing"
	"time"

	dbml "github.com/aidenappl/go-dbml"
	"github.com/aidenappl/go-dbml/model"

	"github.com/aidenappl/dbdiagram-cli/testenv"
)

const RO_PASSWORD = "ro-pw-'quote\\slash"

// mariaSeed: FKs (single and composite, two to the same table, ON DELETE
// CASCADE), a composite PK, enums, unique / secondary / composite indexes,
// table and column comments, defaults, auto-increment, a check.
var mariaSeed = []string{
	`CREATE TABLE orgs (
	   id INT AUTO_INCREMENT PRIMARY KEY,
	   name VARCHAR(100) NOT NULL COMMENT 'display name'
	 ) COMMENT='organisations'`,
	`CREATE TABLE users (
	   id BIGINT AUTO_INCREMENT PRIMARY KEY,
	   org_id INT NOT NULL,
	   email VARCHAR(255) NOT NULL,
	   status ENUM('active','disabled') NOT NULL DEFAULT 'active',
	   age INT NULL CHECK (age >= 0),
	   created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	   UNIQUE KEY uq_users_email (email),
	   KEY idx_users_org_status (org_id, status),
	   CONSTRAINT fk_users_org FOREIGN KEY (org_id) REFERENCES orgs (id) ON DELETE CASCADE
	 )`,
	`CREATE TABLE projects (
	   org_id INT NOT NULL,
	   slug VARCHAR(64) NOT NULL,
	   title VARCHAR(200) NULL,
	   PRIMARY KEY (org_id, slug),
	   CONSTRAINT fk_projects_org FOREIGN KEY (org_id) REFERENCES orgs (id)
	 )`,
	`CREATE TABLE memberships (
	   user_id BIGINT NOT NULL,
	   org_id INT NOT NULL,
	   project_slug VARCHAR(64) NOT NULL,
	   role ENUM('owner','member') NOT NULL,
	   PRIMARY KEY (user_id, org_id, project_slug),
	   CONSTRAINT fk_m_user FOREIGN KEY (user_id) REFERENCES users (id),
	   CONSTRAINT fk_m_project FOREIGN KEY (org_id, project_slug) REFERENCES projects (org_id, slug)
	 )`,
	`CREATE TABLE audit (
	   id INT PRIMARY KEY,
	   actor_id BIGINT NULL,
	   target_id BIGINT NULL,
	   CONSTRAINT fk_audit_actor FOREIGN KEY (actor_id) REFERENCES users (id),
	   CONSTRAINT fk_audit_target FOREIGN KEY (target_id) REFERENCES users (id)
	 )`,
}

var pgSeed = []string{
	`CREATE TYPE user_status AS ENUM ('active', 'disabled')`,
	`CREATE TABLE orgs (id SERIAL PRIMARY KEY, name TEXT NOT NULL)`,
	`COMMENT ON TABLE orgs IS 'organisations'`,
	`COMMENT ON COLUMN orgs.name IS 'display name'`,
	`CREATE TABLE users (
	   id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	   org_id INT NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
	   email TEXT NOT NULL UNIQUE,
	   status user_status NOT NULL DEFAULT 'active',
	   created_at TIMESTAMPTZ DEFAULT now()
	 )`,
	`CREATE INDEX idx_users_org_status ON users (org_id, status)`,
	`CREATE TABLE projects (
	   org_id INT NOT NULL REFERENCES orgs (id),
	   slug TEXT NOT NULL,
	   title TEXT,
	   PRIMARY KEY (org_id, slug)
	 )`,
	`CREATE TABLE memberships (
	   user_id BIGINT NOT NULL REFERENCES users (id),
	   org_id INT NOT NULL,
	   project_slug TEXT NOT NULL,
	   PRIMARY KEY (user_id, org_id, project_slug),
	   CONSTRAINT fk_m_project FOREIGN KEY (org_id, project_slug) REFERENCES projects (org_id, slug)
	 )`,
	`CREATE SCHEMA billing`,
	`CREATE TABLE billing.invoices (id SERIAL PRIMARY KEY, org_id INT NOT NULL REFERENCES public.orgs (id), total NUMERIC(10,2) NOT NULL)`,
}

// fkPairs reads every FK as "child_table->parent_table" from information_schema.
func fkPairs(t *testing.T, driver, dsn, query string) []string {
	t.Helper()
	db, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c, p string
		if err := rows.Scan(&c, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, c+"->"+p)
	}
	sort.Strings(out)
	return out
}

// refPairs maps every compiled Ref to "many_side->one_side" table names.
func refPairs(refs []*model.Ref) []string {
	var out []string
	for _, r := range refs {
		a, b := r.Endpoints[0], r.Endpoints[1]
		// The FK side is the "many" (or 0..*) side.
		if strings.Contains(a.Relation, "*") {
			out = append(out, qual(a)+"->"+qual(b))
		} else {
			out = append(out, qual(b)+"->"+qual(a))
		}
	}
	sort.Strings(out)
	return out
}

func qual(e model.RefEndpoint) string {
	if e.SchemaName != nil && *e.SchemaName != "" {
		return *e.SchemaName + "." + e.TableName
	}
	return e.TableName
}

func compile(t *testing.T, out string) *model.Database {
	t.Helper()
	res := dbml.ParseFile("inspect.dbml", out)
	if res.Database == nil || res.Diagnostics.HasErrors() {
		t.Fatalf("inspected DBML does not compile: %v\n%s", res.Diagnostics.Errors(), out)
	}
	return res.Database
}

func TestInspectMariaDB(t *testing.T) {
	ctx := context.Background()
	c, err := testenv.StartMariaDB(ctx, "app")
	t.Cleanup(c.Terminate)
	if err != nil {
		t.Fatal(err)
	}
	testenv.Exec(t, "mysql", c.AdminDSN, mariaSeed...)
	testenv.Exec(t, "mysql", c.AdminDSN,
		`CREATE USER 'ro'@'%' IDENTIFIED BY '`+strings.ReplaceAll(strings.ReplaceAll(RO_PASSWORD, `\`, `\\`), `'`, `\'`)+`'`,
		`GRANT SELECT ON app.* TO 'ro'@'%'`,
		`CREATE USER 'roref'@'%' IDENTIFIED BY 'roref-pw'`,
		`GRANT SELECT, REFERENCES ON app.* TO 'roref'@'%'`)

	want := fkPairs(t, "mysql", c.AdminDSN, `SELECT TABLE_NAME, REFERENCED_TABLE_NAME FROM information_schema.REFERENTIAL_CONSTRAINTS WHERE CONSTRAINT_SCHEMA = 'app'`)
	if len(want) != 6 {
		t.Fatalf("seed has %d FKs, want 6", len(want))
	}
	opts := Options{Driver: DRIVER_MARIADB, Host: c.Host, Port: c.Port, User: "ro", Password: RO_PASSWORD, Database: "app"}

	// SELECT-only: MariaDB hides REFERENTIAL_CONSTRAINTS, Atlas sees no FK;
	// the CLI rebuilds them from KEY_COLUMN_USAGE and warns.
	// MariaDB 11.4 serves TLS with an auto-generated certificate, so the
	// default (require) works.
	res, err := DBML(ctx, opts)
	if err != nil {
		t.Fatalf("inspect (tls require): %v", err)
	}
	out := res.DBML
	db := compile(t, out)
	if got := refPairs(db.Refs); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("SELECT-only refs = %v\nFKs  = %v\n%s", got, want, out)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "GRANT REFERENCES") {
		t.Fatalf("warnings = %v", res.Warnings)
	}

	// SELECT + REFERENCES: Atlas reads them directly, actions included.
	ref := opts
	ref.User, ref.Password = "roref", "roref-pw"
	res, err = DBML(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	out = res.DBML
	db = compile(t, out)
	if got := refPairs(db.Refs); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("refs = %v\nFKs  = %v\n%s", got, want, out)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", res.Warnings)
	}
	if len(db.Tables) != 5 {
		t.Fatalf("tables = %d", len(db.Tables))
	}
	for _, s := range []string{"age >= 0", "default: 'active'", "users_status_enum", "memberships_role_enum", "disabled", "uq_users_email",
		"idx_users_org_status", "display name", "organisations", "increment", "Table projects", "delete: cascade", "(org_id, slug) [pk]"} {
		if !strings.Contains(out, s) {
			t.Errorf("DBML lacks %s\n%s", s, out)
		}
	}
	if len(db.Enums) != 2 {
		t.Errorf("enums = %d, want 2", len(db.Enums))
	}
	// Composite PK on projects.
	for _, tb := range db.Tables {
		if tb.Name != "projects" {
			continue
		}
		found := false
		for _, ix := range tb.Indexes {
			if ix.PK != nil && *ix.PK && len(ix.Columns) == 2 {
				found = true
			}
		}
		if !found {
			t.Errorf("projects composite PK missing\n%s", out)
		}
	}

	t.Run("driver mysql against mariadb falls back to max_statement_time", func(t *testing.T) {
		o := opts
		o.Driver = DRIVER_MYSQL
		if _, err := DBML(ctx, o); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("session is read-only with a timeout", func(t *testing.T) {
		o := opts
		o.User, o.Password = "root", testenv.ROOT_PASSWORD // even a superuser session cannot write
		o.StatementTimeout = 2 * time.Second
		conn, err := Open(ctx, o)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.Exec(`INSERT INTO orgs (name) VALUES ('x')`); err == nil {
			t.Fatal("write succeeded on a read-only session")
		}
		var ro int
		if err := conn.QueryRow(`SELECT @@session.transaction_read_only`).Scan(&ro); err != nil || ro != 1 {
			t.Fatalf("transaction_read_only = %d, %v", ro, err)
		}
		var mst float64
		if err := conn.QueryRow(`SELECT @@session.max_statement_time`).Scan(&mst); err != nil || mst != 2 {
			t.Fatalf("max_statement_time = %v, %v", mst, err)
		}
		if _, err := conn.Exec(`LOAD DATA LOCAL INFILE '/etc/passwd' INTO TABLE orgs`); err == nil {
			t.Fatal("LOAD DATA LOCAL INFILE was allowed")
		}
		if _, err := conn.Exec(`SELECT 1; SELECT 2`); err == nil {
			t.Fatal("multi-statements were allowed")
		}
	})
	t.Run("verify-full refuses the self-signed certificate", func(t *testing.T) {
		o := opts
		o.TLS = TLS_VERIFY_FULL
		if _, err := DBML(ctx, o); err == nil {
			t.Fatal("verify-full accepted a self-signed certificate")
		}
	})
	t.Run("wrong password", func(t *testing.T) {
		o := opts
		o.Password = "nope"
		_, err := DBML(ctx, o)
		if err == nil || strings.Contains(err.Error(), "nope") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestInspectPostgres(t *testing.T) {
	ctx := context.Background()
	c, err := testenv.StartPostgres(ctx, "app")
	t.Cleanup(c.Terminate)
	if err != nil {
		t.Fatal(err)
	}
	testenv.Exec(t, "postgres", c.AdminDSN, pgSeed...)
	testenv.Exec(t, "postgres", c.AdminDSN,
		`CREATE ROLE ro LOGIN PASSWORD '`+strings.ReplaceAll(RO_PASSWORD, `'`, `''`)+`'`,
		`GRANT USAGE ON SCHEMA public, billing TO ro`,
		`GRANT SELECT ON ALL TABLES IN SCHEMA public, billing TO ro`)

	opts := Options{Driver: DRIVER_POSTGRES, Host: c.Host, Port: c.Port, User: "ro", Password: RO_PASSWORD, Database: "app"}

	t.Run("tls require is the default and refuses a plaintext server", func(t *testing.T) {
		// PG* environment is never consulted (pq.Config is built directly).
		t.Setenv("PGSSLMODE", "disable")
		t.Setenv("PGOPTIONS", "-c default_transaction_read_only=off")
		if _, err := DBML(ctx, opts); err == nil {
			t.Fatal("require connected to a server without TLS")
		}
	})

	opts.TLS = TLS_DISABLE
	res, err := DBML(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	out := res.DBML
	db := compile(t, out)
	want := fkPairs(t, "postgres", c.AdminDSN, `
		SELECT CASE WHEN tc.table_schema = 'public' THEN tc.table_name ELSE tc.table_schema || '.' || tc.table_name END,
		       CASE WHEN ccu.table_schema = 'public' THEN ccu.table_name ELSE ccu.table_schema || '.' || ccu.table_name END
		FROM information_schema.table_constraints tc
		JOIN (SELECT DISTINCT constraint_schema, constraint_name, table_schema, table_name FROM information_schema.constraint_column_usage) ccu
		  ON ccu.constraint_name = tc.constraint_name AND ccu.constraint_schema = tc.constraint_schema
		WHERE tc.constraint_type = 'FOREIGN KEY'`)
	if len(want) != 5 {
		t.Fatalf("seed has %d FKs, want 5: %v", len(want), want)
	}
	if got := refPairs(db.Refs); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("refs = %v\nFKs  = %v\n%s", got, want, out)
	}
	for _, s := range []string{"user_status", "disabled", "display name", "organisations", "billing.invoices", "idx_users_org_status"} {
		if !strings.Contains(out, s) {
			t.Errorf("DBML lacks %s\n%s", s, out)
		}
	}

	t.Run("schema filter", func(t *testing.T) {
		o := opts
		o.Schemas = []string{"billing"}
		res, err := DBML(ctx, o)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(res.DBML, "Table users") || !strings.Contains(res.DBML, "invoices") {
			t.Fatalf("schema filter ignored:\n%s", res.DBML)
		}
		compile(t, res.DBML)
		if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "invoices") || !strings.Contains(res.Warnings[0], "public.orgs") {
			t.Fatalf("warnings = %v", res.Warnings)
		}
	})
	t.Run("session is read-only with a timeout", func(t *testing.T) {
		t.Setenv("PGOPTIONS", "-c default_transaction_read_only=off -c statement_timeout=0")
		o := opts
		o.User, o.Password = "postgres", testenv.ROOT_PASSWORD
		o.StatementTimeout = 1500 * time.Millisecond
		conn, err := Open(ctx, o)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.Exec(`INSERT INTO orgs (name) VALUES ('x')`); err == nil || !strings.Contains(err.Error(), "read-only") {
			t.Fatalf("write on a read-only session: %v", err)
		}
		var st string
		if err := conn.QueryRow(`SHOW statement_timeout`).Scan(&st); err != nil || st != "1500ms" {
			t.Fatalf("statement_timeout = %q, %v", st, err)
		}
		if _, err := conn.Exec(`SELECT pg_sleep(5)`); err == nil {
			t.Fatal("pg_sleep(5) outlived a 1.5s statement timeout")
		}
	})
}
