package store

import (
	"database/sql"
	"errors"
	"strings"
)

type User struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Role         string `json:"role"`
	CreatedAt    string `json:"created_at"`
	PasswordHash string `json:"-"`
}

func uniqueErr(err error) error {
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return ErrExists
	}
	return err
}

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Name, &u.Role, &u.CreatedAt, &u.PasswordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	return u, err
}

const userColumns = `id, name, role, created_at, password_hash`

// HasOwner is what "claimed" means: a unit with no owner in a readable store can be claimed only if
// it also holds a claim code, so missing state never looks like a new unit.
func (s *Store) HasOwner() (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM users WHERE role = 'owner'`).Scan(&n)
	return n > 0, err
}

// Claim creates the first owner and their recovery codes, in one transaction with the audit line.
func (s *Store) Claim(name, passwordHash string, recovery []string, a Audit) (User, error) {
	var u User
	err := s.tx(func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM users WHERE role = 'owner'`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrClaimed
		}
		at := now()
		res, err := tx.Exec(`INSERT INTO users (name, role, password_hash, created_at) VALUES (?, 'owner', ?, ?)`,
			name, passwordHash, at)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		for _, code := range recovery {
			if _, err := tx.Exec(`INSERT INTO recovery_codes (user_id, code_hash) VALUES (?, ?)`, id, code); err != nil {
				return err
			}
		}
		u = User{ID: id, Name: name, Role: "owner", CreatedAt: at}
		return addAudit(tx, a)
	})
	return u, err
}

func (s *Store) UserByName(name string) (User, error) {
	return scanUser(s.db.QueryRow(`SELECT `+userColumns+` FROM users WHERE name = ?`, name))
}

func (s *Store) UserByID(id int64) (User, error) {
	return scanUser(s.db.QueryRow(`SELECT `+userColumns+` FROM users WHERE id = ?`, id))
}

func (s *Store) Users() ([]User, error) {
	rows, err := s.db.Query(`SELECT ` + userColumns + ` FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, u)
	}
	return list, rows.Err()
}

func (s *Store) CreateUser(name, role, passwordHash string, a Audit) (User, error) {
	var u User
	err := s.tx(func(tx *sql.Tx) error {
		at := now()
		res, err := tx.Exec(`INSERT INTO users (name, role, password_hash, created_at) VALUES (?, ?, ?, ?)`,
			name, role, passwordHash, at)
		if err != nil {
			return uniqueErr(err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		u = User{ID: id, Name: name, Role: role, CreatedAt: at}
		return addAudit(tx, a)
	})
	return u, err
}

func (s *Store) DeleteUser(id int64, a Audit) error {
	return s.tx(func(tx *sql.Tx) error {
		var role string
		err := tx.QueryRow(`SELECT role FROM users WHERE id = ?`, id).Scan(&role)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if role == "owner" {
			var owners int
			if err := tx.QueryRow(`SELECT count(*) FROM users WHERE role = 'owner'`).Scan(&owners); err != nil {
				return err
			}
			if owners == 1 {
				return ErrLastOwner
			}
		}
		if _, err := tx.Exec(`DELETE FROM users WHERE id = ?`, id); err != nil {
			return err
		}
		return addAudit(tx, a)
	})
}

func (s *Store) SetPassword(id int64, passwordHash string, a Audit) error {
	return s.tx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, passwordHash, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return addAudit(tx, a)
	})
}

// Recover uses up one recovery code and sets a new password, or changes nothing.
func (s *Store) Recover(name, codeHash, passwordHash string, a Audit) (User, error) {
	var u User
	err := s.tx(func(tx *sql.Tx) error {
		var err error
		u, err = scanUser(tx.QueryRow(`SELECT `+userColumns+` FROM users WHERE name = ?`, name))
		if err != nil {
			return err
		}
		res, err := tx.Exec(`DELETE FROM recovery_codes WHERE user_id = ? AND code_hash = ?`, u.ID, codeHash)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if _, err := tx.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, passwordHash, u.ID); err != nil {
			return err
		}
		return addAudit(tx, a)
	})
	return u, err
}

func (s *Store) RecoveryCodesLeft(id int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM recovery_codes WHERE user_id = ?`, id).Scan(&n)
	return n, err
}
