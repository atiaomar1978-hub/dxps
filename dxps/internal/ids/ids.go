// Package ids generates DxPS identifiers: UUIDv7 for new rows/messages and
// deterministic UUIDv5 values derived from client idempotency keys.
package ids

import (
	"regexp"

	"github.com/google/uuid"
)

// Namespace for deterministic DxPS identifiers.
var Namespace = uuid.MustParse("6d2b1c3e-8f4a-5b7c-9d0e-1f2a3b4c5d6e")

var idemPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{8,64}$`)

func New() uuid.UUID { return uuid.Must(uuid.NewV7()) }

func NewString() string { return New().String() }

// Derive returns a stable UUIDv5 for the given parts (joined with 0x1f, which cannot occur in validated inputs).
func Derive(parts ...string) uuid.UUID {
	b := make([]byte, 0, 128)
	for i, p := range parts {
		if i > 0 {
			b = append(b, 0x1f)
		}
		b = append(b, p...)
	}
	return uuid.NewSHA1(Namespace, b)
}

func ValidIdempotencyKey(k string) bool { return idemPattern.MatchString(k) }

func Parse(s string) (uuid.UUID, error) { return uuid.Parse(s) }

// ValidUUID reports whether s is a canonical 36-char UUID.
func ValidUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	_, err := uuid.Parse(s)
	return err == nil
}
