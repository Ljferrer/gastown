package seat

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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

// setupRepo builds a bare origin + clone with main, plus a feature commit whose
// SHA is returned as auditSHA. target is the main SHA the merge would land on.
func setupRepo(t *testing.T) (clone, target, auditSHA string) {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
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
	target = gitMust(t, clone, "rev-parse", "HEAD")

	gitMust(t, clone, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(clone, "feature.txt"), []byte("hello\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitMust(t, clone, "add", ".")
	gitMust(t, clone, "commit", "-m", "feature change")
	auditSHA = gitMust(t, clone, "rev-parse", "HEAD")
	gitMust(t, clone, "checkout", "main")
	return clone, target, auditSHA
}

func TestCreate_DetachedAtSHA_NoLiveBranch(t *testing.T) {
	clone, target, auditSHA := setupRepo(t)
	seatPath := filepath.Join(filepath.Dir(clone), "seat")

	s, err := Create(Options{RepoPath: clone, SeatPath: seatPath, AuditSHA: auditSHA, Target: target})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Criterion: checked out detached at the given SHA.
	if head := gitMust(t, seatPath, "rev-parse", "HEAD"); head != auditSHA {
		t.Fatalf("seat HEAD = %s, want auditSHA %s", head, auditSHA)
	}
	// Detached => symbolic-ref HEAD fails (no branch attached).
	cmd := exec.Command("git", "symbolic-ref", "-q", "HEAD")
	cmd.Dir = seatPath
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("seat HEAD is attached to a branch %q, want detached", strings.TrimSpace(string(out)))
	}
	// Criterion: the live branch ref is never the seat's checked-out ref —
	// abbrev-ref of a detached HEAD is the literal "HEAD", not a branch name.
	if ref := gitMust(t, seatPath, "rev-parse", "--abbrev-ref", "HEAD"); ref != "HEAD" {
		t.Fatalf("seat checked-out ref = %q, want detached HEAD", ref)
	}
	if s.AuditSHA != auditSHA {
		t.Fatalf("Seat.AuditSHA = %s, want %s", s.AuditSHA, auditSHA)
	}
}

func TestCreate_PushPhysicallyFails(t *testing.T) {
	clone, target, auditSHA := setupRepo(t)
	seatPath := filepath.Join(filepath.Dir(clone), "seat")
	if _, err := Create(Options{RepoPath: clone, SeatPath: seatPath, AuditSHA: auditSHA, Target: target}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// An explicit ref-advancing push must fail.
	cmd := exec.Command("git", "push", "origin", auditSHA+":refs/heads/seat-attack")
	cmd.Dir = seatPath
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("seat push unexpectedly succeeded:\n%s", out)
	}
}

func TestSeat_DiffAndShow(t *testing.T) {
	clone, target, auditSHA := setupRepo(t)
	seatPath := filepath.Join(filepath.Dir(clone), "seat")
	s, err := Create(Options{RepoPath: clone, SeatPath: seatPath, AuditSHA: auditSHA, Target: target})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	patch, err := s.Diff()
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !strings.Contains(patch, "feature.txt") || !strings.Contains(patch, "+hello") {
		t.Fatalf("diff missing expected change:\n%s", patch)
	}

	content, err := s.ShowFile("feature.txt")
	if err != nil {
		t.Fatalf("ShowFile: %v", err)
	}
	if strings.TrimSpace(content) != "hello" {
		t.Fatalf("ShowFile = %q, want %q", content, "hello")
	}
}

func TestBuildSettings_AllowEnablesReadDiffVerdict(t *testing.T) {
	b, err := BuildSettings(SettingsOptions{})
	if err != nil {
		t.Fatalf("BuildSettings: %v", err)
	}
	var parsed claudeSettings
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("settings is not valid JSON: %v\n%s", err, b)
	}

	// NOT bypassed: the permission system must be active.
	if parsed.Permissions.DefaultMode == "bypassPermissions" {
		t.Fatal("seat settings must not use bypassPermissions")
	}
	if parsed.Permissions.DefaultMode != "default" {
		t.Fatalf("defaultMode = %q, want default", parsed.Permissions.DefaultMode)
	}

	allow := strings.Join(parsed.Permissions.Allow, "\n")
	// Criterion: headless seat never hangs while doing read+diff+write-verdict —
	// every step of that workflow must be pre-allowed.
	for _, need := range []string{"Read", "Grep", "Glob", "Bash(git diff:*)", "Bash(git show:*)", "Bash(git log:*)"} {
		if !strings.Contains(allow, need) {
			t.Errorf("allow-list missing read/diff rule %q", need)
		}
	}
	// At least one verdict-write path must be allowed.
	if !strings.Contains(allow, "bd create") && !strings.Contains(allow, "bd update") && !strings.Contains(allow, "gt mail send") {
		t.Errorf("allow-list has no verdict-write rule:\n%s", allow)
	}
}

func TestBuildSettings_DenyBlocksMutation(t *testing.T) {
	b, err := BuildSettings(SettingsOptions{})
	if err != nil {
		t.Fatalf("BuildSettings: %v", err)
	}
	var parsed claudeSettings
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatal(err)
	}
	deny := strings.Join(parsed.Permissions.Deny, "\n")
	// Criterion: Write/Edit/push/commit/merge denied and verified blocked.
	for _, need := range []string{"Write", "Edit", "NotebookEdit", "Bash(git push:*)", "Bash(git commit:*)", "Bash(git merge:*)", "Bash(gt done:*)"} {
		if !strings.Contains(deny, need) {
			t.Errorf("deny-list missing %q", need)
		}
	}
}

func TestBuildSettings_CustomVerdictRules(t *testing.T) {
	b, err := BuildSettings(SettingsOptions{Verdict: []string{"Bash(gt mail send:*)"}})
	if err != nil {
		t.Fatal(err)
	}
	var parsed claudeSettings
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatal(err)
	}
	allow := strings.Join(parsed.Permissions.Allow, "\n")
	if !strings.Contains(allow, "gt mail send") {
		t.Errorf("custom verdict rule not present: %s", allow)
	}
	// Defaults must be replaced, not appended.
	if strings.Contains(allow, "bd create") {
		t.Errorf("default verdict rules should be overridden, got: %s", allow)
	}
}

func TestCreate_WritesSettingsFile(t *testing.T) {
	clone, target, auditSHA := setupRepo(t)
	seatPath := filepath.Join(filepath.Dir(clone), "seat")
	s, err := Create(Options{RepoPath: clone, SeatPath: seatPath, AuditSHA: auditSHA, Target: target})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	data, err := os.ReadFile(s.SettingsPath)
	if err != nil {
		t.Fatalf("reading settings: %v", err)
	}
	if !json.Valid(data) {
		t.Fatalf("settings.json invalid:\n%s", data)
	}
}

func TestRestrictClaudeCommand(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		settings string
		wantNot  string
		wantHas  []string
	}{
		{
			name:     "strips skip flag and adds settings",
			in:       "claude --dangerously-skip-permissions --model opus",
			settings: "/tmp/seat/settings.json",
			wantNot:  "--dangerously-skip-permissions",
			wantHas:  []string{"--model opus", "--settings /tmp/seat/settings.json"},
		},
		{
			name:     "no skip flag present is a no-op on strip",
			in:       "claude --model opus",
			settings: "/tmp/s.json",
			wantNot:  "--dangerously-skip-permissions",
			wantHas:  []string{"--settings /tmp/s.json"},
		},
		{
			name:     "empty settings path omits --settings",
			in:       "claude --dangerously-skip-permissions",
			settings: "",
			wantNot:  "--settings",
			wantHas:  []string{"claude"},
		},
		{
			name:     "quotes settings path with spaces",
			in:       "claude --dangerously-skip-permissions",
			settings: "/tmp/a b/settings.json",
			wantNot:  "--dangerously-skip-permissions",
			wantHas:  []string{"--settings '/tmp/a b/settings.json'"},
		},
		{
			// Critical: the stock polecat command already carries a permissive
			// --settings (shared autonomous profile). It MUST be replaced, not
			// kept, or the seat would run unrestricted.
			name:     "replaces existing permissive --settings",
			in:       "claude --dangerously-skip-permissions --settings /shared/.claude/settings.json",
			settings: "/seat/.gt-seat/settings.json",
			wantNot:  "/shared/.claude/settings.json",
			wantHas:  []string{"--settings /seat/.gt-seat/settings.json"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RestrictClaudeCommand(tt.in, tt.settings)
			if tt.wantNot != "" && strings.Contains(got, tt.wantNot) {
				t.Errorf("output still contains %q: %s", tt.wantNot, got)
			}
			for _, want := range tt.wantHas {
				if !strings.Contains(got, want) {
					t.Errorf("output missing %q: %s", want, got)
				}
			}
		})
	}
}
