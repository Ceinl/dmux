package project

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/Ceinl/dmux/internal/registry"
)

// Runner runs a single command on a host over SSH and returns its stdout. The
// concrete *remote.sshDialer satisfies it; the indexer depends only on this
// narrow surface so remote.Dialer stays unchanged (M8.2).
type Runner interface {
	Run(ctx context.Context, h registry.Host, cmd string) ([]byte, error)
}

// sshIndexer walks a host over SSH to enumerate projects on demand (M8.2).
type sshIndexer struct {
	runner Runner
}

// NewIndexer builds the on-demand project indexer.
func NewIndexer(runner Runner) *sshIndexer { return &sshIndexer{runner: runner} }

// Index lists projects under the host's HomeConfig.Root, bounded by ScanDepth
// (M8.3).
//
// Project rule (telescope-style, v1 interpretation): the result is
//   - every git repo root found under Root within ScanDepth, PLUS
//   - immediate subdirectories of Root that are NOT inside any of those repos
//     (the "directory itself if not in a repo" case).
//
// The walk is two `find` invocations whose output is combined in Go; this keeps
// each remote command simple and predictable on large trees.
func (ix *sshIndexer) Index(ctx context.Context, h registry.Host) ([]Project, error) {
	root := strings.TrimRight(h.Root, "/")
	if root == "" {
		return nil, fmt.Errorf("project: host %s has no sethome root", h.ID)
	}
	depth := h.ScanDepth
	if depth < 1 {
		depth = 1
	}

	// 1) git repo roots: find .git dirs, prune so we don't descend into them.
	gitCmd := fmt.Sprintf(
		"find %s -maxdepth %d -type d -name .git -prune 2>/dev/null",
		shellQuote(root), depth+1,
	)
	gitOut, err := ix.runner.Run(ctx, h, gitCmd)
	if err != nil {
		return nil, fmt.Errorf("project: index git roots on %s: %w", h.ID, err)
	}
	repoRoots := map[string]struct{}{}
	for _, line := range nonEmptyLines(gitOut) {
		repoRoots[path.Dir(line)] = struct{}{}
	}

	// 2) immediate (depth-1) subdirectories of Root.
	dirCmd := fmt.Sprintf(
		"find %s -mindepth 1 -maxdepth 1 -type d -not -name '.*' 2>/dev/null",
		shellQuote(root),
	)
	dirOut, err := ix.runner.Run(ctx, h, dirCmd)
	if err != nil {
		return nil, fmt.Errorf("project: index dirs on %s: %w", h.ID, err)
	}

	// Combine: all repo roots, plus depth-1 dirs not inside any repo root.
	seen := map[string]struct{}{}
	var roots []string
	add := func(p string) {
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		roots = append(roots, p)
	}
	for r := range repoRoots {
		add(r)
	}
	for _, d := range nonEmptyLines(dirOut) {
		if insideAnyRepo(d, repoRoots) {
			continue
		}
		add(d)
	}

	sort.Strings(roots)
	out := make([]Project, 0, len(roots))
	for _, r := range roots {
		out = append(out, Project{HostID: h.ID, Root: r, Name: path.Base(r)})
	}
	return out, nil
}

// insideAnyRepo reports whether dir is one of, or nested under, a repo root.
func insideAnyRepo(dir string, repos map[string]struct{}) bool {
	for r := range repos {
		if dir == r || strings.HasPrefix(dir, r+"/") {
			return true
		}
	}
	return false
}

func nonEmptyLines(b []byte) []string {
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// shellQuote single-quotes s for safe inclusion in a remote shell command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Compile-time check that sshIndexer satisfies Indexer.
var _ Indexer = (*sshIndexer)(nil)
