// Package seatspawn provides the concrete refinery.SeatSpawner that launches and
// tears down restricted, read-only Nun audit seats. It is the production wiring
// the Engineer's audit gate holds as an interface (internal/refinery/audit.go):
// the Engineer counts verdicts and persists panel state; this package turns a
// refinery.SeatSpawnRequest into a real isolated worktree + a restricted Claude
// session, and reaps both on teardown.
//
// Two layers of read-only enforcement, per docs/design/refinery-nun-audit-gate.md:
//
//	(c) Structural isolation (security floor): internal/seat.Create builds a
//	    DETACHED worktree at the audited SHA with origin push neutralised, so a
//	    seat physically cannot advance a ref regardless of agent behaviour.
//	(b) Claude permission profile (defense-in-depth): the seat launches Claude
//	    WITHOUT --dangerously-skip-permissions and with the curated allow/deny
//	    settings.json (seat.RestrictClaudeCommand), so the headless Nun can
//	    read+diff+write-one-verdict but is hard-blocked from mutation.
//
// Seats deliberately live under <rig>/seats/<name>, NOT polecats/: the witness
// zombie patrol enumerates polecats/ directories, so a Nun placed there would be
// misclassified and nuked mid-audit. The dedicated tree keeps Nuns invisible to
// the polecat lifecycle; the Engineer owns seat liveness via SeatAlive.
package seatspawn

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/refinery"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/seat"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

// auditAgent is the runtime a Nun always runs. audit.model is pinned to Opus
// (a Claude model), so the restricted permission profile (b) — which is
// Claude-specific — actually applies; GT_AGENT is set so tmux liveness checks
// (SeatAlive) resolve the right process. If an operator ever repoints
// audit.model at a non-Claude runtime, the structural floor (c) still holds.
const auditAgent = "claude"

// Spawner is the concrete refinery.SeatSpawner for a single rig. It also
// satisfies refinery.SeatLivenessChecker so the gate can detect a mid-audit Nun
// crash.
type Spawner struct {
	rig      *rig.Rig
	tmux     *tmux.Tmux
	repoPath string // shared worktree/bare repo backing `git worktree add`
	townRoot string
}

// Compile-time proof the Spawner satisfies the gate's interfaces.
var (
	_ refinery.SeatSpawner         = (*Spawner)(nil)
	_ refinery.SeatLivenessChecker = (*Spawner)(nil)
)

// New builds a Spawner for the rig. The shared repo for worktree creation is the
// refinery's worktree (fallback: mayor's), matching how refinery.NewEngineer
// resolves its git dir so seats share the same object store as the merge path.
func New(r *rig.Rig) *Spawner {
	repoPath := filepath.Join(r.Path, "refinery", "rig")
	if _, err := os.Stat(repoPath); os.IsNotExist(err) {
		repoPath = filepath.Join(r.Path, "mayor", "rig")
	}
	return &Spawner{
		rig:      r,
		tmux:     tmux.NewTmux(),
		repoPath: repoPath,
		townRoot: filepath.Dir(r.Path),
	}
}

// seatHome is the seat's home directory, disjoint from polecats/.
func (s *Spawner) seatHome(name string) string {
	return filepath.Join(s.rig.Path, "seats", name)
}

// seatWorktree is where the detached audit worktree is checked out. The
// <home>/<rigname> layout mirrors a polecat clone so the agent sees recognizable
// repo context.
func (s *Spawner) seatWorktree(name string) string {
	return filepath.Join(s.seatHome(name), s.rig.Name)
}

// settingsPath is where the curated read-only settings.json is written, outside
// the tracked tree so it never appears in a diff.
func (s *Spawner) settingsPath(name string) string {
	return filepath.Join(s.seatHome(name), ".gt-seat", "settings.json")
}

// sessionName is the tmux session for a seat. It carries a "seat" segment so it
// can never collide with a polecat session name (gt-<rig>-<polecat>).
func (s *Spawner) sessionName(name string) string {
	return fmt.Sprintf("%s-seat-%s", session.PrefixFor(s.rig.Name), strings.ToLower(name))
}

// SpawnSeat launches a fresh seat against req.AuditSHA (round-1 path). It first
// reaps any stale leftovers for this name (idempotent re-entry after a partial
// previous attempt), builds the isolated worktree, then starts the restricted
// session. On any post-worktree failure it tears the seat back down so a failed
// spawn leaks neither a worktree nor a tmux session.
func (s *Spawner) SpawnSeat(req refinery.SeatSpawnRequest) error {
	name := req.SeatName
	if name == "" {
		return fmt.Errorf("seatspawn: empty seat name")
	}

	// Clear any residue from a prior crashed/partial spawn of this seat name so
	// `git worktree add` does not fail on an existing path.
	_ = s.TeardownSeat(name)

	st, err := seat.Create(seat.Options{
		RepoPath:     s.repoPath,
		SeatPath:     s.seatWorktree(name),
		AuditSHA:     req.AuditSHA,
		Target:       req.Target,
		Remote:       "origin",
		SettingsPath: s.settingsPath(name),
	})
	if err != nil {
		return fmt.Errorf("seatspawn: creating seat worktree for %s: %w", name, err)
	}

	if err := s.startSession(name, st.Path, st.SettingsPath, req); err != nil {
		_ = s.TeardownSeat(name)
		return fmt.Errorf("seatspawn: starting seat session for %s: %w", name, err)
	}
	return nil
}

// RearmSeat re-points an already-resident seat at a new SHA/round WITHOUT tearing
// it down, so the Nun keeps her context window and re-reviews against her own
// prior reading (the persistent-context re-audit the fix loop depends on). The
// spawner — not the Nun — moves the worktree's HEAD (the Nun's tools deny
// checkout), then nudges the live session with the new round's contract. If the
// resident session has died, it falls back to a fresh spawn.
func (s *Spawner) RearmSeat(req refinery.SeatSpawnRequest) error {
	name := req.SeatName
	sess := s.sessionName(name)

	alive, _ := s.tmux.HasSession(sess)
	if !alive {
		// Resident seat is gone; a fresh independent read at the new SHA is the
		// correct fallback (her prior findings already reached the worker via the
		// batched FIX_NEEDED).
		return s.SpawnSeat(req)
	}

	// Move the resident worktree's detached HEAD to the new SHA. Fetch first so a
	// just-pushed fix commit is present in the shared object store.
	g := git.NewGit(s.seatWorktree(name))
	if err := g.Fetch("origin"); err != nil {
		return fmt.Errorf("seatspawn: fetching for re-arm of %s: %w", name, err)
	}
	if err := g.CheckoutDetach(req.AuditSHA); err != nil {
		return fmt.Errorf("seatspawn: re-pointing seat %s to %s: %w", name, req.AuditSHA, err)
	}

	if err := s.tmux.NudgeSession(sess, auditPrompt(req)); err != nil {
		return fmt.Errorf("seatspawn: re-arming nudge to seat %s: %w", name, err)
	}
	return nil
}

// TeardownSeat reaps a seat: kill its session, remove its worktree (pruning git's
// worktree metadata), and delete its home directory. Idempotent — every step is
// guarded so teardown of an already-absent seat is a no-op, which SpawnSeat
// relies on for clean re-entry. Returns the first error encountered but always
// attempts every step.
func (s *Spawner) TeardownSeat(name string) error {
	if name == "" {
		return nil
	}
	var firstErr error

	sess := s.sessionName(name)
	if has, _ := s.tmux.HasSession(sess); has {
		if err := s.tmux.KillSessionWithProcesses(sess); err != nil {
			firstErr = fmt.Errorf("seatspawn: killing seat session %s: %w", name, err)
		}
	}

	wt := s.seatWorktree(name)
	if _, err := os.Stat(wt); err == nil {
		if err := git.NewGit(s.repoPath).WorktreeRemove(wt, true); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("seatspawn: removing seat worktree %s: %w", name, err)
		}
	}

	if err := os.RemoveAll(s.seatHome(name)); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("seatspawn: removing seat home %s: %w", name, err)
	}
	return firstErr
}

// SeatAlive reports whether the seat's agent session is still running. Per the
// SeatLivenessChecker contract a non-nil error means "assume alive" (the gate
// never tears down a Nun it cannot prove dead), so a probe error returns true.
func (s *Spawner) SeatAlive(name string) (bool, error) {
	sess := s.sessionName(name)
	has, err := s.tmux.HasSession(sess)
	if err != nil {
		return true, err // fail-open: assume alive
	}
	if !has {
		return false, nil
	}
	return s.tmux.IsAgentAlive(sess), nil
}

// startSession builds the restricted launch command and starts the tmux session
// with the seat's identity. The command is built as a polecat-shaped Claude
// invocation, then RestrictClaudeCommand strips --dangerously-skip-permissions
// and swaps in the curated settings.json; the model is pinned to the audit model.
// Identity env is seat-scoped (BD_ACTOR=<rig>/seats/<name>) so the Nun's one
// verdict bead is attributed to her, never to a polecat.
func (s *Spawner) startSession(name, workDir, settingsPath string, req refinery.SeatSpawnRequest) error {
	sess := s.sessionName(name)
	prompt := auditPrompt(req)

	command, err := config.BuildStartupCommandFromConfig(config.AgentEnvConfig{
		Role:        "polecat",
		Rig:         s.rig.Name,
		AgentName:   name,
		TownRoot:    s.townRoot,
		Prompt:      prompt,
		SessionName: sess,
	}, s.rig.Path, prompt, "")
	if err != nil {
		return fmt.Errorf("building startup command: %w", err)
	}

	command = seat.RestrictClaudeCommand(command, settingsPath)
	command = appendModelFlag(command, req.Model)

	if err := s.tmux.NewSessionWithCommandAndEnv(sess, workDir, command, s.seatEnv(name, workDir)); err != nil {
		return fmt.Errorf("creating session: %w", err)
	}

	// Record the agent pane id for liveness checks (best-effort).
	if paneID, err := s.tmux.GetPaneID(sess); err == nil {
		_ = s.tmux.SetEnvironment(sess, "GT_PANE_ID", paneID)
	}
	return nil
}

// seatEnv builds the seat's tmux session environment. Identity is seat-scoped and
// disjoint from the polecat namespace so verdict beads, git authorship, and
// liveness detection all attribute to the Nun rather than a worker.
func (s *Spawner) seatEnv(name, workDir string) map[string]string {
	role := fmt.Sprintf("%s/seats/%s", s.rig.Name, name)
	return map[string]string{
		"GT_ROLE":          role,
		"GT_RIG":           s.rig.Name,
		"GT_SEAT":          name,
		"GT_SEAT_PATH":     workDir,
		"GT_ROOT":          s.townRoot,
		"BD_ACTOR":         role,
		"GIT_AUTHOR_NAME":  name,
		"BEADS_AGENT_NAME": name,
		"GT_AGENT":         auditAgent,
		"GT_PROCESS_NAMES": strings.Join(config.ResolveProcessNames(auditAgent, auditAgent), ","),
	}
}

// appendModelFlag pins the Nun's model (audit.model, e.g. "opus") by appending
// --model unless the resolved command already carries one. A no-op when model is
// empty.
func appendModelFlag(command, model string) string {
	if model == "" || strings.Contains(command, "--model") {
		return command
	}
	return command + " --model " + model
}
