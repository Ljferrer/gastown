package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitMust runs a git command in dir and fails the test on error.
func gitMust(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// setupSharedRepoWithWorktrees builds a bare "origin", a primary clone with an
// initial commit pushed to main, and returns the clone and origin paths.
func setupSharedRepoWithWorktrees(t *testing.T) (clone, origin string) {
	t.Helper()
	root := t.TempDir()
	origin = filepath.Join(root, "origin.git")
	gitMust(t, root, "init", "--bare", origin)

	clone = filepath.Join(root, "clone")
	gitMust(t, root, "clone", origin, clone)
	gitMust(t, clone, "config", "user.email", "test@test.com")
	gitMust(t, clone, "config", "user.name", "Test User")

	if err := os.WriteFile(filepath.Join(clone, "README.md"), []byte("# seed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitMust(t, clone, "add", ".")
	gitMust(t, clone, "commit", "-m", "seed")
	gitMust(t, clone, "push", "origin", "HEAD:refs/heads/main")
	return clone, origin
}

func TestDisablePush_BlocksPushInSeatOnly(t *testing.T) {
	clone, _ := setupSharedRepoWithWorktrees(t)
	headSHA := gitMust(t, clone, "rev-parse", "HEAD")

	// A normal worktree (stand-in for a worker polecat) must keep the ability to
	// push; the seat worktree must not.
	worker := filepath.Join(filepath.Dir(clone), "worker")
	seat := filepath.Join(filepath.Dir(clone), "seat")

	cloneGit := NewGit(clone)
	if err := cloneGit.WorktreeAddFromRef(worker, "polecat/worker", "origin/main"); err != nil {
		t.Fatalf("add worker worktree: %v", err)
	}
	if err := cloneGit.WorktreeAddDetached(seat, headSHA); err != nil {
		t.Fatalf("add seat worktree: %v", err)
	}

	seatGit := NewGit(seat)
	if err := seatGit.DisablePush("origin"); err != nil {
		t.Fatalf("DisablePush: %v", err)
	}

	// Seat: an explicit ref-advancing push must fail (detached HEAD alone would
	// not stop `git push origin <sha>:<ref>`).
	cmd := exec.Command("git", "push", "origin", headSHA+":refs/heads/seat-attack")
	cmd.Dir = seat
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("seat push unexpectedly succeeded; output:\n%s", out)
	}

	// Worker: a normal push must still succeed — the seat's change must not have
	// leaked into the shared config.
	if err := os.WriteFile(filepath.Join(worker, "worker.txt"), []byte("work\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitMust(t, worker, "add", ".")
	gitMust(t, worker, "commit", "-m", "worker change")
	cmd = exec.Command("git", "push", "origin", "polecat/worker")
	cmd.Dir = worker
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("worker push should still succeed, got error: %v\n%s", err, out)
	}
}

func TestDisablePush_Idempotent(t *testing.T) {
	clone, _ := setupSharedRepoWithWorktrees(t)
	headSHA := gitMust(t, clone, "rev-parse", "HEAD")
	seat := filepath.Join(filepath.Dir(clone), "seat")
	if err := NewGit(clone).WorktreeAddDetached(seat, headSHA); err != nil {
		t.Fatalf("add seat worktree: %v", err)
	}
	seatGit := NewGit(seat)
	if err := seatGit.DisablePush("origin"); err != nil {
		t.Fatalf("DisablePush first call: %v", err)
	}
	if err := seatGit.DisablePush("origin"); err != nil {
		t.Fatalf("DisablePush second call should be idempotent: %v", err)
	}
}

// setupBareRepoWithWorktrees builds a production-style bare repo (a
// `git clone --bare` of a seeded origin, so its COMMON config carries
// core.bare=true at repositoryformatversion=0) and adds linked worktrees
// directly off it. This is the layout DisablePush corrupted: enabling
// extensions.worktreeConfig without migrating leaks core.bare onto every
// worktree. The existing setupSharedRepoWithWorktrees uses a NORMAL clone
// (core.bare=false), where the bug is invisible.
func setupBareRepoWithWorktrees(t *testing.T) (bare, head string) {
	t.Helper()
	root := t.TempDir()

	origin := filepath.Join(root, "origin.git")
	gitMust(t, root, "init", "--bare", origin)

	seed := filepath.Join(root, "seed")
	gitMust(t, root, "clone", origin, seed)
	gitMust(t, seed, "config", "user.email", "test@test.com")
	gitMust(t, seed, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("# seed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitMust(t, seed, "add", ".")
	gitMust(t, seed, "commit", "-m", "seed")
	gitMust(t, seed, "push", "origin", "HEAD:refs/heads/main")

	// The production .repo.git: a bare clone whose common config has
	// core.bare=true and repositoryformatversion=0.
	bare = filepath.Join(root, "repo.git")
	gitMust(t, root, "clone", "--bare", origin, bare)
	if v := gitMust(t, bare, "config", "core.bare"); v != "true" {
		t.Fatalf("precondition: bare repo core.bare = %q, want true", v)
	}
	if v := gitMust(t, bare, "config", "core.repositoryformatversion"); v != "0" {
		t.Fatalf("precondition: bare repo repositoryformatversion = %q, want 0", v)
	}
	head = gitMust(t, bare, "rev-parse", "refs/heads/main")
	return bare, head
}

// TestDisablePush_BareRepoDoesNotCorruptSiblingWorktrees is the regression for
// lgt-44m: on a bare repo, DisablePush on a seat worktree must not make
// core.bare leak onto sibling worktrees. Fails on the un-migrated code (worker
// worktree reports it is not inside a work tree); passes after the migration.
func TestDisablePush_BareRepoDoesNotCorruptSiblingWorktrees(t *testing.T) {
	bare, head := setupBareRepoWithWorktrees(t)

	worker := filepath.Join(filepath.Dir(bare), "worker")
	seat := filepath.Join(filepath.Dir(bare), "seat")

	bareGit := NewGit(bare)
	if err := bareGit.WorktreeAddDetached(worker, head); err != nil {
		t.Fatalf("add worker worktree: %v", err)
	}
	if err := bareGit.WorktreeAddDetached(seat, head); err != nil {
		t.Fatalf("add seat worktree: %v", err)
	}

	// Sanity: both worktrees are valid work trees before the seat is neutralised.
	if got := gitMust(t, worker, "rev-parse", "--is-inside-work-tree"); got != "true" {
		t.Fatalf("precondition: worker is-inside-work-tree = %q, want true", got)
	}

	if err := NewGit(seat).DisablePush("origin"); err != nil {
		t.Fatalf("DisablePush: %v", err)
	}

	// (i) Every worktree must still be a work tree — the corruption made these
	//     fail with "this operation must be run in a work tree".
	for _, wt := range []string{worker, seat} {
		if got := gitMust(t, wt, "rev-parse", "--is-inside-work-tree"); got != "true" {
			t.Fatalf("worktree %s is-inside-work-tree = %q, want true (core.bare leaked)", wt, got)
		}
	}

	// (ii) The common repo must have been migrated to version >= 1.
	if v := gitMust(t, bare, "config", "core.repositoryformatversion"); v != "1" {
		t.Fatalf("repositoryformatversion = %q, want 1 after migration", v)
	}

	// (iii) core.bare must no longer live in the COMMON config (read the file
	//       directly, bypassing the config.worktree merge).
	commonConfig := filepath.Join(bare, "config")
	cmd := exec.Command("git", "config", "--file", commonConfig, "--get", "core.bare")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("core.bare still present in common config (=%q); it must move to config.worktree", strings.TrimSpace(string(out)))
	}

	// The seat must still be unable to advance refs.
	cmd = exec.Command("git", "push", "origin", head+":refs/heads/seat-attack")
	cmd.Dir = seat
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("seat push unexpectedly succeeded; output:\n%s", out)
	}
}

func TestDiffThreeDot_ReadsFromSharedStore(t *testing.T) {
	clone, _ := setupSharedRepoWithWorktrees(t)
	target := gitMust(t, clone, "rev-parse", "HEAD")

	// Create a branch with a change; capture its SHA. We never check it out in
	// the seat — the seat reads the diff straight from the shared object store.
	gitMust(t, clone, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(clone, "feature.txt"), []byte("hello\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitMust(t, clone, "add", ".")
	gitMust(t, clone, "commit", "-m", "feature change")
	auditSHA := gitMust(t, clone, "rev-parse", "HEAD")
	gitMust(t, clone, "checkout", "main")

	// Seat is detached at target (the pre-merge base), never the feature ref.
	seat := filepath.Join(filepath.Dir(clone), "seat")
	if err := NewGit(clone).WorktreeAddDetached(seat, target); err != nil {
		t.Fatalf("add seat worktree: %v", err)
	}
	seatGit := NewGit(seat)

	patch, err := seatGit.DiffThreeDot(target, auditSHA)
	if err != nil {
		t.Fatalf("DiffThreeDot: %v", err)
	}
	if !strings.Contains(patch, "feature.txt") || !strings.Contains(patch, "+hello") {
		t.Fatalf("diff missing expected change:\n%s", patch)
	}

	// git show <sha>:<path> reads file content at the audited SHA without checkout.
	content, err := seatGit.ShowFile(auditSHA, "feature.txt")
	if err != nil {
		t.Fatalf("ShowFile: %v", err)
	}
	if strings.TrimSpace(content) != "hello" {
		t.Fatalf("ShowFile content = %q, want %q", content, "hello")
	}
}
