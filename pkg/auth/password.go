package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

const (
	argonMemory      = 64 * 1024
	argonIterations  = 3
	argonParallelism = 2
	argonSaltLength  = 16
	argonKeyLength   = 32

	minPasswordLength   = 10
	maxPHCLength        = 512
	maxArgonMemory      = 256 * 1024
	maxArgonIterations  = 10
	maxArgonParallelism = 16
	maxSaltLength       = 64
	maxKeyLength        = 64
)

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{3,32}$`)

func NormalizeUsername(username string) (string, error) {
	if !usernamePattern.MatchString(username) {
		return "", errors.New("username must be 3-32 ASCII letters, digits, underscores, or hyphens")
	}
	return strings.ToLower(username), nil
}

func HashPassword(password string) (string, error) {
	if utf8.RuneCountInString(password) < minPasswordLength {
		return "", fmt.Errorf("password must be at least %d characters", minPasswordLength)
	}

	salt := make([]byte, argonSaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}

	hash := argon2.IDKey(
		[]byte(password),
		salt,
		argonIterations,
		argonMemory,
		argonParallelism,
		argonKeyLength,
	)

	return fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		argonMemory,
		argonIterations,
		argonParallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash),
	), nil
}

func VerifyPassword(encoded, password string) (bool, error) {
	params, err := parsePasswordPHC(encoded)
	if err != nil {
		return false, err
	}

	hashLength := len(params.hash)
	if uint64(hashLength) > uint64(^uint32(0)) {
		return false, errors.New("password hash value is too long")
	}

	hash := argon2.IDKey(
		[]byte(password),
		params.salt,
		params.iterations,
		params.memory,
		params.parallelism,
		uint32(hashLength),
	)
	return subtle.ConstantTimeCompare(hash, params.hash) == 1, nil
}

type passwordParams struct {
	memory      uint32
	iterations  uint32
	parallelism uint8
	salt        []byte
	hash        []byte
}

func parsePasswordPHC(encoded string) (passwordParams, error) {
	if len(encoded) > maxPHCLength {
		return passwordParams{}, errors.New("password hash exceeds maximum length")
	}

	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" {
		return passwordParams{}, errors.New("invalid password hash format")
	}
	if parts[1] != "argon2id" {
		return passwordParams{}, errors.New("invalid password hash algorithm")
	}

	version, err := parsePHCValue(parts[2], "v", 32)
	if err != nil {
		return passwordParams{}, fmt.Errorf("invalid password hash version: %w", err)
	}
	if version != argon2.Version {
		return passwordParams{}, fmt.Errorf("unsupported argon2 version %d", version)
	}

	fields := strings.Split(parts[3], ",")
	if len(fields) != 3 {
		return passwordParams{}, errors.New("invalid password hash parameters")
	}

	memory, err := parsePHCValue(fields[0], "m", 32)
	if err != nil {
		return passwordParams{}, fmt.Errorf("invalid argon2 memory: %w", err)
	}
	iterations, err := parsePHCValue(fields[1], "t", 32)
	if err != nil {
		return passwordParams{}, fmt.Errorf("invalid argon2 iterations: %w", err)
	}
	parallelism, err := parsePHCValue(fields[2], "p", 8)
	if err != nil {
		return passwordParams{}, fmt.Errorf("invalid argon2 parallelism: %w", err)
	}
	if memory > maxArgonMemory {
		return passwordParams{}, errors.New("argon2 memory exceeds maximum")
	}
	if iterations > maxArgonIterations {
		return passwordParams{}, errors.New("argon2 iterations exceeds maximum")
	}
	if parallelism > maxArgonParallelism {
		return passwordParams{}, errors.New("argon2 parallelism exceeds maximum")
	}
	if memory == 0 || iterations == 0 || parallelism == 0 {
		return passwordParams{}, errors.New("argon2 parameters must be positive")
	}
	if memory < 8*parallelism {
		return passwordParams{}, errors.New("argon2 memory is too small for parallelism")
	}

	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil {
		return passwordParams{}, fmt.Errorf("invalid password hash salt: %w", err)
	}
	if len(salt) == 0 {
		return passwordParams{}, errors.New("password hash salt is empty")
	}
	if len(salt) > maxSaltLength {
		return passwordParams{}, errors.New("password hash salt exceeds maximum length")
	}

	hash, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil {
		return passwordParams{}, fmt.Errorf("invalid password hash value: %w", err)
	}
	if len(hash) == 0 {
		return passwordParams{}, errors.New("password hash value is empty")
	}
	if len(hash) > maxKeyLength {
		return passwordParams{}, errors.New("password hash value exceeds maximum length")
	}

	return passwordParams{
		memory:      uint32(memory),
		iterations:  uint32(iterations),
		parallelism: uint8(parallelism),
		salt:        salt,
		hash:        hash,
	}, nil
}

func parsePHCValue(field, name string, bitSize int) (uint64, error) {
	prefix := name + "="
	if !strings.HasPrefix(field, prefix) || len(field) == len(prefix) {
		return 0, errors.New("missing parameter")
	}
	return strconv.ParseUint(strings.TrimPrefix(field, prefix), 10, bitSize)
}
