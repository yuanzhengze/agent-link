package api

import (
	"encoding/json"
	"fmt"
)

// RunWhoami prints the local account/device/session identity and the caller's
// received/sent tasks in the active team. There is no v2 `/whoami` endpoint;
// identity is local and tasks come from the team task list.
func RunWhoami() error {
	cfg, creds, err := LoadAuth()
	if err != nil {
		return err
	}

	session, err := FindCurrentSession()
	if err != nil {
		return err
	}

	fmt.Printf("You are %s@%s/%s\n", cfg.Username, cfg.Device, session)
	if cfg.CurrentTeam != "" {
		fmt.Printf("Team: %s\n", cfg.CurrentTeam)
	} else {
		fmt.Println("Team: (none selected — run agentlink team use <team_id>)")
	}

	if cfg.CurrentTeam != "" {
		path, err := TeamPath(cfg, "/tasks")
		if err == nil {
			if resp, err := APIDoWithSession(cfg, creds, session, "GET", path, nil); err == nil {
				defer resp.Body.Close()
				var result struct {
					Received []taskItem `json:"received"`
					Sent     []taskItem `json:"sent"`
				}
				json.NewDecoder(resp.Body).Decode(&result)
				printWhoamiTasks("Received tasks", result.Received)
				printWhoamiTasks("Sent tasks", result.Sent)
			}
		}
	}

	fmt.Println("\nTask vs Msg:")
	fmt.Println("  task — need a result back (completed/suspended)")
	fmt.Println("  msg  — fire-and-forget, no reply needed")
	fmt.Println()
	fmt.Println("Commands (for agent use):")
	fmt.Println("  agentlink task send <target> <id> \"<content>\"      — issue task (need result)")
	fmt.Println("  agentlink task result <id> completed \"<msg>\"        — report result (worker)")
	fmt.Println("  agentlink task status <id>                           — task detail")
	fmt.Println("  agentlink task list                                  — full task list")
	fmt.Println("  agentlink session add <name>                         — create new agent session")
	fmt.Println("  agentlink session remove <name>                      — remove agent session")
	fmt.Println("  agentlink list                                       — team devices")
	fmt.Println("  agentlink send [--interrupt] <target> \"<msg>\"       — send msg (no reply)")
	fmt.Println()
	fmt.Println("User-only (do NOT run): init, install, uninstall, restart, attach")
	return nil
}

func printWhoamiTasks(label string, items []taskItem) {
	if len(items) == 0 {
		return
	}
	fmt.Printf("\n%s:\n", label)
	for _, t := range items {
		extra := ""
		if t.CompletedAt != "" {
			extra += " (completed)"
		}
		if t.Result != "" {
			extra += " → " + t.Result
		}
		fmt.Printf("  %-12s  %-15s  %s%s\n", t.TaskID, t.Status, t.Content, extra)
	}
}
