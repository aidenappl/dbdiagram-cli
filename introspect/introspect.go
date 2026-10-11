// Package introspect reads a live database's schema with Atlas's
// inspectors (ariga.io/atlas, Apache-2.0) and turns it into DBML via
// go-dbml's atlasrealm importer and emitter.
//
// Connections are built only from structured fields — never from a DSN the
// user typed — so no driver option (allowAllFiles, multiStatements,
// LOCAL INFILE, …) can be smuggled in. Every session is read-only and has a
// statement timeout.
package introspect

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"ariga.io/atlas/sql/schema"
	"github.com/aidenappl/go-dbml/importer"
	"github.com/aidenappl/go-dbml/importer/atlasrealm"
)

// Drivers.
const (
	DRIVER_MYSQL    = "mysql"
	DRIVER_MARIADB  = "mariadb"
	DRIVER_POSTGRES = "postgres"
	DRIVER_SQLITE   = "sqlite"
)

// TLS modes.
const (
	TLS_REQUIRE     = "require"     // encrypted, certificate not verified
	TLS_VERIFY_FULL = "verify-full" // encrypted, CA + hostname verified
	TLS_DISABLE     = "disable"     // plaintext
)

const (
	DEFAULT_STATEMENT_TIMEOUT = 30 * time.Second
	DEFAULT_CONNECT_TIMEOUT   = 10 * time.Second
	// MIN_STATEMENT_TIMEOUT: MariaDB's max_statement_time is in seconds and
	// 0 means "no limit", so anything shorter would silently disable it.
	MIN_STATEMENT_TIMEOUT = time.Second
	APPLICATION_NAME      = "dbdiagram-cli"
)

// DRIVERS lists the supported drivers.
var DRIVERS = []string{DRIVER_MYSQL, DRIVER_MARIADB, DRIVER_POSTGRES, DRIVER_SQLITE}

// Options describes one connection. Password is never logged.
type Options struct {
	Driver   string
	Host     string
	Port     int
	User     string
	Password string
	// Database is the database name (MySQL/MariaDB/Postgres) or the file
	// path (SQLite).
	Database string
	// Schemas limits the inspection (Postgres schemas). Empty = all
	// non-system schemas (Postgres) / the database (MySQL) / main (SQLite).
	Schemas []string
	TLS     string
	// TLSCA is an optional PEM CA bundle for verify-full (and Postgres
	// require, where it upgrades to CA verification).
	TLSCA            string
	StatementTimeout time.Duration
	ConnectTimeout   time.Duration
}

// hostPattern accepts hostnames, IPv4 and IPv6 literals — nothing that could
// carry URL or DSN syntax.
var hostPattern = regexp.MustCompile(`^[A-Za-z0-9._:%-]{1,253}$`)

// Validate normalises defaults and checks every field.
func (o *Options) Validate() error {
	o.Driver = strings.ToLower(strings.TrimSpace(o.Driver))
	switch o.Driver {
	case DRIVER_MYSQL, DRIVER_MARIADB, DRIVER_POSTGRES, DRIVER_SQLITE:
	case "":
		return errors.New("--driver is required (mysql, mariadb, postgres or sqlite)")
	default:
		return fmt.Errorf("--driver %q must be one of %s", o.Driver, strings.Join(DRIVERS, ", "))
	}
	switch {
	case o.StatementTimeout == 0:
		o.StatementTimeout = DEFAULT_STATEMENT_TIMEOUT
	case o.StatementTimeout < MIN_STATEMENT_TIMEOUT:
		return fmt.Errorf("--timeout %s must be at least %s", o.StatementTimeout, MIN_STATEMENT_TIMEOUT)
	}
	if o.ConnectTimeout <= 0 {
		o.ConnectTimeout = DEFAULT_CONNECT_TIMEOUT
	}
	if o.Database == "" {
		if o.Driver == DRIVER_SQLITE {
			return errors.New("--database (the SQLite file path) is required")
		}
		return errors.New("--database is required")
	}
	if o.Driver == DRIVER_SQLITE {
		if strings.ContainsAny(o.Database, "?#%") {
			return errors.New("the SQLite path must not contain ?, # or %")
		}
		if _, err := os.Stat(o.Database); err != nil {
			return fmt.Errorf("SQLite database: %w", err)
		}
		return nil
	}
	if o.TLS == "" {
		o.TLS = TLS_REQUIRE
	}
	switch o.TLS {
	case TLS_REQUIRE, TLS_VERIFY_FULL, TLS_DISABLE:
	default:
		return fmt.Errorf("--tls %q must be require, verify-full or disable", o.TLS)
	}
	if o.Host == "" {
		return errors.New("--host is required")
	}
	if !hostPattern.MatchString(o.Host) {
		return fmt.Errorf("--host %q is not a hostname or IP address", o.Host)
	}
	if o.Port == 0 {
		if o.Driver == DRIVER_POSTGRES {
			o.Port = 5432
		} else {
			o.Port = 3306
		}
	}
	if o.Port < 1 || o.Port > 65535 {
		return fmt.Errorf("--port %d is out of range", o.Port)
	}
	if o.User == "" {
		return errors.New("--user is required")
	}
	for _, s := range []string{o.User, o.Database} {
		if strings.ContainsRune(s, 0) {
			return errors.New("user and database must not contain NUL")
		}
	}
	return nil
}

// tlsConfig builds the client TLS config for the MySQL driver.
func (o *Options) tlsConfig() (*tls.Config, error) {
	switch o.TLS {
	case TLS_DISABLE:
		return nil, nil
	case TLS_REQUIRE:
		// Encrypt without verifying (libpq's sslmode=require semantics).
		return &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, nil //nolint:gosec // documented require mode
	}
	cfg := &tls.Config{ServerName: o.Host, MinVersion: tls.VersionTLS12}
	if o.TLSCA != "" {
		pem, err := os.ReadFile(o.TLSCA)
		if err != nil {
			return nil, fmt.Errorf("read --tls-ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("--tls-ca contains no PEM certificates")
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

// Open connects with a read-only, time-limited session.
func Open(ctx context.Context, o Options) (*sql.DB, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	var (
		db  *sql.DB
		err error
	)
	switch o.Driver {
	case DRIVER_MYSQL, DRIVER_MARIADB:
		db, err = openMySQL(o)
	case DRIVER_POSTGRES:
		db, err = openPostgres(o)
	case DRIVER_SQLITE:
		db, err = openSQLite(o)
	}
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)
	pctx, cancel := context.WithTimeout(ctx, o.ConnectTimeout+o.StatementTimeout)
	defer cancel()
	if err := db.PingContext(pctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect to %s: %w", o.Driver, err)
	}
	return db, nil
}

// Result is an inspection: canonical DBML plus anything the user should
// know about what could not be read.
type Result struct {
	DBML     string
	Warnings []string
}

// InspectRealm reads the schema (tables, columns, keys, indexes, enums,
// checks, comments). Warnings explain anything recovered or dropped.
func InspectRealm(ctx context.Context, o Options) (*schema.Realm, []string, error) {
	db, err := Open(ctx, o)
	if err != nil {
		return nil, nil, err
	}
	defer db.Close()
	realm, err := inspectDB(ctx, db, o)
	if err != nil {
		return nil, nil, err
	}
	var warnings []string
	if o.Driver == DRIVER_MYSQL || o.Driver == DRIVER_MARIADB {
		n, err := recoverMySQLFKs(ctx, db, realm, o.Database)
		if err != nil {
			return nil, nil, err
		}
		if n > 0 {
			warnings = append(warnings, fmt.Sprintf(
				"%d foreign key(s) were read from KEY_COLUMN_USAGE because REFERENTIAL_CONSTRAINTS is hidden from this user "+
					"(MariaDB shows it only with a privilege beyond SELECT); their ON DELETE/ON UPDATE actions are omitted. "+
					"GRANT REFERENCES ON %s.* to the inspecting user to include them", n, o.Database))
		}
	}
	return realm, warnings, nil
}

// DBML inspects the database and returns canonical DBML.
func DBML(ctx context.Context, o Options) (*Result, error) {
	realm, warnings, err := InspectRealm(ctx, o)
	if err != nil {
		return nil, err
	}
	out, more, err := RealmDBML(realm, o)
	if err != nil {
		return nil, err
	}
	return &Result{DBML: out, Warnings: append(warnings, more...)}, nil
}

// RealmDBML converts an inspected realm into DBML with go-dbml's
// atlasrealm.Import, which normalises raw expressions per dialect (MySQL
// backtick identifiers), names inline enums per column, drops foreign keys
// to tables outside the inspected schemas, and reports each such decision
// as a warning.
func RealmDBML(realm *schema.Realm, o Options) (string, []string, error) {
	opts := atlasrealm.Options{}
	switch o.Driver {
	case DRIVER_POSTGRES:
		opts.DefaultSchema, opts.Dialect = "public", importer.Postgres
	case DRIVER_SQLITE:
		opts.DefaultSchema = "main" // dialect left to detection
	default:
		opts.DefaultSchema, opts.Dialect = o.Database, importer.MySQL
	}
	res, err := atlasrealm.Import(realm, opts)
	if err != nil {
		return "", nil, fmt.Errorf("convert schema: %w", err)
	}
	return res.DBML, res.Warnings, nil
}
