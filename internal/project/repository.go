package project

import "strings"

// RepositoryRoot returns the canonical repository root for dir, or "" when dir
// is not inside a git repository. Linked worktrees resolve to their shared
// primary checkout root, mirroring DetectProjectFull's Path.
//
// Read-only by contract: unlike DetectProjectFull it never creates or mutates
// the repository identity binding. It exists for read-only consumers — such as
// the store's cloud inbox routing derivation — that need the repository root
// without claiming a project identity for the repository.
func RepositoryRoot(dir string) string {
	if strings.TrimSpace(dir) == "" {
		// An empty directory would make git resolve against the process working
		// directory, which is never the caller's session directory.
		return ""
	}
	return detectGitRootDir(dir)
}
