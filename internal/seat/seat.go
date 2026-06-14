// Package seat builds restricted, read-only "seat" worktrees for Refinery audit
// Nuns. A seat is a polecat-shaped agent that may ONLY read code, diff a merge
// candidate, and write a verdict — it must be physically unable to mutate code
// or advance refs.
//
// Two independent guarantees, per docs/design/refinery-nun-audit-gate.md:
//
//	(c) Structural isolation (load-bearing): the seat worktree is checked out
//	    DETACHED at the audited SHA — never the live branch ref — and its
//	    origin push URL is neutralised per-worktree, so pushing/ref-advancing
//	    is physically impossible regardless of agent behaviour. Diffs are read
//	    straight from the shared bare repo (`git diff <target>...<sha>`,
//	    `git show <sha>:<path>`); a mutable checkout is never required.
//
//	(b) Claude permission profile (defense-in-depth): the seat spawns Claude
//	    WITHOUT --dangerously-skip-permissions and points it at a curated
//	    settings.json whose allow-list lets the headless Nun read+diff+
//	    write-verdict without ever hanging on a tool prompt, and whose
//	    deny-list hard-blocks Write/Edit/push/commit/merge/gt done.
//
// Guarantee (c) is the security floor and holds for any runtime; (b) is
// Claude-specific hardening layered on top.
package seat

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/steveyegge/gastown/internal/git"
)

// settingsArgRe matches an existing `--settings <value>` (or `--settings=<value>`)
// launch argument, including a single- or double-quoted value. The stock polecat
// command already carries a `--settings <shared>/settings.json` (injected by
// config.withRoleSettingsFlag), so a read-only seat must REPLACE that permissive
// profile, not merely skip past it.
var settingsArgRe = regexp.MustCompile(`--settings(?:=|\s+)(?:'[^']*'|"[^"]*"|\S+)`)

// dangerousSkipFlag is the permission-bypass flag stock polecats launch with.
// Seats must launch without it.
const dangerousSkipFlag = "--dangerously-skip-permissions"

// AllowRules are the tool-permission rules a read-only Nun needs to function
// autonomously: read the tree, diff the merge candidate, and read beads. Without
// a complete allow-list a non-bypass (headless) Claude hangs on the first
// unpermitted tool prompt — see acceptance criterion "headless seat never hangs".
//
// The verdict-write command is intentionally NOT hard-coded here: transport is a
// configurable knob (wisp|mail), so the caller supplies it via SettingsOptions.
var AllowRules = []string{
	"Read",
	"Grep",
	"Glob",
	"Bash(git diff:*)",
	"Bash(git show:*)",
	"Bash(git log:*)",
	"Bash(git status:*)",
	"Bash(bd show:*)",
	"Bash(bd list:*)",
	"Bash(bd ready:*)",
}

// DenyRules hard-block every mutation path, even though defaultMode already
// enforces permissions. Defense-in-depth: an explicit deny cannot be overridden
// by a stray allow and documents intent. Mirrors the deny set named in the
// design doc plus the obvious adjacent ref-mutating git verbs.
var DenyRules = []string{
	"Write",
	"Edit",
	"NotebookEdit",
	"Bash(git push:*)",
	"Bash(git commit:*)",
	"Bash(git merge:*)",
	"Bash(git rebase:*)",
	"Bash(git reset:*)",
	"Bash(git checkout:*)",
	"Bash(gt done:*)",
}

// DefaultVerdictRules are the allow entries for the verdict side effect when the
// caller does not specify a transport. Covers both default transports (wisp =
// a bead, mail = gt mail) so a Nun can record her one permitted side effect.
var DefaultVerdictRules = []string{
	"Bash(bd create:*)",
	"Bash(bd update:*)",
	"Bash(gt mail send:*)",
}

// SettingsOptions configures the curated Claude settings.json for a seat.
type SettingsOptions struct {
	// Verdict overrides the verdict-write allow rules. When empty,
	// DefaultVerdictRules is used.
	Verdict []string
}

// claudeSettings is the subset of Claude's settings.json this package writes.
// Note the absence of "bypassPermissions": defaultMode is "default", so the
// allow/deny lists are actually enforced.
type claudeSettings struct {
	HasCompletedOnboarding bool              `json:"hasCompletedOnboarding"`
	Permissions            claudePermissions `json:"permissions"`
}

type claudePermissions struct {
	DefaultMode string   `json:"defaultMode"`
	Allow       []string `json:"allow"`
	Deny        []string `json:"deny"`
}

// BuildSettings returns the curated settings.json bytes for a read-only seat.
func BuildSettings(opts SettingsOptions) ([]byte, error) {
	verdict := opts.Verdict
	if len(verdict) == 0 {
		verdict = DefaultVerdictRules
	}
	allow := make([]string, 0, len(AllowRules)+len(verdict))
	allow = append(allow, AllowRules...)
	allow = append(allow, verdict...)

	s := claudeSettings{
		HasCompletedOnboarding: true,
		Permissions: claudePermissions{
			// "default" (NOT "bypassPermissions") is the whole point: enforce
			// the allow/deny lists rather than skip the permission system.
			DefaultMode: "default",
			Allow:       allow,
			Deny:        DenyRules,
		},
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshalling seat settings: %w", err)
	}
	return append(b, '\n'), nil
}

// RestrictClaudeCommand transforms a stock polecat launch command into a
// read-only seat command: it strips --dangerously-skip-permissions (so the
// permission system is active) and appends --settings <settingsPath> (so the
// curated allow/deny profile is loaded). It is a no-op on the skip flag if the
// flag is absent, and omits --settings when settingsPath is empty.
//
// Pure and side-effect free so it is unit-testable without spawning tmux/Claude;
// the polecat session manager calls it when starting a restricted seat.
func RestrictClaudeCommand(command, settingsPath string) string {
	out := strings.Replace(command, " "+dangerousSkipFlag, "", 1)
	if settingsPath == "" {
		return out
	}
	replacement := "--settings " + shellQuote(settingsPath)
	if settingsArgRe.MatchString(out) {
		// Replace the permissive shared settings with the seat's curated profile.
		// ReplaceAllLiteral avoids $-expansion from the quoted path.
		return settingsArgRe.ReplaceAllLiteralString(out, replacement)
	}
	return out + " " + replacement
}

// shellQuote wraps a path in single quotes when it contains shell-significant
// characters, matching the conservative quoting used elsewhere for launch args.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.IndexFunc(s, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\'' || r == '"' || r == '$' || r == '`' || r == '\\'
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Options configures creation of a seat worktree.
type Options struct {
	// RepoPath is an existing worktree (or the bare repo) that shares the object
	// store the seat will read from; used to run `git worktree add`.
	RepoPath string
	// SeatPath is where the detached seat worktree is created.
	SeatPath string
	// AuditSHA is the commit the seat is checked out DETACHED at (the SHA under
	// review). The live branch ref is never checked out.
	AuditSHA string
	// Target is the branch ref the merge would land on, used as the left side of
	// the three-dot diff (e.g. "origin/main").
	Target string
	// Remote is the remote whose push is neutralised (default "origin").
	Remote string
	// SettingsPath is where the curated settings.json is written. When empty it
	// defaults to <SeatPath>/.gt-seat/settings.json (outside the tracked tree).
	SettingsPath string
	// VerdictRules overrides the verdict-write allow rules (see SettingsOptions).
	VerdictRules []string
}

// Seat is a created, isolated read-only audit worktree.
type Seat struct {
	Path         string
	AuditSHA     string
	Target       string
	SettingsPath string

	git *git.Git
}

// Create builds the seat worktree: a detached checkout at AuditSHA with push
// neutralised, plus the curated settings.json on disk. It establishes guarantee
// (c) structurally and writes the (b) profile; it does not spawn the agent.
func Create(opts Options) (*Seat, error) {
	if opts.RepoPath == "" {
		return nil, fmt.Errorf("seat: RepoPath is required")
	}
	if opts.SeatPath == "" {
		return nil, fmt.Errorf("seat: SeatPath is required")
	}
	if opts.AuditSHA == "" {
		return nil, fmt.Errorf("seat: AuditSHA is required")
	}
	remote := opts.Remote
	if remote == "" {
		remote = "origin"
	}
	settingsPath := opts.SettingsPath
	if settingsPath == "" {
		settingsPath = filepath.Join(opts.SeatPath, ".gt-seat", "settings.json")
	}

	repoGit := git.NewGit(opts.RepoPath)
	// (c) detached at the audited SHA — never the live branch ref.
	if err := repoGit.WorktreeAddDetached(opts.SeatPath, opts.AuditSHA); err != nil {
		return nil, fmt.Errorf("seat: creating detached worktree at %s: %w", opts.AuditSHA, err)
	}

	seatGit := git.NewGit(opts.SeatPath)
	// (c) neutralise push for this worktree only.
	if err := seatGit.DisablePush(remote); err != nil {
		return nil, fmt.Errorf("seat: disabling push: %w", err)
	}

	// (b) write the curated permission profile.
	settings, err := BuildSettings(SettingsOptions{Verdict: opts.VerdictRules})
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		return nil, fmt.Errorf("seat: creating settings dir: %w", err)
	}
	if err := os.WriteFile(settingsPath, settings, 0o644); err != nil {
		return nil, fmt.Errorf("seat: writing settings.json: %w", err)
	}

	return &Seat{
		Path:         opts.SeatPath,
		AuditSHA:     opts.AuditSHA,
		Target:       opts.Target,
		SettingsPath: settingsPath,
		git:          seatGit,
	}, nil
}

// Diff returns the three-dot diff of the audited SHA against the seat's target
// branch — the exact change the merge would introduce. Reads from the shared
// object store; no remote access.
func (s *Seat) Diff() (string, error) {
	if s.Target == "" {
		return "", fmt.Errorf("seat: no target configured for diff")
	}
	return s.git.DiffThreeDot(s.Target, s.AuditSHA)
}

// ShowFile returns the contents of path at the audited SHA without a checkout.
func (s *Seat) ShowFile(path string) (string, error) {
	return s.git.ShowFile(s.AuditSHA, path)
}
