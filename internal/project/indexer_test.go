package project

import (
	"context"
	"strings"
	"testing"

	"dmux/internal/registry"
)

// fakeRunner returns canned output per command substring.
type fakeRunner struct {
	gitOut string
	dirOut string
	err    error
}

func (f *fakeRunner) Run(_ context.Context, _ registry.Host, cmd string) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	if strings.Contains(cmd, "-name .git") {
		return []byte(f.gitOut), nil
	}
	return []byte(f.dirOut), nil
}

func testHost() registry.Host {
	return registry.Host{
		ID:         "h1",
		HomeConfig: registry.HomeConfig{Root: "/home/dima/code", ScanDepth: 2},
	}
}

func TestKeyOf(t *testing.T) {
	p := Project{HostID: "h1", Root: "/x"}
	if KeyOf(p) != (Key{HostID: "h1", Root: "/x"}) {
		t.Errorf("KeyOf = %+v", KeyOf(p))
	}
}

func TestIndexGitRootsAndPlainDirs(t *testing.T) {
	r := &fakeRunner{
		gitOut: "/home/dima/code/app/.git\n/home/dima/code/lib/.git\n",
		// app & lib are repos; notes is a plain dir; app is also listed at depth-1.
		dirOut: "/home/dima/code/app\n/home/dima/code/notes\n",
	}
	ix := NewIndexer(r)
	got, err := ix.Index(context.Background(), testHost())
	if err != nil {
		t.Fatalf("Index: %v", err)
	}

	byRoot := map[string]Project{}
	for _, p := range got {
		byRoot[p.Root] = p
	}
	// Repo roots present.
	for _, want := range []string{"/home/dima/code/app", "/home/dima/code/lib"} {
		if _, ok := byRoot[want]; !ok {
			t.Errorf("missing repo root %s", want)
		}
	}
	// Plain dir present.
	if _, ok := byRoot["/home/dima/code/notes"]; !ok {
		t.Error("missing plain dir /home/dima/code/notes")
	}
	// app must appear once (deduped between repo + depth-1 listing).
	count := 0
	for _, p := range got {
		if p.Root == "/home/dima/code/app" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("app appears %d times, want 1 (dedupe)", count)
	}
	// Name = basename.
	if byRoot["/home/dima/code/app"].Name != "app" {
		t.Errorf("Name = %q, want app", byRoot["/home/dima/code/app"].Name)
	}
	// HostID propagated.
	if byRoot["/home/dima/code/app"].HostID != "h1" {
		t.Errorf("HostID = %q, want h1", byRoot["/home/dima/code/app"].HostID)
	}
}

func TestIndexExcludesDirsInsideRepos(t *testing.T) {
	r := &fakeRunner{
		gitOut: "/home/dima/code/app/.git\n",
		// "app/sub" is inside the repo and must be excluded; only top-level here.
		dirOut: "/home/dima/code/app\n",
	}
	ix := NewIndexer(r)
	got, _ := ix.Index(context.Background(), testHost())
	if len(got) != 1 || got[0].Root != "/home/dima/code/app" {
		t.Fatalf("got %+v, want only the app repo root", got)
	}
}

func TestIndexNoRootErrors(t *testing.T) {
	ix := NewIndexer(&fakeRunner{})
	h := registry.Host{ID: "h"}
	if _, err := ix.Index(context.Background(), h); err == nil {
		t.Error("expected error when host has no sethome root")
	}
}

func TestInsideAnyRepo(t *testing.T) {
	repos := map[string]struct{}{"/a/b": {}}
	if !insideAnyRepo("/a/b", repos) {
		t.Error("repo root itself should count as inside")
	}
	if !insideAnyRepo("/a/b/c", repos) {
		t.Error("nested dir should count as inside")
	}
	if insideAnyRepo("/a/bc", repos) {
		t.Error("sibling prefix should NOT count as inside")
	}
}
