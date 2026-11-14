// Package store is the console's state on /persist: users, tokens, jobs and audit, and the API platform's key
// limits and request log, in SQLite.
//
// Both slots share this file, so a rollback runs an older release against what a newer one wrote
// (docs/console.md, State). Changes to the schema are additive, and the state format is declared in
// STATE_FORMAT beside this module, which bundle/hook checks before an install.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"modernc.org/sqlite"
)

// State format this release reads and writes. Keep in step with ../../STATE_FORMAT.
const (
	FormatWrites    = 1
	FormatReadsFrom = 1
	FormatReadsUpTo = 1
)

// 64 MB of the 232 MB usable on /persist, in 4 KiB pages.
const maxPages = 16384

// Roughly 4 MB of audit, and what's dropped first when space runs short.
const auditKeep = 20000

var (
	ErrIncompatible = errors.New("state format not readable by this release")
	ErrClaimed      = errors.New("unit is already claimed")
	ErrLastOwner    = errors.New("a unit must keep one owner")
	ErrNotFound     = errors.New("not found")
	ErrExists       = errors.New("name is already in use")
)

type Store struct {
	db  *sql.DB
	dir string
}

const schema = `
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS users (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	role TEXT NOT NULL,
	password_hash TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS recovery_codes (
	id INTEGER PRIMARY KEY,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	code_hash TEXT NOT NULL UNIQUE
);
CREATE TABLE IF NOT EXISTS tokens (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	scope TEXT NOT NULL,
	token_hash TEXT NOT NULL UNIQUE,
	created_by TEXT NOT NULL,
	created_at TEXT NOT NULL,
	expires_at TEXT
);
CREATE TABLE IF NOT EXISTS jobs (
	id INTEGER PRIMARY KEY,
	kind TEXT NOT NULL,
	target TEXT NOT NULL,
	actor TEXT NOT NULL,
	idempotency_key TEXT NOT NULL UNIQUE,
	state TEXT NOT NULL,
	result TEXT NOT NULL DEFAULT '',
	boot_id TEXT NOT NULL,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS audit (
	seq INTEGER PRIMARY KEY AUTOINCREMENT,
	boot_id TEXT NOT NULL,
	at TEXT NOT NULL,
	actor TEXT NOT NULL,
	origin TEXT NOT NULL,
	source TEXT NOT NULL,
	action TEXT NOT NULL,
	target TEXT NOT NULL,
	result TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS key_limits (
	token_id INTEGER PRIMARY KEY REFERENCES tokens(id) ON DELETE CASCADE,
	apis TEXT NOT NULL,
	max_concurrent INTEGER NOT NULL,
	max_body INTEGER NOT NULL,
	tokens_per_day INTEGER NOT NULL,
	images_per_day INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS api_requests (
	id INTEGER PRIMARY KEY,
	at TEXT NOT NULL,
	day TEXT NOT NULL,
	key_id INTEGER NOT NULL,
	key_name TEXT NOT NULL,
	api TEXT NOT NULL,
	endpoint TEXT NOT NULL,
	model TEXT NOT NULL,
	status INTEGER NOT NULL,
	prompt_tokens INTEGER NOT NULL,
	completion_tokens INTEGER NOT NULL,
	images INTEGER NOT NULL,
	millis INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS api_requests_day ON api_requests (day, key_id);
CREATE TABLE IF NOT EXISTS api_usage (
	day TEXT NOT NULL,
	key_id INTEGER NOT NULL,
	key_name TEXT NOT NULL,
	requests INTEGER NOT NULL,
	errors INTEGER NOT NULL,
	prompt_tokens INTEGER NOT NULL,
	completion_tokens INTEGER NOT NULL,
	images INTEGER NOT NULL,
	millis INTEGER NOT NULL,
	PRIMARY KEY (day, key_id)
);
`

// Open opens or creates the store. The returned error says which kind of fault it was, through
// IsStorageFault, because a storage fault isn't the release's fault and a bad schema is.
func Open(dir string) (*Store, error) {
	path := filepath.Join(dir, "console.db")
	dsn := "file:" + path + "?_txlock=immediate" +
		"&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)&_pragma=journal_size_limit(4194304)&_pragma=wal_autocheckpoint(256)" +
		fmt.Sprintf("&_pragma=max_page_count(%d)", maxPages)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One writer, one connection: the unit has one person at a time, and the pragmas are per connection.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, dir: dir}
	if err := s.prepare(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) prepare() error {
	if err := s.db.Ping(); err != nil {
		return err
	}
	var quick string
	if err := s.db.QueryRow("PRAGMA quick_check(1)").Scan(&quick); err != nil {
		return err
	}
	if quick != "ok" {
		return fmt.Errorf("quick_check: %s: %w", quick, errCorrupt)
	}
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		return err
	}
	var stored string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'state_format'`).Scan(&stored)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	default:
		format, err := strconv.Atoi(stored)
		if err != nil || format < FormatReadsFrom || format > FormatReadsUpTo {
			return fmt.Errorf("%w: stored %q, reads %d to %d", ErrIncompatible, stored, FormatReadsFrom, FormatReadsUpTo)
		}
	}
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	if _, err := s.db.Exec(`INSERT INTO meta (key, value) VALUES ('state_format', ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, strconv.Itoa(FormatWrites)); err != nil {
		return err
	}
	// bundle/hook reads this from a shell, before an install, with no SQLite on hand.
	return writeFileSync(filepath.Join(s.dir, "state-format"), strconv.Itoa(FormatWrites)+"\n")
}

func writeFileSync(path, content string) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err = f.WriteString(content); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

var errCorrupt = errors.New("database is corrupt")

// IsStorageFault says whether an error from Open means the storage is bad rather than this release.
func IsStorageFault(err error) bool {
	if errors.Is(err, errCorrupt) || errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
		return true
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() & 0xff {
		case 8, 10, 11, 13, 14, 26: // READONLY, IOERR, CORRUPT, FULL, CANTOPEN, NOTADB
			return true
		}
	}
	return false
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) tx(fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// Audit is one line of who did what. Time alone can't order it: a unit may not know the date, so the
// boot id and sequence number do (docs/console.md, Clock).
type Audit struct {
	Seq    int64  `json:"seq"`
	BootID string `json:"boot_id"`
	At     string `json:"at"`
	Actor  string `json:"actor"`
	Origin string `json:"origin"`
	Source string `json:"source"`
	Action string `json:"action"`
	Target string `json:"target"`
	Result string `json:"result"`
}

func addAudit(tx *sql.Tx, a Audit) error {
	res, err := tx.Exec(`INSERT INTO audit (boot_id, at, actor, origin, source, action, target, result)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, a.BootID, now(), a.Actor, a.Origin, a.Source, a.Action, a.Target, a.Result)
	if err != nil {
		return err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return err
	}
	if seq%500 == 0 {
		_, err = tx.Exec(`DELETE FROM audit WHERE seq <= ?`, seq-auditKeep)
	}
	return err
}

// AddAudit records something that changes nothing else here, such as a refused login.
func (s *Store) AddAudit(a Audit) error {
	return s.tx(func(tx *sql.Tx) error { return addAudit(tx, a) })
}

func (s *Store) AuditList(limit int, before int64) ([]Audit, error) {
	if before <= 0 {
		before = 1 << 62
	}
	rows, err := s.db.Query(`SELECT seq, boot_id, at, actor, origin, source, action, target, result
		FROM audit WHERE seq < ? ORDER BY seq DESC LIMIT ?`, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Audit{}
	for rows.Next() {
		var a Audit
		if err := rows.Scan(&a.Seq, &a.BootID, &a.At, &a.Actor, &a.Origin, &a.Source, &a.Action, &a.Target, &a.Result); err != nil {
			return nil, err
		}
		list = append(list, a)
	}
	return list, rows.Err()
}
