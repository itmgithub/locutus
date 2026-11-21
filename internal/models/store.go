// Package models contains the domain types of Locutus and their persistence
// in SQLite (the "model" part of MVC).
package models

import (
	"database/sql"
	"errors"
	"time"

	"itmon.nl/locutus/internal/secrets"
)

// ErrNotFound is returned when a requested record does not exist.
var ErrNotFound = errors.New("not found")

// Store gives access to all persisted data.
type Store struct {
	db  *sql.DB
	box *secrets.Box
}

// NewStore wraps an opened database. box encrypts secret columns.
func NewStore(db *sql.DB, box *secrets.Box) *Store {
	return &Store{db: db, box: box}
}

func now() int64 { return time.Now().Unix() }

func unixTime(v int64) time.Time {
	if v <= 0 {
		return time.Time{}
	}
	return time.Unix(v, 0)
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// nullID converts an optional id (0 meaning "none") to a SQL value.
func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}
