// Package ids creates the unique IDs used as primary keys and references.
//
// We use ULIDs: 26 characters, unique, and they sort by creation time
// (newer IDs sort after older ones), which keeps database indexes fast.
package ids

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// New returns a new unique ID such as "01JB3Y1X4M8K2T6W0QF9ZC7R5D".
func New() string {
	return ulid.MustNew(ulid.Timestamp(time.Now()), rand.Reader).String()
}

// NewReference returns a human-friendly reference with a prefix, for
// example "SWP-01JB3Y1X4M8K2T6W0QF9ZC7R5D". Used for transactions.
func NewReference(prefix string) string {
	return strings.ToUpper(prefix) + "-" + New()
}

// RandomToken returns a long random secret as hex (used for refresh tokens).
func RandomToken(byteCount int) string {
	buffer := make([]byte, byteCount)
	if _, err := rand.Read(buffer); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buffer)
}

// RandomDigits returns a random number code such as "4821" (used for OTP codes).
func RandomDigits(length int) string {
	buffer := make([]byte, length)
	if _, err := rand.Read(buffer); err != nil {
		panic(err)
	}
	digits := make([]byte, length)
	for index, randomByte := range buffer {
		digits[index] = '0' + randomByte%10
	}
	return string(digits)
}

// FirstForTime returns the smallest possible ULID for a moment in time.
// ULIDs start with their creation time, so every id made at or after that
// moment sorts after this one. Used to say "rows created since then" with
// a primary-key range, which needs no extra index.
func FirstForTime(moment time.Time) string {
	var id ulid.ULID
	if err := id.SetTime(ulid.Timestamp(moment)); err != nil {
		return ulid.ULID{}.String() // a time before 1970: every id comes after it
	}
	return id.String()
}
