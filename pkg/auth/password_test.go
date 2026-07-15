package auth

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"
)

func TestNormalizeUsername(t *testing.T) {
	tests := map[string]string{
		"Kirby": "kirby",
		"pm_01": "pm_01",
		"A-b":   "a-b",
	}
	for in, want := range tests {
		got, err := NormalizeUsername(in)
		if err != nil || got != want {
			t.Fatalf("NormalizeUsername(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, invalid := range []string{"ab", "has space", "中文名", "a/b", strings.Repeat("a", 33)} {
		if _, err := NormalizeUsername(invalid); err == nil {
			t.Errorf("NormalizeUsername(%q) unexpectedly succeeded", invalid)
		}
	}
}

func TestHashAndVerifyPassword(t *testing.T) {
	encoded, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := VerifyPassword(encoded, "correct horse battery staple"); err != nil || !ok {
		t.Fatalf("verify correct password = %v, %v", ok, err)
	}
	if ok, err := VerifyPassword(encoded, "wrong password"); err != nil || ok {
		t.Fatalf("verify wrong password = %v, %v", ok, err)
	}
}

func TestHashPasswordUsesRequiredParameters(t *testing.T) {
	encoded, err := HashPassword("password")
	if err != nil {
		t.Fatal(err)
	}
	const prefix = "$argon2id$v=19$m=65536,t=3,p=2$"
	if !strings.HasPrefix(encoded, prefix) {
		t.Fatalf("HashPassword() = %q; want prefix %q", encoded, prefix)
	}
}

func TestVerifyPasswordParsesEncodedParameters(t *testing.T) {
	const password = "password"
	salt := []byte("fixed test salt!")
	hash := argon2.IDKey([]byte(password), salt, 1, 64, 1, 16)
	encoded := fmt.Sprintf(
		"$argon2id$v=19$m=64,t=1,p=1$%s$%s",
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash),
	)

	ok, err := VerifyPassword(encoded, password)
	if err != nil || !ok {
		t.Fatalf("VerifyPassword() = %v, %v; want true, nil", ok, err)
	}
}

func TestVerifyPasswordRejectsMalformedEncoding(t *testing.T) {
	for _, encoded := range []string{
		"",
		"$argon2i$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA",
		"$argon2id$v=18$m=65536,t=3,p=2$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=65536,t=3$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=x,t=3,p=2$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=65536,t=3,p=2$%%%$aGFzaA",
		"$argon2id$v=19$m=65536,t=3,p=2$$aGFzaA",
		"$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$",
	} {
		if ok, err := VerifyPassword(encoded, "password"); err == nil || ok {
			t.Errorf("VerifyPassword(%q) = %v, %v; want false and an error", encoded, ok, err)
		}
	}
}
