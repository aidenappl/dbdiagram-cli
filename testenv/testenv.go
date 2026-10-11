//go:build integration

// Package testenv starts throwaway MariaDB 11.4 and Postgres 16 containers
// for the integration suite. Data directories live on tmpfs (the Docker VM
// disk is shared and tight); readiness is a real ping from the host, the
// approach dbdiagram-api's testdb uses.
package testenv

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"strconv"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/mariadb"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	MARIADB_IMAGE  = "mariadb:11.4"
	POSTGRES_IMAGE = "postgres:16"
	ROOT_PASSWORD  = "test-root-pw"
	START_TIMEOUT  = 3 * time.Minute
)

func init() {
	// The driver logs every refused connection while the server boots.
	_ = mysql.SetLogger(log.New(io.Discard, "", 0))
}

// DB is a running container and how to reach it.
type DB struct {
	Host     string
	Port     int
	Database string
	// AdminDSN is a superuser DSN (seeding only — the CLI never takes a DSN).
	AdminDSN string
	c        testcontainers.Container
}

// Terminate removes the container.
func (d *DB) Terminate() {
	if d != nil && d.c != nil {
		_ = testcontainers.TerminateContainer(d.c)
	}
}

// StartMariaDB starts mariadb:11.4 with database name db.
func StartMariaDB(ctx context.Context, db string) (*DB, error) {
	c, err := mariadb.Run(ctx, MARIADB_IMAGE,
		mariadb.WithDatabase(db),
		mariadb.WithPassword(ROOT_PASSWORD),
		testcontainers.WithWaitStrategy(wait.ForListeningPort("3306/tcp").SkipInternalCheck().WithStartupTimeout(START_TIMEOUT)),
		testcontainers.WithTmpfs(map[string]string{"/var/lib/mysql": "rw,noexec,nosuid,size=512m"}),
		testcontainers.WithCmdArgs("--innodb-flush-log-at-trx-commit=0", "--skip-log-bin", "--performance-schema=0"),
	)
	out := &DB{Database: db}
	if c != nil {
		out.c = c
	}
	if err != nil {
		return out, fmt.Errorf("start mariadb: %w", err)
	}
	if out.Host, out.Port, err = hostPort(ctx, c, "3306/tcp"); err != nil {
		return out, err
	}
	cfg := mysql.NewConfig()
	cfg.User, cfg.Passwd, cfg.Net, cfg.Addr, cfg.DBName = "root", ROOT_PASSWORD, "tcp", out.Host+":"+strconv.Itoa(out.Port), db
	cfg.MultiStatements = true // seeding only
	out.AdminDSN = cfg.FormatDSN()
	return out, waitPing("mysql", out.AdminDSN)
}

// StartPostgres starts postgres:16 with database name db (TLS off, the
// image default).
func StartPostgres(ctx context.Context, db string) (*DB, error) {
	c, err := postgres.Run(ctx, POSTGRES_IMAGE,
		postgres.WithDatabase(db),
		postgres.WithUsername("postgres"),
		postgres.WithPassword(ROOT_PASSWORD),
		testcontainers.WithWaitStrategy(wait.ForListeningPort("5432/tcp").SkipInternalCheck().WithStartupTimeout(START_TIMEOUT)),
		testcontainers.WithTmpfs(map[string]string{"/var/lib/postgresql/data": "rw,size=512m"}),
		testcontainers.WithCmdArgs("-c", "fsync=off", "-c", "full_page_writes=off"),
	)
	out := &DB{Database: db}
	if c != nil {
		out.c = c
	}
	if err != nil {
		return out, fmt.Errorf("start postgres: %w", err)
	}
	if out.Host, out.Port, err = hostPort(ctx, c, "5432/tcp"); err != nil {
		return out, err
	}
	out.AdminDSN = fmt.Sprintf("host=%s port=%d user=postgres password=%s dbname=%s sslmode=disable", out.Host, out.Port, ROOT_PASSWORD, db)
	return out, waitPing("postgres", out.AdminDSN)
}

func hostPort(ctx context.Context, c testcontainers.Container, port string) (string, int, error) {
	host, err := c.Host(ctx)
	if err != nil {
		return "", 0, err
	}
	p, err := c.MappedPort(ctx, port)
	if err != nil {
		return "", 0, err
	}
	if host == "localhost" {
		host = "127.0.0.1"
	}
	n, err := strconv.Atoi(p.Port())
	return host, n, err
}

// waitPing polls a real connection: the port opens before the server is
// usable (both entrypoints run a temporary server while initialising).
func waitPing(driver, dsn string) error {
	deadline := time.Now().Add(START_TIMEOUT)
	var last error
	for time.Now().Before(deadline) {
		db, err := sql.Open(driver, dsn)
		if err == nil {
			err = db.Ping()
			db.Close()
		}
		if err == nil {
			// The temporary init server can answer once and then restart.
			time.Sleep(500 * time.Millisecond)
			if db2, err2 := sql.Open(driver, dsn); err2 == nil {
				err2 = db2.Ping()
				db2.Close()
				if err2 == nil {
					return nil
				}
				last = err2
			}
		} else {
			last = err
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("%s never became ready: %v", driver, last)
}

// Exec runs statements as the admin.
func Exec(t testing.TB, driver, dsn string, stmts ...string) {
	t.Helper()
	db, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
	}
}
