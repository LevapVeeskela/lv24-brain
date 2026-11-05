package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	argon2idAlgorithm = "argon2id"
	passwordSaltLen   = 16
	passwordKeyLen    = 32
	// MinPasswordLength is the smallest accepted local password.
	MinPasswordLength = 10
	// MaxPasswordLength bounds hashing work for untrusted input.
	MaxPasswordLength = 1024

	maxPasswordMemory      = 1024 * 1024
	maxPasswordIterations  = 10
	maxPasswordParallelism = 16
)

// Argon2Params controls Argon2id work factors.
type Argon2Params struct {
	Memory      uint32
	Iterations  uint32
	Parallelism uint8
}

// DefaultArgon2Params follows the OWASP baseline for interactive logins.
var DefaultArgon2Params = Argon2Params{
	Memory:      19 * 1024,
	Iterations:  2,
	Parallelism: 1,
}

var errInvalidHash = errors.New("invalid password hash")

// ValidatePassword checks the local password policy.
func ValidatePassword(password string) error {
	if len(password) < MinPasswordLength {
		return fmt.Errorf("password must be at least %d characters", MinPasswordLength)
	}
	if len(password) > MaxPasswordLength {
		return fmt.Errorf("password must be at most %d bytes", MaxPasswordLength)
	}
	return nil
}

// HashPassword derives an Argon2id hash encoded as
// $argon2id$v=19$m=...,t=...,p=...$salt$hash.
func HashPassword(password string, params Argon2Params) (string, error) {
	if password == "" {
		return "", errors.New("password is required")
	}
	if len(password) > MaxPasswordLength {
		return "", fmt.Errorf("password must be at most %d bytes", MaxPasswordLength)
	}
	if err := params.validate(); err != nil {
		return "", err
	}
	salt := make([]byte, passwordSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, params.Iterations, params.Memory, params.Parallelism, passwordKeyLen)
	return fmt.Sprintf(
		"$%s$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2idAlgorithm,
		argon2.Version,
		params.Memory,
		params.Iterations,
		params.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword checks password against an encoded Argon2id hash. It returns
// false with a nil error for a normal password mismatch.
func VerifyPassword(encoded, password string) (bool, error) {
	parsed, err := decodePasswordHash(encoded)
	if err != nil {
		return false, err
	}
	if len(password) > MaxPasswordLength {
		return false, nil
	}
	key := argon2.IDKey([]byte(password), parsed.salt, parsed.iterations, parsed.memory, parsed.parallelism, uint32(len(parsed.hash)))
	return subtle.ConstantTimeCompare(key, parsed.hash) == 1, nil
}

// HashNeedsRehash reports whether an encoded hash uses weaker parameters than
// the supplied policy.
func HashNeedsRehash(encoded string, params Argon2Params) bool {
	parsed, err := decodePasswordHash(encoded)
	if err != nil {
		return true
	}
	return parsed.memory < params.Memory ||
		parsed.iterations < params.Iterations ||
		parsed.parallelism < params.Parallelism
}

type passwordHash struct {
	memory      uint32
	iterations  uint32
	parallelism uint8
	salt        []byte
	hash        []byte
}

func decodePasswordHash(encoded string) (passwordHash, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != argon2idAlgorithm {
		return passwordHash{}, errInvalidHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return passwordHash{}, errInvalidHash
	}
	var memory uint32
	var iterations uint32
	var parallelism uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &parallelism); err != nil {
		return passwordHash{}, errInvalidHash
	}
	if memory < 8 || memory > maxPasswordMemory || iterations < 1 || iterations > maxPasswordIterations ||
		parallelism < 1 || parallelism > maxPasswordParallelism {
		return passwordHash{}, errInvalidHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 {
		return passwordHash{}, errInvalidHash
	}
	hash, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(hash) < 16 {
		return passwordHash{}, errInvalidHash
	}
	return passwordHash{
		memory:      memory,
		iterations:  iterations,
		parallelism: parallelism,
		salt:        salt,
		hash:        hash,
	}, nil
}

func (p Argon2Params) validate() error {
	if p.Memory < 8 || p.Memory > maxPasswordMemory {
		return fmt.Errorf("argon2 memory must be between 8 and %d KiB", maxPasswordMemory)
	}
	if p.Iterations < 1 || p.Iterations > maxPasswordIterations {
		return fmt.Errorf("argon2 iterations must be between 1 and %d", maxPasswordIterations)
	}
	if p.Parallelism < 1 || p.Parallelism > maxPasswordParallelism {
		return fmt.Errorf("argon2 parallelism must be between 1 and %d", maxPasswordParallelism)
	}
	return nil
}

// FormatArgon2Params is used in diagnostics without exposing salts or hashes.
func FormatArgon2Params(params Argon2Params) string {
	return fmt.Sprintf("m=%d,t=%d,p=%d", params.Memory, params.Iterations, params.Parallelism)
}
