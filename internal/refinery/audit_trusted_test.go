package refinery

import (
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// Tests for the trusted operator de-escalation (solo) and override honoring:
// the pure pieces the auditGate I/O shell composes. filterVerdictsBySeat is the
// mechanism that makes a de-escalated panel tally a single seat; together with
// decideAuditAction at panelSize 1 it reproduces the solo gate.

func TestFilterVerdictsBySeat_KeepsOnlyRetained(t *testing.T) {
	verdicts := []*beads.Issue{
		verdictIssue("v1", VerdictLabels("mr1", "head", "Mary", 1, VerdictApprove)...),
		verdictIssue("v2", VerdictLabels("mr1", "head", "Teresa", 1, VerdictRequestChanges)...),
		verdictIssue("v3", VerdictLabels("mr1", "head", "Agnes", 1, VerdictApprove)...),
	}
	got := filterVerdictsBySeat(verdicts, "Mary")
	if len(got) != 1 || got[0].ID != "v1" {
		t.Fatalf("filterVerdictsBySeat = %v, want only v1 (Mary)", got)
	}
}

func TestFilterVerdictsBySeat_NoneMatch(t *testing.T) {
	verdicts := []*beads.Issue{
		verdictIssue("v1", VerdictLabels("mr1", "head", "Mary", 1, VerdictApprove)...),
	}
	if got := filterVerdictsBySeat(verdicts, "Lucia"); len(got) != 0 {
		t.Errorf("filterVerdictsBySeat = %v, want empty", got)
	}
}

// Solo de-escalation: a coven that previously dissented (Teresa) must not block
// once the panel is reduced to the retained seat (Mary) and Mary approves. The
// gate composes filterVerdictsBySeat with decideAuditAction at panelSize 1.
func TestSolo_RetainedSeatApprovalGatesPastCovenDissent(t *testing.T) {
	verdicts := []*beads.Issue{
		verdictIssue("v1", VerdictLabels("mr1", "head", "Mary", 1, VerdictApprove)...),
		verdictIssue("v2", VerdictLabels("mr1", "head", "Teresa", 1, VerdictRequestChanges)...),
	}
	filtered := filterVerdictsBySeat(verdicts, "Mary")
	action, _ := decideAuditAction("head", "head", filtered, "mr1", 1)
	if action != actionApproved {
		t.Errorf("action = %v, want actionApproved (solo retained seat approved)", action)
	}
}

// Solo still fails closed on the retained seat's own dissent.
func TestSolo_RetainedSeatDissentStillBlocks(t *testing.T) {
	verdicts := []*beads.Issue{
		verdictIssue("v1", VerdictLabels("mr1", "head", "Mary", 1, VerdictRequestChanges)...),
		verdictIssue("v2", VerdictLabels("mr1", "head", "Teresa", 1, VerdictApprove)...),
	}
	filtered := filterVerdictsBySeat(verdicts, "Mary")
	action, dissents := decideAuditAction("head", "head", filtered, "mr1", 1)
	if action != actionDissent {
		t.Fatalf("action = %v, want actionDissent (retained seat dissented)", action)
	}
	if len(dissents) != 1 || dissents[0].ID != "v1" {
		t.Errorf("dissents = %v, want [v1]", dissents)
	}
}
