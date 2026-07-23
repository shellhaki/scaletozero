package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Status is the lifecycle state of a provisioned database.
type Status string

const (
	// StatusProvisioning: volume and container are being created / initialised.
	StatusProvisioning Status = "provisioning"
	// StatusStopped: scaled to zero. Container exists but is not running.
	StatusStopped Status = "stopped"
	// StatusRunning: container is up and serving.
	StatusRunning Status = "running"
	// StatusPaused: container processes frozen via the Docker freezer.
	StatusPaused Status = "paused"
	// StatusError: provisioning or a lifecycle transition failed.
	StatusError Status = "error"
)

// ErrNotFound is returned when no database matches the given name.
var ErrNotFound = errors.New("store: database not found")

// ErrExists is returned when creating a database whose name is taken.
var ErrExists = errors.New("store: database already exists")

// Database is one provisioned logical database and its compute metadata.
type Database struct {
	Name          string
	Engine        string
	ContainerID   string
	ContainerName string
	VolumeName    string
	Username      string
	Password      string
	Status        Status
	LastActive    time.Time
	CreatedAt     time.Time
}

// Store persists database metadata in SQLite.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS databases (
    name           TEXT PRIMARY KEY,
    engine         TEXT NOT NULL,
    container_id   TEXT NOT NULL DEFAULT '',
    container_name TEXT NOT NULL,
    volume_name    TEXT NOT NULL,
    username       TEXT NOT NULL,
    password       TEXT NOT NULL,
    status         TEXT NOT NULL,
    last_active    INTEGER NOT NULL DEFAULT 0,
    created_at     INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_databases_status ON databases(status);
`

// Open opens (creating if needed) the SQLite file at path and applies the
// schema. WAL plus a busy timeout keeps concurrent readers from tripping over
// the reaper's writes.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite takes a single writer; one connection removes lock contention
	// entirely. Metadata operations are off the proxy's hot path.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the underlying handle.
func (s *Store) Close() error { return s.db.Close() }

// Create inserts a new database row.
func (s *Store) Create(ctx context.Context, d Database) error {
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO databases
            (name, engine, container_id, container_name, volume_name, username, password, status, last_active, created_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.Name, d.Engine, d.ContainerID, d.ContainerName, d.VolumeName,
		d.Username, d.Password, string(d.Status), d.LastActive.Unix(), d.CreatedAt.Unix(),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrExists
		}
		return fmt.Errorf("insert database: %w", err)
	}
	return nil
}

// Get loads one database by name.
func (s *Store) Get(ctx context.Context, name string) (*Database, error) {
	row := s.db.QueryRowContext(ctx, `
        SELECT name, engine, container_id, container_name, volume_name,
               username, password, status, last_active, created_at
        FROM databases WHERE name = ?`, name)

	d, err := scanDatabase(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get database: %w", err)
	}
	return d, nil
}

// List returns every database, oldest first.
func (s *Store) List(ctx context.Context) ([]Database, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT name, engine, container_id, container_name, volume_name,
               username, password, status, last_active, created_at
        FROM databases ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("list databases: %w", err)
	}
	defer rows.Close()

	var out []Database
	for rows.Next() {
		d, err := scanDatabase(rows)
		if err != nil {
			return nil, fmt.Errorf("scan database: %w", err)
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// SetStatus updates the lifecycle status of a database.
func (s *Store) SetStatus(ctx context.Context, name string, status Status) error {
	return s.exec(ctx, name, `UPDATE databases SET status = ? WHERE name = ?`, string(status), name)
}

// SetContainer records the container ID backing a database.
func (s *Store) SetContainer(ctx context.Context, name, containerID string) error {
	return s.exec(ctx, name, `UPDATE databases SET container_id = ? WHERE name = ?`, containerID, name)
}

// Touch records that the database saw traffic at t.
func (s *Store) Touch(ctx context.Context, name string, t time.Time) error {
	return s.exec(ctx, name, `UPDATE databases SET last_active = ? WHERE name = ?`, t.Unix(), name)
}

// Delete removes a database row.
func (s *Store) Delete(ctx context.Context, name string) error {
	return s.exec(ctx, name, `DELETE FROM databases WHERE name = ?`, name)
}

// exec runs a statement and maps "no rows affected" to ErrNotFound.
func (s *Store) exec(ctx context.Context, name, query string, args ...any) error {
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("update database %q: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// scanner abstracts *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanDatabase(sc scanner) (*Database, error) {
	var (
		d                     Database
		status                string
		lastActive, createdAt int64
	)
	err := sc.Scan(&d.Name, &d.Engine, &d.ContainerID, &d.ContainerName, &d.VolumeName,
		&d.Username, &d.Password, &status, &lastActive, &createdAt)
	if err != nil {
		return nil, err
	}
	d.Status = Status(status)
	if lastActive > 0 {
		d.LastActive = time.Unix(lastActive, 0)
	}
	d.CreatedAt = time.Unix(createdAt, 0)
	return &d, nil
}

func isUniqueViolation(err error) bool {
	// modernc's driver reports constraint failures in the message; matching on
	// it avoids depending on the driver's internal error types.
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
