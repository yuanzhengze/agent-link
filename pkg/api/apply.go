package api

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
)

// projectLock returns the mutex serializing writes+commits for a project,
// creating it on first use. The key is opaque: v2 uses teamProjectMutexKey.
func (s *Server) projectLock(id string) *sync.Mutex {
	mu, _ := s.projMu.LoadOrStore(id, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// safeApplyPath validates that path is a safe, relative path inside
// projectDir (no traversal, not absolute, not inside .git), and returns the
// resolved on-disk path to write to.
func safeApplyPath(projectDir, path string) (string, error) {
	if path == "" {
		return "", errors.New("missing field: path")
	}
	if filepath.IsAbs(path) {
		return "", errors.New("path must not be absolute")
	}
	clean := filepath.Clean(path)
	if clean == "." {
		return "", errors.New("path must not be empty")
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean) {
		return "", errors.New("path escapes project directory")
	}

	full := filepath.Join(projectDir, clean)
	rel, err := filepath.Rel(projectDir, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", errors.New("path escapes project directory")
	}
	if rel == "." {
		return "", errors.New("path must not be empty")
	}
	// Reject any path segment that is ".git" case-insensitively: on
	// case-insensitive filesystems (macOS APFS, Windows NTFS) a segment
	// like ".GIT" or ".Git" resolves to the same on-disk directory as
	// ".git", which would otherwise let a client overwrite files such as
	// .git/hooks/pre-commit and gain code execution on the next commit.
	for _, seg := range strings.Split(rel, string(filepath.Separator)) {
		if strings.EqualFold(seg, ".git") {
			return "", errors.New("path must not target the .git directory")
		}
	}

	return full, nil
}
