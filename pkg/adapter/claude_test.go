package adapter

import (
	"strings"
	"testing"
)

// Test data is based on real tmux capture-pane output observed from
// Claude Code v2.1.119 running with --dangerously-skip-permissions.

var (
	sep        = "────────────────────────────────────────────────────────────────────────────────"
	statusIdle = "  ⏵⏵ bypass permissions on (shift+tab to cycle)                ◈ max · /effort"
	statusBusy = "  ⏵⏵ bypass permissions on (shift+tab to cycle) · esc to interrupt · ctrl+t to hide tasks"
)

func TestClaudeCodeDetector_IsBusy(t *testing.T) {
	d := &ClaudeCodeDetector{}

	tests := []struct {
		name string
		pane string
		busy bool
	}{
		{
			name: "empty pane",
			pane: "",
			busy: false,
		},
		{
			name: "idle — bare prompt",
			pane: sep + "\n❯\n" + sep + "\n" + statusIdle + "\n",
			busy: false,
		},
		{
			name: "typing — text after prompt",
			pane: sep + "\n❯ some typed message\n" + sep + "\n" + statusIdle + "\n",
			busy: false,
		},
		{
			name: "busy — esc to interrupt in status bar",
			pane: sep + "\n❯\n" + sep + "\n" + statusBusy + "\n",
			busy: true,
		},
		{
			name: "busy — esc to interrupt anywhere",
			pane: "some output\nesc to interrupt\nmore output\n",
			busy: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := d.IsBusy(tt.pane)
			if got != tt.busy {
				t.Errorf("IsBusy() = %v, want %v", got, tt.busy)
			}
		})
	}
}

func TestClaudeCodeDetector_IsPromptEmpty(t *testing.T) {
	d := &ClaudeCodeDetector{}

	tests := []struct {
		name        string
		pane        string
		promptEmpty bool
	}{
		{
			name:        "empty pane",
			pane:        "",
			promptEmpty: false, // no ❯ found → not ready
		},
		{
			name:        "only newlines",
			pane:        "\n\n\n\n",
			promptEmpty: false,
		},
		{
			name:        "idle — bare prompt",
			pane:        sep + "\n❯\n" + sep + "\n" + statusIdle + "\n",
			promptEmpty: true,
		},
		{
			name:        "idle — prompt with trailing whitespace",
			pane:        sep + "\n❯ \n" + sep + "\n" + statusIdle + "\n",
			promptEmpty: true,
		},
		{
			name:        "idle — busy but prompt empty",
			pane:        sep + "\n❯\n" + sep + "\n" + statusBusy + "\n",
			promptEmpty: true, // prompt is empty even though Claude is busy
		},
		{
			name:        "typing — text after prompt",
			pane:        sep + "\n❯ some typed message\n" + sep + "\n" + statusIdle + "\n",
			promptEmpty: false,
		},
		{
			name:        "typing — short command",
			pane:        sep + "\n❯ agentlink pull\n" + sep + "\n" + statusIdle + "\n",
			promptEmpty: false,
		},
		{
			name:        "claude output — no prompt visible",
			pane:        "Here is some code:\nfunc foo() {\n  return 1\n}\n",
			promptEmpty: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := d.IsPromptEmpty(tt.pane)
			if got != tt.promptEmpty {
				t.Errorf("IsPromptEmpty() = %v, want %v", got, tt.promptEmpty)
			}
		})
	}
}

func TestClaudeCodeLauncher_ResumeArgs(t *testing.T) {
	l := &ClaudeCodeLauncher{}

	t.Run("with session_id", func(t *testing.T) {
		args := l.ResumeArgs("abc-123")
		if len(args) != 3 || args[0] != "--resume" || args[1] != "abc-123" || args[2] != "--dangerously-skip-permissions" {
			t.Errorf("unexpected args: %v", args)
		}
	})

	t.Run("empty session_id triggers continue fallback", func(t *testing.T) {
		args := l.ResumeArgs("")
		if len(args) != 2 || args[0] != "--continue" || args[1] != "--dangerously-skip-permissions" {
			t.Errorf("unexpected fallback args: %v", args)
		}
	})
}

func TestClaudeInitTemplate_containsLockRules(t *testing.T) {
	l := &ClaudeCodeLauncher{}
	got := l.InitTemplate("main", "dev1")

	for _, want := range []string{"lock acquire", "lock release", "409"} {
		if !strings.Contains(got, want) {
			t.Errorf("InitTemplate() missing %q; got:\n%s", want, got)
		}
	}

	// The original identity line must survive the append.
	if !strings.Contains(got, "dev1") || !strings.Contains(got, "main") {
		t.Errorf("InitTemplate() lost device/session identity; got:\n%s", got)
	}
}
