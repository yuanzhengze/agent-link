package adapter

import (
	"fmt"
	"os/exec"
	"strings"
)

// ClaudeCodeLauncher implements AgentLauncher for Claude Code.
type ClaudeCodeLauncher struct{}

func (l *ClaudeCodeLauncher) Command() (name string, args []string) {
	return "claude", []string{"--dangerously-skip-permissions"}
}

// ResumeArgs returns args for resuming a prior Claude Code session.
// An empty sessionID triggers the --continue fallback (23c) for configs
// created before session_id recording.
func (l *ClaudeCodeLauncher) ResumeArgs(sessionID string) []string {
	if sessionID == "" {
		return []string{"--continue", "--dangerously-skip-permissions"}
	}
	return []string{"--resume", sessionID, "--dangerously-skip-permissions"}
}

func (l *ClaudeCodeLauncher) CheckPrereqs() error {
	var missing []string
	for _, cmd := range []string{"tmux", "claude"} {
		if _, err := exec.LookPath(cmd); err != nil {
			missing = append(missing, cmd)
		}
	}
	if len(missing) > 0 {
		msg := "require " + strings.Join(missing, " and ") + " to be installed"
		return fmt.Errorf("%s", msg)
	}
	return nil
}

// coworkLockRules documents the cowork file-lock protocol that agents must
// follow before editing files in a shared project: acquire a lock, handle
// 409 (already held) by backing off, then release when done.
const coworkLockRules = `## 协同写锁规则（cowork）
- 修改任何文件前，先执行：agentlink lock acquire <project> <相对路径>
- 若返回 409（被占用），改去做别的文件，或稍后重试；不要强行修改。
- 改完后执行：agentlink lock release <project> <相对路径>
- 不要绕过锁直接改文件——服务器会拒绝未持锁的写入（apply 409）。
`

func (l *ClaudeCodeLauncher) InitTemplate(session string, device string) string {
	return fmt.Sprintf(
		"You are agentlink device **%s**, session **%s** on the team network.\n"+
			"When involving agent collaboration network, run `agentlink whoami` first.\n"+
			"\n%s",
		device, session, coworkLockRules,
	)
}

// ClaudeCodeDetector implements IdleDetector for Claude Code.
type ClaudeCodeDetector struct{}

// IsBusy returns true when the pane content shows "esc to interrupt",
// indicating Claude is currently generating or running a tool.
func (d *ClaudeCodeDetector) IsBusy(paneContent string) bool {
	return strings.Contains(paneContent, "esc to interrupt")
}

// IsPromptEmpty scans the pane from bottom to find the last line
// containing "❯". If nothing (or only whitespace) follows the "❯",
// the input box is empty.
//
// Returns false when no "❯" is found at all (Claude likely not ready).
func (d *ClaudeCodeDetector) IsPromptEmpty(paneContent string) bool {
	lines := strings.Split(paneContent, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		pos := strings.LastIndex(lines[i], "❯")
		if pos < 0 {
			continue
		}
		// Found a line containing ❯. If nothing (or only whitespace)
		// follows the last ❯ on that line, the prompt is empty.
		rest := strings.TrimSpace(lines[i][pos+len("❯"):])
		return rest == ""
	}
	return false
}
