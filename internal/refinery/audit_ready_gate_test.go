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
