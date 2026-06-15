package refinery

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Tests for the Option C live-path wiring (lgt-k4x): the audit gate runs as a
// side effect of ListReadyMRs and excludes un-approved MRs so the refinery agent
// — which performs the actual merge via `gt refinery ready` — can never select
// an un-audited change. These exercise the pure exclusion seam (excludeAuditPending)
// with an injected gate, so the fail-closed contract is verified without real
// beads/git/seat I/O.

// approvedGate models an MR the gate cleared to merge (audit disabled/approved,
// or an override/solo escape valve resolved inside auditGate): a zero result.
func approvedGate(_ *MRInfo) ProcessResult { return ProcessResult{} }

// pendingGate models an MR whose audit is still in flight: AuditPending=true.
func pendingGate(_ *MRInfo) ProcessResult { return ProcessResult{AuditPending: true} }

func TestExcludeAuditPending_HoldsPendingMRsFromReady(t *testing.T) {
	// Fail-closed: an MR whose audit has not unanimously approved at HEAD must
	// NOT appear in ready, so the agent cannot select it for merge.
	mrs := []*MRInfo{{ID: "mr-approved"}, {ID: "mr-pending"}}
	gate := func(mr *MRInfo) ProcessResult {
		if mr.ID == "mr-pending" {
			return ProcessResult{AuditPending: true}
		}
		return ProcessResult{}
	}

	ready, held := excludeAuditPending(mrs, gate)

	if got := mrIDs(ready); len(got) != 1 || got[0] != "mr-approved" {
		t.Errorf("ready = %v, want [mr-approved] (pending MR must be excluded)", got)
	}
	if len(held) != 1 || held[0].MR.ID != "mr-pending" {
		t.Fatalf("held = %v, want exactly [mr-pending]", held)
	}
}

func TestExcludeAuditPending_AllPendingYieldsEmptyReady(t *testing.T) {
	// With a nil seat spawner (or any not-yet-unanimous panel) every enabled-gate
	// MR parks audit-pending: ready is empty and nothing can merge. Fail-closed
	// and unusable-without-verdicts is the intended state until a verdict lands.
	mrs := []*MRInfo{{ID: "a"}, {ID: "b"}, {ID: "c"}}

	ready, held := excludeAuditPending(mrs, pendingGate)

	if len(ready) != 0 {
		t.Errorf("ready = %v, want empty (all MRs audit-pending → none mergeable)", mrIDs(ready))
	}
	if len(held) != 3 {
		t.Errorf("held = %d, want 3 (every MR parked)", len(held))
	}
}

func TestExcludeAuditPending_ApprovedAndOverrideFlowThrough(t *testing.T) {
	// The escape valves (gt audit override / gt audit solo) resolve inside
	// auditGate to a non-pending result, so a cleared MR flows back into ready.
	mrs := []*MRInfo{{ID: "x"}, {ID: "y"}}

	ready, held := excludeAuditPending(mrs, approvedGate)

	if got := mrIDs(ready); len(got) != 2 || got[0] != "x" || got[1] != "y" {
		t.Errorf("ready = %v, want [x y] (cleared MRs pass through)", got)
	}
	if len(held) != 0 {
		t.Errorf("held = %v, want none", held)
	}
}

func TestExcludeAuditPending_InfraErrorIsHeldWithReason(t *testing.T) {
	// An infra fault mid-gate returns AuditPending=true with an Error; it must be
	// held (fail-closed: never merge on an unresolved gate) and the reason is
	// preserved so the caller can log it distinctly from an ordinary park.
	mrs := []*MRInfo{{ID: "boom"}}
	gate := func(_ *MRInfo) ProcessResult {
		return ProcessResult{AuditPending: true, Error: "audit: resolving HEAD: boom"}
	}

	ready, held := excludeAuditPending(mrs, gate)

	if len(ready) != 0 {
		t.Errorf("ready = %v, want empty (infra-fault MR must be held)", mrIDs(ready))
	}
	if len(held) != 1 || held[0].Result.Error == "" {
		t.Fatalf("held = %v, want one entry carrying the gate error", held)
	}
}

func TestExcludeAuditPending_PreservesOrder(t *testing.T) {
	// Ready ordering (priority sort upstream) must survive the gate filter.
	mrs := []*MRInfo{{ID: "1"}, {ID: "2"}, {ID: "3"}, {ID: "4"}}
	gate := func(mr *MRInfo) ProcessResult {
		if mr.ID == "2" {
			return ProcessResult{AuditPending: true}
		}
		return ProcessResult{}
	}

	ready, _ := excludeAuditPending(mrs, gate)

	want := []string{"1", "3", "4"}
	got := mrIDs(ready)
	if len(got) != len(want) {
		t.Fatalf("ready = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ready[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// writeMergeQueueAuditConfig writes a rig config.json into dir whose
// merge_queue.audit block opts the rig into the Nun audit gate (or leaves it at
// the default-off state). It mirrors the minimal shape LoadConfig parses; only
// the fields the gate reads are set, the rest fall back to DefaultAuditConfig.
func writeMergeQueueAuditConfig(t *testing.T, dir string, auditEnabled bool) {
	t.Helper()
	cfg := `{
  "type": "rig",
  "version": 1,
  "name": "test-rig",
  "merge_queue": {
    "enabled": true,
    "audit": {
      "enabled": ` + map[bool]string{true: "true", false: "false"}[auditEnabled] + `,
      "panel_size": 1,
      "max_seats": 2,
      "verdict": "mail"
    }
  }
}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0644); err != nil {
		t.Fatalf("writing config.json: %v", err)
	}
}

// readyGateSeam replicates the exact daemon-path filter ListReadyMRs runs
// (engineer.go): it feeds candidate MRs through the REAL auditGate via
// excludeAuditPending. This is what `gt refinery ready` consults, so a held MR is
// one the refinery agent can never select for the live merge.
func readyGateSeam(e *Engineer, mrs []*MRInfo) (ready []*MRInfo, held []heldMR) {
	return excludeAuditPending(mrs, func(mr *MRInfo) ProcessResult {
		return e.auditGate(mr, time.Now())
	})
}

// TestReadyGate_LoadConfigEnablesAuditAndHoldsMRs is the lgt-4c7 regression for
// the DAEMON merge path (runRefineryStart → newLiveEngineer → ListReadyMRs).
// Unlike batch_test.go's TestProcessBatch_AuditEnabled_DoesNotBatchMerge, which
// flips the gate on by hand (e.config.Audit.Enabled = true), this exercises the
// real wiring lgt-i2h restored: the live engineer arms the gate via LoadConfig,
// which must read merge_queue.audit.enabled=true from the rig's config.json.
// ListReadyMRs then runs that gate as a side effect (excludeAuditPending) and must
// HOLD every un-approved MR out of `ready`, so the refinery agent — which performs
// the live merge via `gt refinery ready` — can never select an un-audited change.
// Before the fix LoadConfig was never called on the live path, so audit.enabled
// was silently ignored and merges fell open.
func TestReadyGate_LoadConfigEnablesAuditAndHoldsMRs(t *testing.T) {
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()
	createFeatureBranch(t, workDir, "feature-a", "a.txt", "hello a\n")

	writeMergeQueueAuditConfig(t, workDir, true)

	e := newTestEngineer(t, workDir, g)
	if err := e.LoadConfig(); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	// The wiring under test: LoadConfig must have read audit.enabled=true from
	// config.json. Without this the gate below is a no-op and merges fall open.
	if !e.auditEnabled() {
		t.Fatal("LoadConfig must enable the audit gate from config.json merge_queue.audit.enabled=true")
	}

	mrs := []*MRInfo{makeMR("mr-a", "feature-a", "main")}

	ready, held := readyGateSeam(e, mrs)

	// Fail-closed: no Nun has approved, so the MR must never be mergeable.
	if len(ready) != 0 {
		t.Errorf("ready = %v, want empty: an un-approved MR must not be mergeable with audit on", mrIDs(ready))
	}
	if len(held) != 1 || held[0].MR.ID != "mr-a" {
		t.Fatalf("held = %v, want [mr-a] (audit-pending, fail-closed)", held)
	}
}

// TestReadyGate_WithoutLoadConfig_AuditStaysOffMRsFlowThrough is the fail-open
// half of the lgt-4c7/lgt-i2h regression: an Engineer that never calls LoadConfig
// (the pre-fix daemon bug) runs on DefaultMergeQueueConfig with audit disabled, so
// the same gate is inert and the MR flows straight into `ready` — exactly the
// un-audited merge the fix prevents. This pins the merge-blocking behavior to the
// LoadConfig wiring, not to a hand-set flag: the config.json opts the rig in, but
// without LoadConfig the daemon never sees it.
func TestReadyGate_WithoutLoadConfig_AuditStaysOffMRsFlowThrough(t *testing.T) {
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()
	createFeatureBranch(t, workDir, "feature-a", "a.txt", "hello a\n")

	// config.json opts the rig into audit, but the daemon never loads it.
	writeMergeQueueAuditConfig(t, workDir, true)

	e := newTestEngineer(t, workDir, g) // NOTE: no e.LoadConfig()
	if e.auditEnabled() {
		t.Fatal("audit must stay off until LoadConfig reads config.json")
	}

	mrs := []*MRInfo{makeMR("mr-a", "feature-a", "main")}

	ready, held := readyGateSeam(e, mrs)

	if len(held) != 0 {
		t.Errorf("held = %v, want none: audit off means the gate is a no-op", held)
	}
	if got := mrIDs(ready); len(got) != 1 || got[0] != "mr-a" {
		t.Fatalf("ready = %v, want [mr-a] flowing through the disabled gate", got)
	}
}

// TestResolveMRHead_RemoteOnlyBranch is the lgt-6g4 regression. The audit gate
// resolves the MR's HEAD before it can arm a Nun panel. A merge-queue branch
// exists in the refinery worktree only as a remote-tracking ref (origin/<branch>);
// the local ref does not exist. Resolving the BARE branch name fails closed with
// "unknown revision", so the gate parked the queue on every MR in an audit-enabled
// rig and no panel ever convened. resolveMRHead must resolve such a branch via
// origin/<branch> instead — mirroring the remote-ref resolution the merge path
// already uses — so the gate proceeds to arm a panel rather than error.
func TestResolveMRHead_RemoteOnlyBranch(t *testing.T) {
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	// The exact branch shape from the live repro: present only as the remote-tracking
	// ref, no local ref. The @mqedg15y suffix is the mq session marker; it resolves
	// once the ref is fully qualified as refs/remotes/origin/<branch> (lgt-8u1).
	const branch = "polecat/furiosa/gtt-zem@mqedg15y"
	createRemoteOnlyBranch(t, workDir, branch, "a.txt", "hello a\n")

	e := newTestEngineer(t, workDir, g)

	// Precondition (the bug): the bare branch name does NOT resolve, because the
	// local ref was never created. This is what produced
	// "git rev-parse: fatal: ambiguous argument ...: unknown revision".
	if _, err := e.git.Rev(branch); err == nil {
		t.Fatal("precondition failed: bare branch name resolved; the repro requires a remote-only branch")
	}

	// The fix: resolveMRHead resolves the same branch via refs/remotes/origin/<branch>.
	head, err := e.resolveMRHead(branch)
	if err != nil {
		t.Fatalf("resolveMRHead must resolve a remote-only branch via refs/remotes/origin/<branch>, got error: %v", err)
	}

	// It must equal the remote-tracking ref's tip — the commit the merge path lands.
	want := run(t, workDir, "git", "rev-parse", "refs/remotes/origin/"+branch)
	if head != want {
		t.Errorf("resolveMRHead = %s, want %s (refs/remotes/origin/<branch> tip)", head, want)
	}
}

// TestResolveMRHead_QualifiedRefBeatsShadowingLocal is the lgt-8u1 regression. mq
// branches carry an @<session> suffix. The short origin/<branch> form (lgt-6g4) is
// subject to git's DWIM ref-resolution: when a LOCAL ref of the same name shadows
// the remote-tracking ref, `git rev-parse origin/<branch>` resolves to the LOCAL ref
// with only an "ambiguous" warning to stderr and exit 0 — silently SHA-pinning the
// audit verdict to the WRONG commit (worse than the hard failure it usually causes).
// resolveMRHead must fully qualify as refs/remotes/origin/<branch> so it deterministic-
// ally lands the remote tip the merge path will use, regardless of any local shadow.
func TestResolveMRHead_QualifiedRefBeatsShadowingLocal(t *testing.T) {
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	// Remote-only mq branch: refs/remotes/origin/<branch> exists, no plain local ref.
	const branch = "polecat/furiosa/gtt-zem@mqedg15y"
	createRemoteOnlyBranch(t, workDir, branch, "a.txt", "remote a\n")
	remoteTip := run(t, workDir, "git", "rev-parse", "refs/remotes/origin/"+branch)

	// A LOCAL branch literally named "origin/<branch>" at a DIFFERENT commit. This is
	// the refs/heads/origin/<branch> shadow that makes the short origin/<branch> form
	// ambiguous and steers git's DWIM to the wrong (local) ref.
	run(t, workDir, "git", "checkout", "-b", "shadow-src", "main")
	writeFile(t, workDir, "b.txt", "local shadow\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "shadow: divergent commit")
	localShadowTip := run(t, workDir, "git", "rev-parse", "HEAD")
	run(t, workDir, "git", "branch", "origin/"+branch, "shadow-src")
	run(t, workDir, "git", "checkout", "main")

	if remoteTip == localShadowTip {
		t.Fatal("precondition failed: remote tip and shadow tip must differ to expose the wrong-SHA bug")
	}

	e := newTestEngineer(t, workDir, g)

	// Document the trap: the short form silently resolves the WRONG (shadow) commit.
	if got, err := e.git.Rev("origin/" + branch); err == nil && got != localShadowTip {
		t.Logf("note: short origin/<branch> resolved %s (expected the shadow %s); DWIM behavior is git-version dependent", got, localShadowTip)
	}

	// The fix: resolveMRHead pins the remote tip, never the local shadow.
	head, err := e.resolveMRHead(branch)
	if err != nil {
		t.Fatalf("resolveMRHead must resolve the @-suffixed mq branch, got error: %v", err)
	}
	if head != remoteTip {
		t.Errorf("resolveMRHead = %s, want %s (remote tip); a shadowing local ref must not win", head, remoteTip)
	}
	if head == localShadowTip {
		t.Errorf("resolveMRHead resolved the shadowing LOCAL ref %s — audit verdict would SHA-pin the wrong commit", head)
	}
}

// TestResolveMRHead_LocalBranchFallback pins the fallback half of the lgt-6g4 fix:
// when a branch exists only as a LOCAL ref (no origin/<branch>), resolveMRHead must
// still resolve it via the bare name. This keeps the gate working for any
// local-only branch and guarantees the origin/-first change did not regress the
// previously-working path.
func TestResolveMRHead_LocalBranchFallback(t *testing.T) {
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	// createFeatureBranch leaves a local ref and never pushes, so origin/feature-a
	// does not exist — the bare-name fallback must carry it.
	createFeatureBranch(t, workDir, "feature-a", "a.txt", "hello a\n")

	e := newTestEngineer(t, workDir, g)

	head, err := e.resolveMRHead("feature-a")
	if err != nil {
		t.Fatalf("resolveMRHead must fall back to the bare branch name for a local-only branch, got error: %v", err)
	}

	want := run(t, workDir, "git", "rev-parse", "feature-a")
	if head != want {
		t.Errorf("resolveMRHead = %s, want %s (local branch tip)", head, want)
	}
}
