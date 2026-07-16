package api

import (
	"fmt"
	"io/fs"
	"os/exec"
	"path/filepath"
	"strings"
)

// seedIndexHTML is the initial content of a newly created project's work tree.
const seedIndexHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>New Project</title>
</head>
<body>
  <h1>New Project</h1>
</body>
</html>
`

type SnapshotFileEntry struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// gitCommit stages all changes in dir and commits them under the repo-local
// cowork identity, returning the resulting HEAD sha.
func (s *Server) gitCommit(dir, msg string) (sha string, err error) {
	if out, err := exec.Command("git", "-C", dir, "add", "-A").CombinedOutput(); err != nil {
		return "", fmt.Errorf("git add: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("git", "-C", dir, "commit", "-m", msg).CombinedOutput(); err != nil {
		return "", fmt.Errorf("git commit: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// listProjectFiles walks a project's work tree and returns the forward-slash
// relative path of every regular file, skipping the .git directory entirely.
func listProjectFiles(dir string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			// Non-regular entries (symlinks, sockets, devices, etc.) are
			// intentionally skipped: following a symlink here could walk
			// or read outside the project's work tree.
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return paths, nil
}
