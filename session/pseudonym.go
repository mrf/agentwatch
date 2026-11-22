package session

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// pseudonymKeySize is both the generated key length and the minimum length
// accepted from WithPseudonymKey.
const pseudonymKeySize = 32

// pseudonymBytes is how much of the HMAC-SHA256 output a pseudonym keeps
// (128 bits), hex-encoded to a fixed 32-character string.
const pseudonymBytes = 16

// Pseudonymizer maps session identifiers to stable, opaque pseudonyms with
// HMAC-SHA256 under a secret key. The same identifier always maps to the same
// pseudonym for a given key, so clients can correlate snapshot, delta, and
// removal events without learning real IDs. Without the key a pseudonym
// cannot be reversed or recomputed.
//
// A Pseudonymizer is immutable and safe for concurrent use.
type Pseudonymizer struct {
	key []byte
}

// PseudonymizerOption configures a Pseudonymizer.
type PseudonymizerOption func(*Pseudonymizer)

// WithPseudonymKey sets the HMAC key instead of generating a random one. The
// key must be at least 32 bytes and should come from a CSPRNG; it is copied.
// Processes that share a key produce the same pseudonyms, which lets a
// multi-process deployment correlate sessions across them.
func WithPseudonymKey(key []byte) PseudonymizerOption {
	return func(z *Pseudonymizer) {
		z.key = append([]byte(nil), key...)
	}
}

// NewPseudonymizer returns a Pseudonymizer. Without WithPseudonymKey it uses
// a fresh 32-byte key from crypto/rand, so pseudonyms are stable only for the
// life of the Pseudonymizer.
func NewPseudonymizer(opts ...PseudonymizerOption) (*Pseudonymizer, error) {
	z := &Pseudonymizer{}
	for _, o := range opts {
		o(z)
	}
	if z.key == nil {
		z.key = make([]byte, pseudonymKeySize)
		if _, err := rand.Read(z.key); err != nil {
			return nil, fmt.Errorf("session: generate pseudonym key: %w", err)
		}
	}
	if len(z.key) < pseudonymKeySize {
		return nil, fmt.Errorf("session: pseudonym key is %d bytes, need at least %d", len(z.key), pseudonymKeySize)
	}
	return z, nil
}

// Pseudonym returns the pseudonym for id. An empty id stays empty so that
// "no parent" and "no slug" remain distinguishable.
func (z *Pseudonymizer) Pseudonym(id string) string {
	if id == "" {
		return ""
	}
	mac := hmac.New(sha256.New, z.key)
	mac.Write([]byte(id))
	return hex.EncodeToString(mac.Sum(nil)[:pseudonymBytes])
}
