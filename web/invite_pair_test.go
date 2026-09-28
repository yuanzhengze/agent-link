package web

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestInvitePairParser locks the join-form paste parser: one block containing
// Team ID / Invite code (or 团队 ID / 邀请码) fills both fields.
func TestInvitePairParser(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	script := filepath.Join(filepath.Dir(file), "invite_pair_test.js")
	out, err := exec.Command(node, script).CombinedOutput()
	if err != nil {
		t.Fatalf("invite pair parser: %v\n%s", err, out)
	}
}
