package refinery

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// Tests for the refinery-mediated batched fix loop (dissent path): round
// classification, the pure decision core, the once-per-round FIX_NEEDED guard,
// and findings aggregation. These exercise the ZFC-clean decision logic; the
// I/O shell (auditGate) wires these together.

// --- classifyRound: pending / approved / dissent ---

func TestClassifyRound_PendingWhenIncomplete(t *testing.T) {
	// One approve but panel_size 2 and no dissent → still waiting.
	verdicts := []*beads.Issue{
		verdictIssue("v1", VerdictLabels("mr1", "head", "Mary", 1, VerdictApprove)...),
	}
	outcome, dissents := classifyRound(verdicts, "mr1", "head", 2)
	if outcome != roundPending {
		t.Errorf("outcome = %v, want roundPending", outcome)
	}
	if dissents != nil {
		t.Errorf("dissents = %v, want nil", dissents)
	}
}

func TestClassifyRound_Approved(t *testing.T) {
	verdicts := []*beads.Issue{
		verdictIssue("v1", VerdictLabels("mr1", "head", "Mary", 1, VerdictApprove)...),
		verdictIssue("v2", VerdictLabels("mr1", "head", "Teresa", 1, VerdictApprove)...),
	}
	outcome, _ := classifyRound(verdicts, "mr1", "head", 2)
	if outcome != roundApproved {
		t.Errorf("outcome = %v, want roundApproved", outcome)
	}
}

func TestClassifyRound_DissentReturnsDissentingBeads(t *testing.T) {
	// Any single request_changes ends the round, even alongside approvals, and
	// the dissenting bead is returned for findings aggregation.
	verdicts := []*beads.Issue{
		verdictIssue("v1", VerdictLabels("mr1", "head", "Mary", 1, VerdictApprove)...),
		verdictIssue("v2", VerdictLabels("mr1", "head", "Teresa", 1, VerdictRequestChanges)...),
	}
	outcome, dissents := classifyRound(verdicts, "mr1", "head", 2)
	if outcome != roundDissent {
		t.Fatalf("outcome = %v, want roundDissent", outcome)
	}
	if len(dissents) != 1 || dissents[0].ID != "v2" {
		t.Errorf("dissents = %v, want [v2]", dissents)
	}
}

func TestClassifyRound_MultipleDissentsCollected(t *testing.T) {
	verdicts := []*beads.Issue{
		verdictIssue("v1", VerdictLabels("mr1", "head", "Mary", 2, VerdictRequestChanges)...),
		verdictIssue("v2", VerdictLabels("mr1", "head", "Teresa", 2, VerdictRequestChanges)...),
	}
	outcome, dissents := classifyRound(verdicts, "mr1", "head", 2)
	if outcome != roundDissent || len(dissents) != 2 {
		t.Errorf("outcome=%v dissents=%d, want roundDissent and 2 dissents", outcome, len(dissents))
	}
}

func TestClassifyRound_StaleSHAApprovalDoesNotConverge(t *testing.T) {
	// Convergent unanimity on a single SHA: a prior-SHA approval must NOT count
	// toward the new HEAD — a prior approver must re-confirm.
	verdicts := []*beads.Issue{
		verdictIssue("v1", VerdictLabels("mr1", "oldsha", "Mary", 1, VerdictApprove)...),
	}
	outcome, _ := classifyRound(verdicts, "mr1", "newsha", 1)
	if outcome != roundPending {
		t.Errorf("stale-SHA approval gave outcome %v, want roundPending (must re-confirm new SHA)", outcome)
	}
}

// --- decideAuditAction: the pure decision core ---

func TestDecideAuditAction_ArmFreshWhenNoPanel(t *testing.T) {
	action, _ := decideAuditAction("", "head", nil, "mr1", 1)
	if action != actionArmFresh {
		t.Errorf("action = %v, want actionArmFresh", action)
	}
}

func TestDecideAuditAction_RearmWhenHeadMoved(t *testing.T) {
	// HEAD moved (worker pushed a fix) wins over any verdict tally: even a full
	// set of approvals at the OLD sha must not merge — re-arm at the new SHA.
	verdicts := []*beads.Issue{
		verdictIssue("v1", VerdictLabels("mr1", "oldsha", "Mary", 1, VerdictApprove)...),
	}
	action, _ := decideAuditAction("oldsha", "newsha", verdicts, "mr1", 1)
	if action != actionRearm {
		t.Errorf("action = %v, want actionRearm", action)
	}
}

func TestDecideAuditAction_ApprovedAtHead(t *testing.T) {
	verdicts := []*beads.Issue{
		verdictIssue("v1", VerdictLabels("mr1", "head", "Mary", 1, VerdictApprove)...),
	}
	action, _ := decideAuditAction("head", "head", verdicts, "mr1", 1)
	if action != actionApproved {
		t.Errorf("action = %v, want actionApproved", action)
	}
}

func TestDecideAuditAction_DissentAtHead(t *testing.T) {
	verdicts := []*beads.Issue{
		verdictIssue("v1", VerdictLabels("mr1", "head", "Mary", 1, VerdictRequestChanges)...),
	}
	action, dissents := decideAuditAction("head", "head", verdicts, "mr1", 1)
	if action != actionDissent {
		t.Fatalf("action = %v, want actionDissent", action)
	}
	if len(dissents) != 1 {
		t.Errorf("dissents = %d, want 1", len(dissents))
	}
}

func TestDecideAuditAction_PendingAtHead(t *testing.T) {
	action, _ := decideAuditAction("head", "head", nil, "mr1", 1)
	if action != actionPending {
		t.Errorf("action = %v, want actionPending", action)
	}
}

// --- once-per-round FIX_NEEDED guard ---

func TestFixNeededDue(t *testing.T) {
	// Round 1, nothing sent yet (default 0) → due.
	if !fixNeededDue(0, 1) {
		t.Error("round 1 with no prior send should be due")
	}
	// Round 1 already sent → not due again this round (idempotent).
	if fixNeededDue(1, 1) {
		t.Error("round 1 already sent must not re-send")
	}
	// Round 2 after a re-arm (prior send was round 1) → due again.
	if !fixNeededDue(1, 2) {
		t.Error("round 2 after re-arm should re-enable a single send")
	}
}

// --- findings aggregation ---

func TestParseSeatLabel(t *testing.T) {
	if got := parseSeatLabel([]string{"nun-verdict", "seat:Lucia", "round:1"}); got != "Lucia" {
		t.Errorf("parseSeatLabel = %q, want Lucia", got)
	}
	if got := parseSeatLabel([]string{"nun-verdict"}); got != "" {
		t.Errorf("parseSeatLabel with no seat = %q, want empty", got)
	}
}

func TestAggregateFindings_AttributesEachSeat(t *testing.T) {
	dissents := []*beads.Issue{
		{ID: "v1", Labels: []string{"seat:Mary"}, Description: "null deref in handler"},
		{ID: "v2", Labels: []string{"seat:Teresa"}, Description: "missing test for retry path"},
	}
	got := aggregateFindings(dissents)
	if !strings.Contains(got, "[Mary] null deref in handler") {
		t.Errorf("aggregate missing Mary's finding:\n%s", got)
	}
	if !strings.Contains(got, "[Teresa] missing test for retry path") {
		t.Errorf("aggregate missing Teresa's finding:\n%s", got)
	}
}

func TestAggregateFindings_HandlesEmptyDescriptionAndSeat(t *testing.T) {
	dissents := []*beads.Issue{
		{ID: "v1", Labels: nil, Description: "   "},
	}
	got := aggregateFindings(dissents)
	if !strings.Contains(got, "[Nun] (no findings recorded)") {
		t.Errorf("aggregate of empty dissent = %q, want fallback seat + findings", got)
	}
}
