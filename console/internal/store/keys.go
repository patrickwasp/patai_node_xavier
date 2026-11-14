package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Limits are an API key's: the APIs it may use, empty for all of them, and how much at once, in a request and in a
// day. Zero is no limit.
type Limits struct {
	APIs          []string `json:"apis"`
	MaxConcurrent int64    `json:"max_concurrent"`
	MaxBody       int64    `json:"max_body"`
	TokensPerDay  int64    `json:"tokens_per_day"`
	ImagesPerDay  int64    `json:"images_per_day"`
}

// Key is an API key and its limits: a token with the key scope, which reaches the apps' APIs and nothing else.
type Key struct {
	Token
	Limits Limits `json:"limits"`
}

// KeyScope is an API key's scope.
const KeyScope = "key"

// newTokenID is past every token's id there has been, so a new one never takes a deleted one's usage: SQLite would
// reuse the highest.
func newTokenID(tx *sql.Tx) (int64, error) {
	var id int64
	err := tx.QueryRow(`SELECT MAX(id) FROM (SELECT COALESCE(MAX(id), 0) AS id FROM tokens
		UNION ALL SELECT COALESCE(MAX(key_id), 0) FROM api_usage UNION ALL SELECT COALESCE(MAX(key_id), 0) FROM api_requests
		UNION ALL SELECT COALESCE(CAST(value AS INTEGER), 0) FROM meta WHERE key = 'last_token_id')`).Scan(&id)
	if err != nil {
		return 0, err
	}
	id++
	_, err = tx.Exec(`INSERT INTO meta (key, value) VALUES ('last_token_id', ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, id)
	return id, err
}

func scanLimits(apis string, l *Limits) {
	l.APIs = []string{}
	for _, api := range strings.Split(apis, ",") {
		if api != "" {
			l.APIs = append(l.APIs, api)
		}
	}
}

// CreateKey makes the token and its limits at once.
func (s *Store) CreateKey(t Token, hash string, l Limits, a Audit) (Key, error) {
	var k Key
	err := s.tx(func(tx *sql.Tx) error {
		t.CreatedAt, t.Scope = now(), KeyScope
		id, err := newTokenID(tx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO tokens (id, name, scope, token_hash, created_by, created_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, id, t.Name, t.Scope, hash, t.CreatedBy, t.CreatedAt, t.ExpiresAt); err != nil {
			return uniqueErr(err)
		}
		t.ID = id
		if err := setLimits(tx, t.ID, l); err != nil {
			return err
		}
		k = Key{Token: t, Limits: l}
		return addAudit(tx, a)
	})
	return k, err
}

func setLimits(tx *sql.Tx, id int64, l Limits) error {
	_, err := tx.Exec(`INSERT INTO key_limits (token_id, apis, max_concurrent, max_body, tokens_per_day, images_per_day)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (token_id) DO UPDATE SET apis = excluded.apis,
		max_concurrent = excluded.max_concurrent, max_body = excluded.max_body,
		tokens_per_day = excluded.tokens_per_day, images_per_day = excluded.images_per_day`,
		id, strings.Join(l.APIs, ","), l.MaxConcurrent, l.MaxBody, l.TokensPerDay, l.ImagesPerDay)
	return err
}

// SetLimits changes a key's limits.
func (s *Store) SetLimits(id int64, l Limits, a Audit) error {
	return s.tx(func(tx *sql.Tx) error {
		var scope string
		err := tx.QueryRow(`SELECT scope FROM tokens WHERE id = ?`, id).Scan(&scope)
		if errors.Is(err, sql.ErrNoRows) || err == nil && scope != KeyScope {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := setLimits(tx, id, l); err != nil {
			return err
		}
		return addAudit(tx, a)
	})
}

// Keys lists the API keys, with their limits.
func (s *Store) Keys() ([]Key, error) {
	rows, err := s.db.Query(`SELECT t.id, t.name, t.scope, t.created_by, t.created_at, t.expires_at,
		COALESCE(l.apis, ''), COALESCE(l.max_concurrent, 0), COALESCE(l.max_body, 0), COALESCE(l.tokens_per_day, 0),
		COALESCE(l.images_per_day, 0) FROM tokens t LEFT JOIN key_limits l ON l.token_id = t.id
		WHERE t.scope = 'key' ORDER BY t.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Key{}
	for rows.Next() {
		var k Key
		var expires sql.NullString
		var apis string
		if err := rows.Scan(&k.ID, &k.Name, &k.Scope, &k.CreatedBy, &k.CreatedAt, &expires, &apis, &k.Limits.MaxConcurrent,
			&k.Limits.MaxBody, &k.Limits.TokensPerDay, &k.Limits.ImagesPerDay); err != nil {
			return nil, err
		}
		if expires.Valid {
			k.ExpiresAt = &expires.String
		}
		scanLimits(apis, &k.Limits)
		list = append(list, k)
	}
	return list, rows.Err()
}

// KeyLimits is a key's limits, or none for one made before them.
func (s *Store) KeyLimits(id int64) (Limits, error) {
	var l Limits
	var apis string
	err := s.db.QueryRow(`SELECT apis, max_concurrent, max_body, tokens_per_day, images_per_day FROM key_limits
		WHERE token_id = ?`, id).Scan(&apis, &l.MaxConcurrent, &l.MaxBody, &l.TokensPerDay, &l.ImagesPerDay)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	scanLimits(apis, &l)
	return l, err
}

// APIRequest is a line of the API platform's log: who, which API, and how it went. Never what was asked or answered.
// KeyID is 0 for a person logged in to the console, and Key their name.
type APIRequest struct {
	ID               int64  `json:"id"`
	At               string `json:"at"`
	KeyID            int64  `json:"key_id"`
	Key              string `json:"key"`
	API              string `json:"api"`
	Endpoint         string `json:"endpoint"`
	Model            string `json:"model"`
	Status           int    `json:"status"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	Images           int64  `json:"images"`
	Millis           int64  `json:"millis"`
	Chunks           int64  `json:"-"` // a stream's events, as the gateway counts them
}

// The request log is kept for 30 days, and to this many lines, about 10 MB. Each key's totals for a day are kept
// apart, for 30 days whatever the log's size: a flood of cheap requests can push others out of the log, but not out of
// what a key has used today.
const (
	RequestDays  = 30
	requestsKeep = 60000
)

// Day is the day a request counts towards, in UTC.
func Day(at time.Time) string { return at.UTC().Format("2006-01-02") }

// AddAPIRequests records a batch of requests, oldest first, with each key's totals, after dropping what's past
// keeping, in a transaction of its own: a store that's full can still make room.
func (s *Store) AddAPIRequests(list []APIRequest) error {
	if len(list) == 0 {
		return nil
	}
	if err := s.tx(func(tx *sql.Tx) error {
		cutoff := Day(time.Now().AddDate(0, 0, -RequestDays))
		if _, err := tx.Exec(`DELETE FROM api_requests WHERE id <= (SELECT COALESCE(MAX(id), 0) FROM api_requests) - ?`,
			requestsKeep-len(list)); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM api_requests WHERE day < ?`, cutoff); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM api_usage WHERE day < ?`, cutoff)
		return err
	}); err != nil {
		return err
	}
	return s.tx(func(tx *sql.Tx) error {
		for _, r := range list {
			day := r.At
			if at, err := time.Parse(time.RFC3339Nano, r.At); err == nil {
				day = Day(at)
			}
			if _, err := tx.Exec(`INSERT INTO api_requests (at, day, key_id, key_name, api, endpoint, model, status,
				prompt_tokens, completion_tokens, images, millis) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				r.At, day, r.KeyID, r.Key, r.API, r.Endpoint, r.Model, r.Status, r.PromptTokens, r.CompletionTokens,
				r.Images, r.Millis); err != nil {
				return err
			}
			failed := 0
			if r.Status >= 400 {
				failed = 1
			}
			if _, err := tx.Exec(`INSERT INTO api_usage (day, key_id, key_name, requests, errors, prompt_tokens,
				completion_tokens, images, millis) VALUES (?, ?, ?, 1, ?, ?, ?, ?, ?) ON CONFLICT (day, key_id) DO UPDATE SET
				key_name = excluded.key_name, requests = requests + 1, errors = errors + excluded.errors,
				prompt_tokens = prompt_tokens + excluded.prompt_tokens, completion_tokens = completion_tokens + excluded.completion_tokens,
				images = images + excluded.images, millis = millis + excluded.millis`,
				day, r.KeyID, r.Key, failed, r.PromptTokens, r.CompletionTokens, r.Images, r.Millis); err != nil {
				return err
			}
		}
		return nil
	})
}

// APIRequests lists the log, newest first, before an id.
func (s *Store) APIRequests(limit int, before int64) ([]APIRequest, error) {
	if before <= 0 {
		before = 1 << 62
	}
	rows, err := s.db.Query(`SELECT id, at, key_id, key_name, api, endpoint, model, status, prompt_tokens,
		completion_tokens, images, millis FROM api_requests WHERE id < ? ORDER BY id DESC LIMIT ?`, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []APIRequest{}
	for rows.Next() {
		var r APIRequest
		if err := rows.Scan(&r.ID, &r.At, &r.KeyID, &r.Key, &r.API, &r.Endpoint, &r.Model, &r.Status, &r.PromptTokens,
			&r.CompletionTokens, &r.Images, &r.Millis); err != nil {
			return nil, err
		}
		list = append(list, r)
	}
	return list, rows.Err()
}

// Usage is a day's use by one key: requests, those that failed, tokens, images and time taken.
type Usage struct {
	Day              string `json:"day"`
	KeyID            int64  `json:"key_id"`
	Key              string `json:"key"`
	Requests         int64  `json:"requests"`
	Errors           int64  `json:"errors"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	Images           int64  `json:"images"`
	Millis           int64  `json:"millis"`
}

// UsageSince is each key's use by day, from that day on, newest first.
func (s *Store) UsageSince(day string) ([]Usage, error) {
	rows, err := s.db.Query(`SELECT day, key_id, key_name, requests, errors, prompt_tokens, completion_tokens, images,
		millis FROM api_usage WHERE day >= ? ORDER BY day DESC, key_id`, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Usage{}
	for rows.Next() {
		var u Usage
		if err := rows.Scan(&u.Day, &u.KeyID, &u.Key, &u.Requests, &u.Errors, &u.PromptTokens, &u.CompletionTokens,
			&u.Images, &u.Millis); err != nil {
			return nil, err
		}
		list = append(list, u)
	}
	return list, rows.Err()
}

// UsedOn is what a key has used on a day, in tokens and images, for its limits.
func (s *Store) UsedOn(day string, keyID int64) (tokens, images int64, err error) {
	err = s.db.QueryRow(`SELECT COALESCE(SUM(prompt_tokens + completion_tokens), 0), COALESCE(SUM(images), 0)
		FROM api_usage WHERE day = ? AND key_id = ?`, day, keyID).Scan(&tokens, &images)
	return tokens, images, err
}
