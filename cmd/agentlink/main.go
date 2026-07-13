package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	api "github.com/team/agentlink/pkg/cli/net"
	rt "github.com/team/agentlink/pkg/cli/runtime"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "init":
		cmdInit(os.Args[2:])
	case "send":
		cmdSend(os.Args[2:])
	case "pull":
		cmdPull(os.Args[2:])
	case "task":
		cmdTask(os.Args[2:])
	case "poll":
		cmdPoll(os.Args[2:])
	case "whoami":
		cmdWhoami()
	case "ping":
		cmdPing()
	case "list":
		cmdList(os.Args[2:])
	case "session":
		cmdSession(os.Args[2:])
	case "project":
		cmdProject(os.Args[2:])
	case "lock":
		cmdLock(os.Args[2:])
	case "attach":
		cmdAttach(os.Args[2:])
	case "restart":
		cmdResume(os.Args[2:])
	case "uninstall":
		cmdUninstall()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Print(`agentlink - Agent communication tool

Usage:
  agentlink init --server <url> --password <pw> [--device <name>] [./path]
  agentlink send [--interrupt] [--title <title>] <target> <content>
  agentlink pull [--all]
  agentlink whoami
  agentlink ping
  agentlink list [--all]
  agentlink poll
  agentlink task send [--interrupt] [--title <title>] <target> [<task_id>] <content>
  agentlink task result <task_id> <status> <result>
  agentlink task resume <task_id> <guidance>
  agentlink task cancel <task_id>
  agentlink task reopen <task_id> <reason>
  agentlink task status <task_id>
  agentlink task list
  agentlink session add|remove <name>
  agentlink project create <name>
  agentlink project list
  agentlink lock acquire <project> <path>
  agentlink lock release <project> <path>
  agentlink lock list <project>
  agentlink attach <session>
  agentlink restart
  agentlink uninstall
`)
}

func cmdInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	server := fs.String("server", "", "API server URL")
	password := fs.String("password", "", "Registration password")
	device := fs.String("device", "", "Device name (default: hostname)")
	agent := fs.String("agent", "claude", "Agent type (default: claude)")
	noPoll := fs.Bool("no-poll", false, "Disable auto-polling (default: false)")
	force := fs.Bool("force", false, "Force re-register if device exists (default: false)")
	fs.Parse(args)

	path := fs.Arg(0)
	if path == "" {
		path = "./agent_team"
	}

	opts := &rt.InitOptions{
		Server:   *server,
		Password: *password,
		Device:   *device,
		Path:     path,
		Agent:    *agent,
		NoPoll:   *noPoll,
		Force:    *force,
	}

	// When a required field is missing, run the interactive wizard on a
	// terminal; otherwise keep the original hard error (so piped stdin in
	// CI/scripts fails fast instead of hanging).
	if opts.Server == "" || opts.Password == "" {
		if rt.IsInteractive() {
			opts.Interactive = true
			if err := rt.PromptInitOptions(opts); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %s\n", err)
				os.Exit(1)
			}
		} else {
			fmt.Fprintln(os.Stderr, "init: --server and --password are required")
			os.Exit(1)
		}
	}

	// In interactive mode, show a summary and let the user back out before
	// anything is created or registered.
	if opts.Interactive && !rt.ConfirmInitSummary(opts) {
		fmt.Fprintln(os.Stderr, "已取消")
		os.Exit(1)
	}

	if err := rt.RunInit(opts); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}

	fmt.Println()
	fmt.Println("Attaching to main session...")

	attach := exec.Command("tmux", "attach", "-t", "main")
	attach.Stdin = os.Stdin
	attach.Stdout = os.Stdout
	attach.Stderr = os.Stderr
	if err := attach.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}

func cmdSend(args []string) {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	interrupt := fs.Bool("interrupt", false, "Interrupt the target if busy")
	title := fs.String("title", "", "Short title shown in recipient status (default: first 40 chars of content)")
	fs.Parse(args)

	rest := fs.Args()
	if len(rest) < 2 {
		fmt.Fprintln(os.Stderr, "usage: agentlink send [--interrupt] [--title <title>] <target> <content>")
		os.Exit(1)
	}
	target := rest[0]
	content := strings.Join(rest[1:], " ")

	if err := api.RunSend(target, content, *interrupt, *title); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}

func cmdPull(args []string) {
	fs := flag.NewFlagSet("pull", flag.ExitOnError)
	all := fs.Bool("all", false, "Pull all available messages (max 10)")
	fs.Parse(args)

	if err := api.RunPull(*all); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}

func cmdPoll(args []string) {
	if err := rt.RunPoll(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}

func cmdPing() {
	if err := api.RunPing(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}

func cmdList(args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	all := fs.Bool("all", false, "Show all devices")
	fs.Parse(args)

	if err := api.RunList(*all); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}

func cmdTask(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: agentlink task <subcommand> [args]")
		os.Exit(1)
	}
	switch args[0] {
	case "send":
		cmdTaskSend(args[1:])
	case "result":
		cmdTaskResult(args[1:])
	case "resume":
		cmdTaskResume(args[1:])
	case "cancel":
		cmdTaskCancel(args[1:])
	case "reopen":
		cmdTaskReopen(args[1:])
	case "status":
		cmdTaskStatus(args[1:])
	case "list":
		cmdTaskList()
	default:
		fmt.Fprintf(os.Stderr, "unknown task subcommand: %s\n", args[0])
		os.Exit(1)
	}
}

func cmdTaskSend(args []string) {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	interrupt := fs.Bool("interrupt", false, "Interrupt the target if busy")
	title := fs.String("title", "", "Short title shown in recipient status (default: task_id)")
	fs.Parse(args)

	rest := fs.Args()
	if len(rest) < 2 {
		fmt.Fprintln(os.Stderr, "usage: agentlink task send [--interrupt] [--title <title>] <target> [<task_id>] <content>")
		fmt.Fprintln(os.Stderr, "  with task_id: agentlink task send --interrupt worker my-id \"do something\"")
		fmt.Fprintln(os.Stderr, "  without:      agentlink task send --interrupt worker \"do something\"")
		os.Exit(1)
	}
	target := rest[0]
	var taskID string
	var content string
	if len(rest) == 2 {
		content = rest[1]
	} else {
		taskID = rest[1]
		content = strings.Join(rest[2:], " ")
	}

	if err := api.RunTaskSend(target, taskID, content, *interrupt, *title); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}

func cmdTaskResult(args []string) {
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: agentlink task result <task_id> <status> <result>")
		os.Exit(1)
	}
	taskID := args[0]
	status := args[1]
	result := strings.Join(args[2:], " ")

	if err := api.RunTaskResult(taskID, status, result); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}

func cmdTaskResume(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: agentlink task resume <task_id> <guidance>")
		os.Exit(1)
	}
	taskID := args[0]
	guidance := strings.Join(args[1:], " ")

	if err := api.RunTaskResume(taskID, guidance); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}

func cmdTaskCancel(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: agentlink task cancel <task_id>")
		os.Exit(1)
	}
	taskID := args[0]

	if err := api.RunTaskCancel(taskID); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}

func cmdTaskReopen(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: agentlink task reopen <task_id> <reason>")
		os.Exit(1)
	}
	taskID := args[0]
	reason := strings.Join(args[1:], " ")

	if err := api.RunTaskReopen(taskID, reason); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}

func cmdSession(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: agentlink session add|remove <name>")
		os.Exit(1)
	}
	switch args[0] {
	case "add":
		cmdSessionAdd(args[1:])
	case "remove":
		cmdSessionRemove(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown session subcommand: %s\n", args[0])
		os.Exit(1)
	}
}

func cmdSessionAdd(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: agentlink session add <name>")
		os.Exit(1)
	}
	name := args[0]
	if err := rt.RunSessionAdd(name); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}

func cmdSessionRemove(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: agentlink session remove <name>")
		os.Exit(1)
	}
	name := args[0]
	if err := rt.RunSessionRemove(name); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}

func cmdProject(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: agentlink project create|list [args]")
		os.Exit(1)
	}
	switch args[0] {
	case "create":
		cmdProjectCreate(args[1:])
	case "list":
		cmdProjectList()
	default:
		fmt.Fprintf(os.Stderr, "unknown project subcommand: %s\n", args[0])
		os.Exit(1)
	}
}

func cmdProjectCreate(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: agentlink project create <name>")
		os.Exit(1)
	}
	name := args[0]
	if err := api.RunProjectCreate(name); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}

func cmdProjectList() {
	if err := api.RunProjectList(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}

func cmdLock(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: agentlink lock acquire|release|list [args]")
		os.Exit(1)
	}
	switch args[0] {
	case "acquire":
		cmdLockAcquire(args[1:])
	case "release":
		cmdLockRelease(args[1:])
	case "list":
		cmdLockList(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown lock subcommand: %s\n", args[0])
		os.Exit(1)
	}
}

func cmdLockAcquire(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: agentlink lock acquire <project> <path>")
		os.Exit(1)
	}
	if err := api.RunLockAcquire(args[0], args[1]); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}

func cmdLockRelease(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: agentlink lock release <project> <path>")
		os.Exit(1)
	}
	if err := api.RunLockRelease(args[0], args[1]); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}

func cmdLockList(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: agentlink lock list <project>")
		os.Exit(1)
	}
	if err := api.RunLockList(args[0]); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}

func cmdWhoami() {
	if err := api.RunWhoami(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}

func cmdAttach(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: agentlink attach <session>")
		os.Exit(1)
	}
	session := args[0]
	if err := rt.RunAttach(session); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}

func cmdResume(args []string) {
	if err := rt.RunResume(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}

func cmdUninstall() {
	if err := rt.RunUninstall(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}

func cmdTaskList() {
	if err := api.RunTaskList(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}

func cmdTaskStatus(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: agentlink task status <task_id>")
		os.Exit(1)
	}
	taskID := args[0]

	if err := api.RunTaskStatus(taskID); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}
