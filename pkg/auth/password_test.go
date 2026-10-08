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
	for _, invalid := range []string{"ab", "has space", "中文名", "Kirby", "a/b", strings.Repeat("a", 33)} {
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
	encoded, err := HashPassword("password10")
	if err != nil {
		t.Fatal(err)
	}
	const prefix = "$argon2id$v=19$m=65536,t=3,p=2$"
	if !strings.HasPrefix(encoded, prefix) {
		t.Fatalf("HashPassword() = %q; want prefix %q", encoded, prefix)
	}
}

func TestHashPasswordRejectsShortPasswords(t *testing.T) {
	for _, password := range []string{
		"shortpass",
		"密码密码密码密码密",
	} {
		if _, err := HashPassword(password); err == nil || !strings.Contains(err.Error(), "at least 10") {
			t.Errorf("HashPassword(%q) error = %v; want a minimum-length error", password, err)
		}
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

func TestVerifyPasswordRejectsResourceLimits(t *testing.T) {
	salt := make([]byte, 16)
	hash := make([]byte, 16)
	tests := []struct {
		name        string
		encoded     string
		wantErrPart string
	}{
		{
			name:        "PHC length",
			encoded:     encodeTestPHC(64, 1, 1, salt, make([]byte, 350)),
			wantErrPart: "password hash exceeds maximum length",
		},
		{
			name:        "memory",
			encoded:     encodeTestPHC(4294967295, 1, 1, salt, hash),
			wantErrPart: "argon2 memory exceeds maximum",
		},
		{
			name:        "iterations",
			encoded:     encodeTestPHC(64, 11, 1, salt, hash),
			wantErrPart: "argon2 iterations exceeds maximum",
		},
		{
			name:        "parallelism",
			encoded:     encodeTestPHC(136, 1, 17, salt, hash),
			wantErrPart: "argon2 parallelism exceeds maximum",
		},
		{
			name:        "salt length",
			encoded:     encodeTestPHC(64, 1, 1, make([]byte, 65), hash),
			wantErrPart: "password hash salt exceeds maximum length",
		},
		{
			name:        "key length",
			encoded:     encodeTestPHC(64, 1, 1, salt, make([]byte, 65)),
			wantErrPart: "password hash value exceeds maximum length",
		},
	}

	if len(tests[0].encoded) <= 512 {
		t.Fatalf("PHC length regression fixture is only %d bytes", len(tests[0].encoded))
	}

	// Keep the bounded-cost PHC-length case first so the pre-fix RED run
	// cannot reach the oversized-memory case and attempt a huge allocation.
	for _, test := range tests {
		ok, err := VerifyPassword(test.encoded, "password")
		if err == nil {
			t.Fatalf("%s: VerifyPassword() = %v, nil; want an error", test.name, ok)
		}
		if ok || !strings.Contains(err.Error(), test.wantErrPart) {
			t.Fatalf("%s: VerifyPassword() = %v, %v; want false and error containing %q", test.name, ok, err, test.wantErrPart)
		}
	}
}

func encodeTestPHC(memory, iterations, parallelism uint64, salt, hash []byte) string {
	return fmt.Sprintf(
		"$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		memory,
		iterations,
		parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash),
	)
}
