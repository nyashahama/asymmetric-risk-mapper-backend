package api

import (
	"github.com/google/uuid"
)

// uuidType is an alias so handler files don't import uuid directly.
type uuidType = uuid.UUID

// uuidParse wraps uuid.Parse.
func uuidParse(s string) (uuid.UUID, error) {
	return uuid.Parse(s)
}