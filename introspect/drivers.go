package introspect

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"ariga.io/atlas/sql/migrate"
	atlasmysql "ariga.io/atlas/sql/mysql"
	atlaspg "ariga.io/atlas/sql/postgres"
	"ariga.io/atlas/sql/schema"
	atlassqlite "ariga.io/atlas/sql/sqlite"
	"github.com/go-sql-driver/mysql"
	"github.com/lib/pq"
	_ "modernc.org/sqlite" // pure-Go SQLite ("sqlite"), so cross-compiles need no cgo
)

// ER_UNKNOWN_SYSTEM_VARIABLE is MySQL/MariaDB error 1193.
const ER_UNKNOWN_SYSTEM_VARIABLE = 1193

// sessionConnector runs init on every new connection before database/sql
// hands it out, so a reconnect can never come back without the read-only
// session and the statement timeout.
type sessionConnector struct {
	driver.Connector
	init func(ctx context.Context, ex driver.ExecerContext) error
}

func (c *sessionConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	ex, ok := conn.(driver.ExecerContext)
	if !ok {
		conn.Close()
		return nil, errors.New("driver connection cannot execute session setup")
	}
	if err := c.init(ctx, ex); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// MySQLConfig builds the go-sql-driver config. Exported for tests: it
// proves no file-loading or multi-statement option is ever on.
func MySQLConfig(o Options) (*mysql.Config, error) {
	cfg := mysql.NewConfig()
	cfg.User = o.User
	cfg.Passwd = o.Password
	cfg.Net = "tcp"
	cfg.Addr = netJoin(o.Host, o.Port)
	cfg.DBName = o.Database
	cfg.AllowAllFiles = false // never LOAD DATA LOCAL INFILE
	cfg.AllowCleartextPasswords = false
	cfg.AllowOldPasswords = false
	cfg.AllowFallbackToPlaintext = false
	cfg.MultiStatements = false
	cfg.InterpolateParams = false
	cfg.Timeout = o.ConnectTimeout
	cfg.ReadTimeout = o.StatementTimeout + o.ConnectTimeout
	cfg.WriteTimeout = o.StatementTimeout + o.ConnectTimeout
	cfg.Params = nil
	tc, err := o.tlsConfig()
	if err != nil {
		return nil, err
	}
	cfg.TLS = tc
	cfg.TLSConfig = ""
	return cfg, nil
}

func netJoin(host string, port int) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		return "[" + host + "]:" + strconv.Itoa(port)
	}
	return host + ":" + strconv.Itoa(port)
}

func openMySQL(o Options) (*sql.DB, error) {
	cfg, err := MySQLConfig(o)
	if err != nil {
		return nil, err
	}
	conn, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	// MySQL caps SELECTs with max_execution_time (ms); MariaDB caps every
	// statement with max_statement_time (s). Try the one the driver flag
	// names first and fall back on "unknown system variable", so a MariaDB
	// server inspected with --driver mysql still gets a timeout.
	ms := o.StatementTimeout.Milliseconds()
	mysqlTimeout := fmt.Sprintf("SET SESSION max_execution_time = %d", ms)
	mariaTimeout := fmt.Sprintf("SET SESSION max_statement_time = %s", strconv.FormatFloat(float64(ms)/1000, 'f', 3, 64))
	first, second := mysqlTimeout, mariaTimeout
	if o.Driver == DRIVER_MARIADB {
		first, second = mariaTimeout, mysqlTimeout
	}
	return sql.OpenDB(&sessionConnector{Connector: conn, init: func(ctx context.Context, ex driver.ExecerContext) error {
		if _, err := ex.ExecContext(ctx, "SET SESSION TRANSACTION READ ONLY", nil); err != nil {
			return fmt.Errorf("make the session read-only: %w", err)
		}
		if _, err := ex.ExecContext(ctx, first, nil); err != nil {
			var me *mysql.MySQLError
			if !errors.As(err, &me) || me.Number != ER_UNKNOWN_SYSTEM_VARIABLE {
				return fmt.Errorf("set the statement timeout: %w", err)
			}
			if _, err := ex.ExecContext(ctx, second, nil); err != nil {
				return fmt.Errorf("set the statement timeout: %w", err)
			}
		}
		return nil
	}}), nil
}

// PostgresConfig builds the lib/pq config field by field. It is handed to
// pq.NewConnectorConfig directly: no connection string is ever parsed, and
// lib/pq's environment handling (PGHOST, PGSSLMODE, PGOPTIONS, PGSERVICE,
// PGPASSFILE, …) is never consulted — that only happens in pq.NewConfig /
// pq.NewConnector, which this package does not call. Exported for tests.
//
// The one file lib/pq may still read on its own is libpq's standard
// ~/.postgresql/root.crt as the CA for verify-full when --tls-ca is not
// given (and ~/.postgresql/postgresql.crt/.key as a client certificate);
// ~/.pgpass is consulted only when the password is empty.
func PostgresConfig(o Options) pq.Config {
	mode := pq.SSLModeRequire
	switch o.TLS {
	case TLS_DISABLE:
		mode = pq.SSLModeDisable
	case TLS_VERIFY_FULL:
		mode = pq.SSLModeVerifyFull
	}
	return pq.Config{
		Host:               o.Host,
		Port:               uint16(o.Port),
		Database:           o.Database,
		User:               o.User,
		Password:           o.Password,
		ApplicationName:    APPLICATION_NAME,
		SSLMode:            mode,
		SSLRootCert:        o.TLSCA,
		SSLSNI:             true,
		ConnectTimeout:     o.ConnectTimeout,
		ClientEncoding:     "UTF8",
		Datestyle:          "ISO, MDY",
		MinProtocolVersion: pq.ProtocolVersion30,
		MaxProtocolVersion: pq.ProtocolVersion30,
		// Runtime parameters sent at startup: every transaction on the
		// session is read-only and every statement is capped.
		Runtime: map[string]string{
			"default_transaction_read_only": "on",
			"statement_timeout":             strconv.FormatInt(o.StatementTimeout.Milliseconds(), 10),
		},
	}
}

func openPostgres(o Options) (*sql.DB, error) {
	conn, err := pq.NewConnectorConfig(PostgresConfig(o))
	if err != nil {
		return nil, fmt.Errorf("postgres connection settings: %w", err)
	}
	return sql.OpenDB(&sessionConnector{Connector: conn, init: func(ctx context.Context, ex driver.ExecerContext) error {
		// Belt and braces on top of the startup parameter.
		if _, err := ex.ExecContext(ctx, "SET SESSION CHARACTERISTICS AS TRANSACTION READ ONLY", nil); err != nil {
			return fmt.Errorf("make the session read-only: %w", err)
		}
		return nil
	}}), nil
}

// SQLiteDSN opens the file read-only with query_only on. Exported for tests.
func SQLiteDSN(path string) string {
	p := filepath.ToSlash(path)
	if runtime.GOOS == "windows" && filepath.VolumeName(path) != "" {
		p = "/" + p
	}
	return "file:" + p + "?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)"
}

func openSQLite(o Options) (*sql.DB, error) {
	return sql.Open("sqlite", SQLiteDSN(o.Database))
}

// inspectDB runs the Atlas inspector for o.Driver over db.
func inspectDB(ctx context.Context, db *sql.DB, o Options) (*schema.Realm, error) {
	var (
		drv migrate.Driver
		err error
	)
	opts := &schema.InspectRealmOption{Mode: schema.InspectSchemas | schema.InspectTables | schema.InspectTypes}
	switch o.Driver {
	case DRIVER_MYSQL, DRIVER_MARIADB:
		drv, err = atlasmysql.Open(db)
		opts.Schemas = []string{o.Database}
	case DRIVER_POSTGRES:
		drv, err = atlaspg.Open(db)
		opts.Schemas = o.Schemas
	case DRIVER_SQLITE:
		drv, err = atlassqlite.Open(db)
		opts.Schemas = []string{"main"}
	default:
		return nil, fmt.Errorf("unsupported driver %q", o.Driver)
	}
	if err != nil {
		return nil, fmt.Errorf("open inspector: %w", err)
	}
	realm, err := drv.InspectRealm(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("inspect schema: %w", err)
	}
	return realm, nil
}

// recoverMySQLFKs adds foreign keys that Atlas missed. Atlas reads FKs by
// joining KEY_COLUMN_USAGE with REFERENTIAL_CONSTRAINTS, and MariaDB hides
// REFERENTIAL_CONSTRAINTS (and TABLE_CONSTRAINTS) from a user that has only
// SELECT — so the recommended read-only account silently loses every FK.
// KEY_COLUMN_USAGE stays visible, so the constraints are rebuilt from it
// (without their ON DELETE / ON UPDATE actions). Returns how many were added.
func recoverMySQLFKs(ctx context.Context, db *sql.DB, realm *schema.Realm, database string) (int, error) {
	var sch *schema.Schema
	for _, s := range realm.Schemas {
		if s.Name == database {
			sch = s
		}
	}
	if sch == nil || len(sch.Tables) == 0 {
		return 0, nil
	}
	rows, err := db.QueryContext(ctx, `
SELECT CONSTRAINT_NAME, TABLE_NAME, COLUMN_NAME, REFERENCED_TABLE_SCHEMA, REFERENCED_TABLE_NAME, REFERENCED_COLUMN_NAME
FROM INFORMATION_SCHEMA.KEY_COLUMN_USAGE
WHERE TABLE_SCHEMA = ? AND REFERENCED_TABLE_NAME IS NOT NULL
ORDER BY TABLE_NAME, CONSTRAINT_NAME, ORDINAL_POSITION`, database)
	if err != nil {
		return 0, fmt.Errorf("read foreign keys: %w", err)
	}
	defer rows.Close()
	type key struct{ table, name string }
	built := map[key]*schema.ForeignKey{}
	var order []key
	stubs := map[string]*schema.Table{}
	for rows.Next() {
		var name, table, column, refSchema, refTable, refColumn string
		if err := rows.Scan(&name, &table, &column, &refSchema, &refTable, &refColumn); err != nil {
			return 0, fmt.Errorf("read foreign keys: %w", err)
		}
		t, ok := sch.Table(table)
		if !ok {
			continue
		}
		if _, exists := t.ForeignKey(name); exists {
			continue
		}
		k := key{table, name}
		fk := built[k]
		if fk == nil {
			var rt *schema.Table
			if refSchema == sch.Name {
				rt, _ = sch.Table(refTable)
			}
			if rt == nil {
				id := refSchema + "." + refTable
				if rt = stubs[id]; rt == nil {
					rt = &schema.Table{Name: refTable, Schema: &schema.Schema{Name: refSchema}}
					stubs[id] = rt
				}
			}
			fk = &schema.ForeignKey{Symbol: name, Table: t, RefTable: rt}
			built[k] = fk
			order = append(order, k)
		}
		c, ok := t.Column(column)
		if !ok {
			continue
		}
		rc, ok := fk.RefTable.Column(refColumn)
		if !ok {
			rc = &schema.Column{Name: refColumn}
			fk.RefTable.Columns = append(fk.RefTable.Columns, rc)
		}
		fk.Columns = append(fk.Columns, c)
		fk.RefColumns = append(fk.RefColumns, rc)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("read foreign keys: %w", err)
	}
	for _, k := range order {
		fk := built[k]
		if len(fk.Columns) == 0 || len(fk.Columns) != len(fk.RefColumns) {
			continue
		}
		fk.Table.ForeignKeys = append(fk.Table.ForeignKeys, fk)
	}
	return len(order), nil
}
