package refinery

import (
	"testing"
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
