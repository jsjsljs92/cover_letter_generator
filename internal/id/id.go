// Package id generates short, collision-resistant identifiers for stored
// objects. It uses crypto/rand so identifiers cannot be guessed or enumerated.
package id

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// New returns a 16-character hex identifier (64 bits of entropy).
func New() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand failing is unrecoverable and should not be papered over
		// with a predictable fallback.
		panic(fmt.Sprintf("id: crypto/rand unavailable: %v", err))
	}
	return hex.EncodeToString(buf[:])
}
