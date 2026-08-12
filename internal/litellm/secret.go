package litellm

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

// KeyPrefix is the fixed prefix of every key the gateway issues.
const KeyPrefix = "sk-gw-"

// SecretRandomLength is the number of random characters after the consumer
// name. The contract requires at least 32 from a cryptographic source.
const SecretRandomLength = 40

// alphabet excludes look-alike characters so a key read off a screen or a log
// can be retyped without ambiguity.
const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// SecretPattern matches a well-formed gateway key.
var SecretPattern = regexp.MustCompile(`^sk-gw-[a-z0-9]+(-[a-z0-9]+)*-[a-zA-Z0-9]{32,}$`)

// NewSecret builds a key of the form sk-gw-<consumer>-<random>. The consumer
// name is part of the secret on purpose: a key found in someone else's log
// immediately identifies what has to be revoked.
func NewSecret(consumer string) (string, error) {
	if consumer == "" {
		return "", fmt.Errorf("consumer name is required")
	}

	var sb strings.Builder
	sb.WriteString(KeyPrefix)
	sb.WriteString(consumer)
	sb.WriteString("-")

	max := big.NewInt(int64(len(alphabet)))
	for i := 0; i < SecretRandomLength; i++ {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("read random source: %w", err)
		}
		sb.WriteByte(alphabet[n.Int64()])
	}
	return sb.String(), nil
}

// MaskSecret renders a key for display without disclosing it.
func MaskSecret(secret string) string {
	if len(secret) <= 12 {
		return "…"
	}
	return secret[:len(KeyPrefix)] + "…" + secret[len(secret)-4:]
}
