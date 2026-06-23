package repo

import (
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// nowMs returns the current time as epoch milliseconds, matching the bigint
// timestamp convention used throughout the original schema.
func nowMs() int64 { return time.Now().UnixMilli() }

// isNoRows reports whether err is pgx's "no rows" sentinel.
func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
