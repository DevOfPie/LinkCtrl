package gate

import (
	"crypto/rand"
	"fmt"
)

// NewSecret returns fresh key material for a workspace.
//
// crypto/rand, and the error is returned rather than swallowed: a signing secret
// drawn from a source that failed would verify signatures nobody had to know a
// key to make.
func NewSecret() ([]byte, error) {
	b := make([]byte, SecretLength)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("gate: read signing secret: %w", err)
	}
	return b, nil
}
