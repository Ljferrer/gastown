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
