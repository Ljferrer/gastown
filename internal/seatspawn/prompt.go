package seatspawn

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/refinery"
)

// auditPrompt renders the self-contained startup prompt handed to a read-only
// Nun seat. The prompt is the formula I/O contract made concrete: it carries
// every in-var the gate passes (mr_id, source_issue, branch, target, audit_sha,
// seat_name, round, depth, flavor) and the single permitted side effect — one
// verdict bead whose labels are SHA-pinned so the Engineer's tally
// (refinery.classifyRound) matches it to exactly this (mr, sha).
//
// It is deliberately self-contained rather than relying on `gt prime`/hook
// rendering of the mol-nun-audit molecule: a seat is not a registered polecat
// (it lives outside polecats/ so the witness zombie patrol never touches it), so
// the standard hook/formula machinery does not apply. The verdict-label strings
// are produced by refinery.VerdictLabels — the same builder the gate tallies
// against — so the instruction can never drift from what the gate reads.
func auditPrompt(req refinery.SeatSpawnRequest) string {
	approveLabels := refinery.VerdictLabels(req.MRID, req.AuditSHA, req.SeatName, req.Round, refinery.VerdictApprove)
	changesLabels := refinery.VerdictLabels(req.MRID, req.AuditSHA, req.SeatName, req.Round, refinery.VerdictRequestChanges)

	depthGuidance := "neighbors depth: review the diff plus the definitions and callers the changed lines directly reference (one hop)."
	if req.Depth == "deep" {
		depthGuidance = "deep depth: follow the impact of the change wherever it leads, not just one hop."
	}

	// Default transport is wisp (an ephemeral bead). "mail" produces a durable
	// verdict bead so operators get a permanent, reviewable trail. Either way the
	// bead carries the nun-verdict / mr / sha / seat / round / verdict labels the
	// gate queries, so the create command differs only by the --ephemeral flag.
	ephemeralFlag := " --ephemeral"
	transportNote := "ephemeral wisp"
	if req.Verdict == "mail" {
		ephemeralFlag = ""
		transportNote = "durable bead (mail transport)"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "You are %s, a READ-ONLY Refinery audit Nun. You are reviewing one merge candidate.\n", req.SeatName)
	b.WriteString("You physically cannot push, commit, merge, or edit code: your worktree is a detached checkout with push disabled, and your tools are restricted to reading and diffing. Do not attempt to mutate anything. Your ONLY output is a single verdict.\n\n")

	b.WriteString("Audit assignment:\n")
	fmt.Fprintf(&b, "  mr_id        = %s\n", req.MRID)
	fmt.Fprintf(&b, "  source_issue = %s\n", req.SourceIssue)
	fmt.Fprintf(&b, "  branch       = %s\n", req.Branch)
	fmt.Fprintf(&b, "  target       = %s\n", req.Target)
	fmt.Fprintf(&b, "  audit_sha    = %s\n", req.AuditSHA)
	fmt.Fprintf(&b, "  round        = %d\n", req.Round)
	fmt.Fprintf(&b, "  depth        = %s\n", req.Depth)
	fmt.Fprintf(&b, "  flavor       = %s  (your assigned review lens)\n\n", req.Flavor)

	b.WriteString("Review the exact change that would land:\n")
	fmt.Fprintf(&b, "  git diff %s...%s\n", req.Target, req.AuditSHA)
	b.WriteString("Read any file at the audited commit without a checkout:\n")
	fmt.Fprintf(&b, "  git show %s:<path>\n", req.AuditSHA)
	fmt.Fprintf(&b, "Discover the plan: read the source bead (bd show %s) and any linked or committed plan files; if none exist, review the code on its own merits.\n\n", req.SourceIssue)

	fmt.Fprintf(&b, "Apply your assigned lens (%s) and %s\n\n", req.Flavor, depthGuidance)

	fmt.Fprintf(&b, "When you have decided, write EXACTLY ONE verdict as a %s. Approve only if the change is correct and safe to merge at this SHA; otherwise request changes and record every blocking concern in the description.\n\n", transportNote)
	b.WriteString("Approve:\n")
	fmt.Fprintf(&b, "  bd create%s -t %q -l %q -d \"<your findings>\"\n", ephemeralFlag, verdictTitle(req, "approve"), strings.Join(approveLabels, ","))
	b.WriteString("Request changes:\n")
	fmt.Fprintf(&b, "  bd create%s -t %q -l %q -d \"<every blocking concern>\"\n\n", ephemeralFlag, verdictTitle(req, "request_changes"), strings.Join(changesLabels, ","))
	b.WriteString("Write only one verdict. After it is written, your audit is complete — do nothing further.")

	return b.String()
}

// verdictTitle builds a stable, human-scannable title for a verdict bead.
func verdictTitle(req refinery.SeatSpawnRequest, verdict string) string {
	return fmt.Sprintf("Nun %s verdict (%s) %s r%d", req.SeatName, verdict, req.MRID, req.Round)
}
