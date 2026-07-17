package rt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/team/agentlink/pkg/adapter"
	api "github.com/team/agentlink/pkg/cli/net"
)

// Poller polls the inbox and injects messages into the agent via tmux.
type Poller struct {
	Session       string
	Server        string
	TeamID        string
	DeviceSession string

	// IdleDetector checks whether the agent pane is ready for input.
	IdleDetector adapter.IdleDetector

	// Polling interval. Default 5s.
	Interval time.Duration

	// Log output. Defaults to io.Discard.
	Stdout io.Writer

	// Cancellation. When nil, context.Background() is used.
	Ctx context.Context

	// Overridable for testing.
	capturePane func(session string) (string, error)
	sendKeys    func(session, text string) error
	httpDo      func(req *http.Request) (*http.Response, error)
}

func (p *Poller) Run() error {
	p.initDefaults()
	lastHeartbeat := time.Now()
	for {
		if err := p.ctx().Err(); err != nil {
			return err
		}

		// Heartbeat every ~12 iterations (60s with 5s interval)
		if time.Since(lastHeartbeat) > 60*time.Second {
			lastHeartbeat = time.Now()
			p.heartbeat()
		}

		msg, err := p.pullOne()
		if err != nil {
			fmt.Fprintf(p.Stdout, "poll error: %s\n", err)
			p.sleep(p.Interval)
			continue
		}

		if msg != nil {
			fmt.Fprintf(p.Stdout, "message from %s:%s\n", msg.FromDevice, msg.FromSession)
			if msg.Interrupt {
				fmt.Fprintf(p.Stdout, "interrupt: sending Ctrl+C\n")
				exec.Command("tmux", "send-keys", "-t", p.Session, "Escape").Run()
				time.Sleep(3 * time.Second)
			} else {
				p.waitForIdle()
			}

			pane, err := p.capturePane(p.Session)
			if err == nil && !p.IdleDetector.IsBusy(pane) && p.IdleDetector.IsPromptEmpty(pane) {
				injectContent := msg.Content
				if msg.Type == "msg" {
					prefix := fmt.Sprintf("[来自 %s:%s 的消息] ", msg.FromDevice, msg.FromSession)
					injectContent = prefix + msg.Content
				} else if msg.Type == "task" {
					injectContent = fmt.Sprintf(
						"[来自 %s:%s 的任务 %s]\n%s\n完成后请执行: agentlink task result %s completed \"<结果>\"\n如需挂起: agentlink task result %s suspended \"<原因>\"",
						msg.FromDevice, msg.FromSession, msg.TaskID,
						msg.Content,
						msg.TaskID, msg.TaskID,
					)
				}
				fmt.Fprintf(p.Stdout, "inject: %s\n", injectContent)
				if err := p.sendKeys(p.Session, injectContent); err != nil {
					fmt.Fprintf(p.Stdout, "send-keys error: %s\n", err)
				}
			}
		}

		pane, _ := p.capturePane(p.Session)
		if pane != "" && (strings.Contains(pane, "Do you trust") || strings.Contains(pane, "trust this folder")) {
			fmt.Fprintf(p.Stdout, "auto-accept trust prompt\n")
			p.sendKeys(p.Session, "1")
		}

		p.sleep(p.Interval)
	}
}

// teamURL builds a full team-scoped URL for the poller's active team.
func (p *Poller) teamURL(suffix string) string {
	return p.Server + "/api/teams/" + url.PathEscape(p.TeamID) + suffix
}

// authHeaders stamps a request with the Device credential and the poller's
// local session, so the server attributes presence/inbox to the right actor.
func (p *Poller) authHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Device "+p.DeviceSession)
	req.Header.Set("X-Agentlink-Session", p.Session)
}

func (p *Poller) heartbeat() {
	req, err := http.NewRequestWithContext(p.ctx(), "POST", p.teamURL("/agents/heartbeat"), nil)
	if err != nil {
		return
	}
	p.authHeaders(req)
	resp, err := p.httpDo(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

// -- internal --

type pollerInboxItem struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Content     string `json:"content"`
	FromDevice  string `json:"from_device"`
	FromSession string `json:"from_session"`
	TaskID      string `json:"task_id,omitempty"`
	Title       string `json:"title,omitempty"`
	Interrupt   bool   `json:"interrupt,omitempty"`
}

type pollerPullResponse struct {
	Items []pollerInboxItem `json:"items"`
}

func (p *Poller) pullOne() (*pollerInboxItem, error) {
	req, err := http.NewRequestWithContext(p.ctx(), "GET", p.teamURL("/inbox?limit=1"), nil)
	if err != nil {
		return nil, err
	}
	p.authHeaders(req)

	resp, err := p.httpDo(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			return nil, fmt.Errorf("server %d: %s", resp.StatusCode, e.Error)
		}
		return nil, fmt.Errorf("server %d", resp.StatusCode)
	}

	var pr pollerPullResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, err
	}
	if len(pr.Items) == 0 {
		return nil, nil
	}
	return &pr.Items[0], nil
}

func (p *Poller) waitForIdle() {
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		if p.ctx().Err() != nil {
			return
		}
		pane, err := p.capturePane(p.Session)
		if err != nil {
			continue
		}
		if !p.IdleDetector.IsBusy(pane) && p.IdleDetector.IsPromptEmpty(pane) {
			return
		}
		if strings.Contains(pane, "Do you trust") || strings.Contains(pane, "trust this folder") {
			fmt.Fprintf(p.Stdout, "auto-accept trust prompt\n")
			p.sendKeys(p.Session, "1")
		}
		select {
		case <-p.ctx().Done():
			return
		case <-time.After(time.Second):
		}
	}
	fmt.Fprintf(p.Stdout, "waitForIdle timeout after 5m\n")
}

func (p *Poller) sleep(d time.Duration) {
	select {
	case <-p.ctx().Done():
	case <-time.After(d):
	}
}

func (p *Poller) ctx() context.Context {
	if p.Ctx != nil {
		return p.Ctx
	}
	return context.Background()
}

func (p *Poller) initDefaults() {
	if p.Stdout == nil {
		p.Stdout = io.Discard
	}
	if p.Interval <= 0 {
		p.Interval = 5 * time.Second
	}
	if p.capturePane == nil {
		p.capturePane = tmuxCapturePane
	}
	if p.sendKeys == nil {
		p.sendKeys = tmuxSendKeys
	}
	if p.httpDo == nil {
		p.httpDo = http.DefaultClient.Do
	}
}

// -- tmux helpers (package-level for test override) --

var tmuxCapturePane = func(session string) (string, error) {
	var stdout bytes.Buffer
	cmd := exec.Command("tmux", "capture-pane", "-p", "-t", session)
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return stdout.String(), nil
}

var tmuxSendKeys = func(session, text string) error {
	cmd1 := exec.Command("tmux", "send-keys", "-l", "-t", session, text)
	if err := cmd1.Run(); err != nil {
		return err
	}
	time.Sleep(50 * time.Millisecond)
	cmd2 := exec.Command("tmux", "send-keys", "-t", session, "Enter")
	return cmd2.Run()
}

// -- entry point --

func RunPoll() error {
	cfg, creds, err := api.LoadAuth()
	if err != nil {
		return err
	}

	if !cfg.Poll.Enabled {
		fmt.Println("Auto-polling is disabled in config")
		return nil
	}

	if cfg.CurrentTeam == "" {
		return fmt.Errorf("no active team; run agentlink team use <team_id>")
	}

	session, err := api.FindCurrentSession()
	if err != nil {
		return err
	}

	interval := cfg.Poll.Interval
	if interval <= 0 {
		interval = api.DefaultPollInterval
	}

	p := &Poller{
		Session:       session,
		Server:        cfg.Server,
		TeamID:        cfg.CurrentTeam,
		DeviceSession: creds.DeviceSession,
		Interval:      time.Duration(interval) * time.Second,
		Stdout:        os.Stdout,
		IdleDetector:  adapter.NewDetector(cfg.Agent),
	}
	return p.Run()
}
