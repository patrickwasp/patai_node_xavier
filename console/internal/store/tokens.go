package store

import (
	"database/sql"
	"errors"
	"time"
)

// Token is an API credential. Scope is viewer, operator, or key for an API key, which reaches only the apps'
// APIs. A token can never manage users, tokens, network or the cloud.
type Token struct {
	ID        int64   `json:"id"`
	Name      string  `json:"name"`
	Scope     string  `json:"scope"`
	CreatedBy string  `json:"created_by"`
	CreatedAt string  `json:"created_at"`
	ExpiresAt *string `json:"expires_at"`
}

func (s *Store) CreateToken(t Token, hash string, a Audit) (Token, error) {
	err := s.tx(func(tx *sql.Tx) error {
		t.CreatedAt = now()
		id, err := newTokenID(tx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO tokens (id, name, scope, token_hash, created_by, created_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, id, t.Name, t.Scope, hash, t.CreatedBy, t.CreatedAt, t.ExpiresAt); err != nil {
			return uniqueErr(err)
		}
		t.ID = id
		return addAudit(tx, a)
	})
	return t, err
}

func scanToken(row interface{ Scan(...any) error }) (Token, error) {
	var t Token
	var expires sql.NullString
	err := row.Scan(&t.ID, &t.Name, &t.Scope, &t.CreatedBy, &t.CreatedAt, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	if expires.Valid {
		t.ExpiresAt = &expires.String
	}
	return t, err
}

const tokenColumns = `id, name, scope, created_by, created_at, expires_at`

func (s *Store) TokenByHash(hash string) (Token, error) {
	return scanToken(s.db.QueryRow(`SELECT `+tokenColumns+` FROM tokens WHERE token_hash = ?`, hash))
}

func (s *Store) Tokens() ([]Token, error) {
	rows, err := s.db.Query(`SELECT ` + tokenColumns + ` FROM tokens ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Token{}
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, t)
	}
	return list, rows.Err()
}

func (s *Store) DeleteToken(id int64, a Audit) error {
	return s.tx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM tokens WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return addAudit(tx, a)
	})
}

// DeleteExpiredTokens removes what had run out by at, so that a clock set back can't bring it back.
func (s *Store) DeleteExpiredTokens(at time.Time) (int64, error) {
	var n int64
	err := s.tx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM tokens WHERE expires_at IS NOT NULL AND expires_at <= ?`,
			at.UTC().Format(time.RFC3339))
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}
