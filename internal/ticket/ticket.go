// Package ticket provides queue ticket IDs and HMAC-signed cookie values.
package ticket

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

const version = "v1"

// NewID returns a 128-bit random hex identifier.
func NewID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating ticket id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// NewSecret returns a random secret suitable for a Signer.
func NewSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating cookie secret: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Signer signs and verifies cookie payloads with HMAC-SHA256.
type Signer struct {
	secret []byte
}

func NewSigner(secret string) *Signer {
	return &Signer{secret: []byte(secret)}
}

// Sign returns "v1:<payload>:<base64url(HMAC-SHA256("v1:<payload>"))>".
func (s *Signer) Sign(payload string) string {
	prefix := version + ":" + payload
	return prefix + ":" + s.mac(prefix)
}

// Verify parses a signed value and returns the payload if the signature is
// valid. Comparison is constant-time.
func (s *Signer) Verify(value string) (string, bool) {
	// Split off the signature (last segment); the payload itself may contain
	// colons.
	i := strings.LastIndex(value, ":")
	if i < 0 {
		return "", false
	}
	prefix, sig := value[:i], value[i+1:]
	if !strings.HasPrefix(prefix, version+":") {
		return "", false
	}
	if !hmac.Equal([]byte(sig), []byte(s.mac(prefix))) {
		return "", false
	}
	return strings.TrimPrefix(prefix, version+":"), true
}

func (s *Signer) mac(data string) string {
	h := hmac.New(sha256.New, s.secret)
	h.Write([]byte(data))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}
