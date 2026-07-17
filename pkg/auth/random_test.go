package auth

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestGeneratedIdentifiers(t *testing.T) {
	userID, _ := NewUserID()
	teamID, _ := NewTeamID()
	secret, _ := NewSecret("ds_", 32)
	if !strings.HasPrefix(userID, "usr_") || !strings.HasPrefix(teamID, "tm_") {
		t.Fatalf("unexpected IDs: %q %q", userID, teamID)
	}
	if !strings.HasPrefix(secret, "ds_") || len(secret) < 40 {
		t.Fatalf("unexpected secret shape: %q", secret)
	}
}

func TestTeamIDUsesUnambiguousLowercaseBase32(t *testing.T) {
	teamID, err := NewTeamID()
	if err != nil {
		t.Fatal(err)
	}
	encoded := strings.TrimPrefix(teamID, "tm_")
	const alphabet = "abcdefghjklmnpqrstuvwxyz23456789"
	if encoded == "" {
		t.Fatal("NewTeamID() returned an empty encoded value")
	}
	for _, character := range encoded {
		if !strings.ContainsRune(alphabet, character) {
			t.Fatalf("NewTeamID() = %q; character %q is outside the ID alphabet", teamID, character)
		}
	}
}

func TestNewSecretEncodesRequestedBytes(t *testing.T) {
	secret, err := NewSecret("ds_", 32)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(secret, "ds_"))
	if err != nil {
		t.Fatalf("NewSecret() = %q: %v", secret, err)
	}
	if len(decoded) != 32 {
		t.Fatalf("NewSecret() encoded %d bytes; want 32", len(decoded))
	}
}
