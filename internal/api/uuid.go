package api

import (
	"database/sql"
	"strings"

	"github.com/google/uuid"
)

// uuidType is an alias so handler files don't need to import uuid directly
// for the type name — they only call parseUUID.
type uuidType = uuid.UUID

// parseUUID wraps uuid.Parse with a leaner call site in handlers.
func parseUUID(s string) (uuid.UUID, error) {
	return uuid.Parse(s)
}

// nullString converts a Go string to sql.NullString.
// Empty/whitespace-only strings become NULL, matching the HTTP layer behaviour.
func nullString(s string) sql.NullString {
	s = strings.TrimSpace(s)
	return sql.NullString{String: s, Valid: s != ""}
}