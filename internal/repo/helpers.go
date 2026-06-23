package repo

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"gorm.io/gorm"
)

// nowMs returns the current time as epoch milliseconds, matching the bigint
// timestamp convention used throughout the original schema.
func nowMs() int64 { return time.Now().UnixMilli() }

// isNoRows reports whether err is a "record not found" sentinel. It checks both
// GORM's sentinel (used by First/Find) and database/sql's sql.ErrNoRows (returned
// by raw .Row().Scan() on some drivers, e.g. SQLite via GORM).
func isNoRows(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, sql.ErrNoRows)
}

// jsonMarshal is a thin wrapper used by the repos to serialize JSON columns.
func jsonMarshal(v interface{}) ([]byte, error) { return json.Marshal(v) }
