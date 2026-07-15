package auth

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
)

const identifierBytes = 16

var identifierEncoding = base32.NewEncoding("abcdefghjklmnpqrstuvwxyz23456789").
	WithPadding(base32.NoPadding)

func NewUserID() (string, error) {
	return newIdentifier("usr_")
}

func NewTeamID() (string, error) {
	return newIdentifier("tm_")
}

func NewDeviceID() (string, error) {
	return newIdentifier("dev_")
}

func NewInviteCode() (string, error) {
	return NewSecret("inv_", 32)
}

func NewSecret(prefix string, bytes int) (string, error) {
	if bytes <= 0 {
		return "", errors.New("secret byte count must be positive")
	}

	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate secret: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(value), nil
}

func newIdentifier(prefix string) (string, error) {
	value := make([]byte, identifierBytes)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate identifier: %w", err)
	}
	return prefix + identifierEncoding.EncodeToString(value), nil
}
