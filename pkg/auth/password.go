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

	"golang.org/x/crypto/argon2"
)

const (
	argonMemory      = 64 * 1024
	argonIterations  = 3
	argonParallelism = 2
	argonSaltLength  = 16
	argonKeyLength   = 32
)

var usernamePattern = regexp.MustCompile(`^[a-z0-9_-]{3,32}$`)

func NormalizeUsername(username string) (string, error) {
	normalized := strings.ToLower(username)
	if !usernamePattern.MatchString(normalized) {
		return "", errors.New("username must be 3-32 ASCII letters, digits, underscores, or hyphens")
	}
	return normalized, nil
}

func HashPassword(password string) (string, error) {
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

	hash := argon2.IDKey(
		[]byte(password),
		params.salt,
		params.iterations,
		params.memory,
		params.parallelism,
		uint32(len(params.hash)),
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

	hash, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil {
		return passwordParams{}, fmt.Errorf("invalid password hash value: %w", err)
	}
	if len(hash) == 0 {
		return passwordParams{}, errors.New("password hash value is empty")
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
