package refinery

// Bounds, backpressure and escalation for the Nun audit gate (lgt-c37). The gate
// must never silently pass OR silently fail: every terminal or slow condition
// either parks the MR (fail-closed, retry next cycle) or escalates to the Mayor /
// a human, with all debounce state persisted on the MR bead so the behavior
// survives a Refinery restart. ZFC-clean: the decisions here are pure counting;
// the agents (Nuns) still own every review judgment.
//
// The five guarded conditions:
//   - round_limit (hard):   N consecutive dissenting rounds -> audit-blocked + escalate
//   - wall_clock_min (soft): deadline passes -> notify Mayor, never block/force-merge
//   - seat/roster exhausted: park audit-pending + escalate, debounced once per park
//   - spawn failure:        bounded retries, then park + escalate + block
//   - mid-audit crash:      respawn the dead Nun once, then escalate
//
// Source: docs/design/refinery-nun-audit-gate.md (gh#4168).

import (
	"errors"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

const (
	// auditLabelBlocked is the terminal hard-block label. Once present, the MR
	// never merges, never re-spawns and never re-escalates — it waits for a human
	// / Mayor to intervene (and clear the label) after a round-limit, repeated
	// spawn failure, or repeated mid-audit crash.
	auditLabelBlocked = "audit-blocked"

	// auditSpawnMaxRetries is the number of spawn retries AFTER the first attempt
	// before a fresh-panel spawn failure is treated as terminal (design: 2).
	auditSpawnMaxRetries = 2

	// auditCrashMaxRespawns is the number of times a crashed (dead-session,
	// no-verdict) Nun is respawned within a round before escalating (design: 1).
	auditCrashMaxRespawns = 1
)

// errSpawnFailed wraps a SeatSpawner.SpawnSeat error so auditGate can distinguish
// a spawn failure (bounded-retry path) from seat exhaustion (park path) and from
// generic bead/git errors.
var errSpawnFailed = errors.New("audit: seat spawn failed")

// roundLimitReached reports whether the consecutive-dissent-round count has
// reached the hard, panel-wide round limit. A non-positive limit disables the
// hard block (the gate then loops on dissent indefinitely, never auto-rejecting).
func roundLimitReached(dissentRounds, roundLimit int) bool {
	return roundLimit > 0 && dissentRounds >= roundLimit
}

// deadlinePassed reports whether the soft wall-clock deadline (RFC3339) has
// elapsed as of now. An empty or unparseable deadline is treated as "not passed"
// so a malformed deadline never trips the slowness path (fail-open for the soft
// signal: it only ever notifies, never blocks).
func deadlinePassed(deadline string, now time.Time) bool {
	if deadline == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, deadline)
	if err != nil {
		return false
	}
	return now.After(t)
}

// deadlineNotifyDue reports whether a soft-deadline slowness notification to the
// Mayor is due: the current round's deadline has passed and no notification has
// yet fired for this round. It is purely a notify trigger — it NEVER blocks the
// MR or force-merges it.
func deadlineNotifyDue(deadline string, round, notifiedRound int, now time.Time) bool {
	return notifiedRound < round && deadlinePassed(deadline, now)
}

// spawnRetriesExhausted reports whether the bounded fresh-panel spawn retries are
// used up. attempts is the running count of consecutive failures; maxRetries is
// the retry budget after the first attempt. Exhausted once failures exceed the
// budget (e.g. maxRetries=2 -> exhausted on the 3rd failure).
func spawnRetriesExhausted(attempts, maxRetries int) bool {
	return attempts > maxRetries
}

// crashRespawnExhausted reports whether a crashed Nun has already used up her
// in-round respawn budget. respawnedRound is the round in which a respawn already
// fired; round is the current round. Exhausted when a respawn already happened
// this round (so a second crash in the same round escalates instead of looping).
func crashRespawnExhausted(respawnedRound, round, maxRespawns int) bool {
	// maxRespawns is currently always 1; the count collapses to "did we already
	// respawn in THIS round?" because each round resets the budget.
	return maxRespawns <= 0 || respawnedRound >= round
}

// --- bead persistence for bounds debounce state ---

// recordDissentRound atomically marks the FIX_NEEDED for `round` as sent AND
// advances the consecutive-dissent-round counter. The two move in lockstep so the
// round-limit counter only ever advances on a genuine request_changes round (the
// only caller is the actionDissent path), never on an infra fault.
func (e *Engineer) recordDissentRound(mrID string, round, dissentRounds int) error {
	return e.mutateMRFields(mrID, func(f *beads.MRFields) {
		f.AuditFixSentRound = round
		f.AuditDissentRounds = dissentRounds
	})
}

// setDeadlineNotified records that the soft wall-clock slowness notification for
// `round` has been sent, debouncing it to once per round.
func (e *Engineer) setDeadlineNotified(mrID string, round int) error {
	return e.mutateMRFields(mrID, func(f *beads.MRFields) {
		f.AuditDeadlineNotifiedRound = round
	})
}

// setSpawnAttempts persists the running fresh-panel spawn-failure count.
func (e *Engineer) setSpawnAttempts(mrID string, attempts int) error {
	return e.mutateMRFields(mrID, func(f *beads.MRFields) {
		f.AuditSpawnAttempts = attempts
	})
}

// setParkEscalated persists the seat-exhaustion park-escalation debounce flag.
func (e *Engineer) setParkEscalated(mrID string, escalated bool) error {
	return e.mutateMRFields(mrID, func(f *beads.MRFields) {
		f.AuditParkEscalated = escalated
	})
}

// setRespawnedRound records the round in which a crashed Nun was respawned,
// debouncing the mid-audit-crash respawn to once per round.
func (e *Engineer) setRespawnedRound(mrID string, round int) error {
	return e.mutateMRFields(mrID, func(f *beads.MRFields) {
		f.AuditRespawnedRound = round
	})
}

// mutateMRFields reads the MR bead, applies mutate to its parsed fields, and
// writes the description back. It is the shared read-modify-write used by the
// bounds debounce setters so each persists exactly one concern.
func (e *Engineer) mutateMRFields(mrID string, mutate func(*beads.MRFields)) error {
	issue, err := e.beads.Show(mrID)
	if err != nil {
		return err
	}
	f := beads.ParseMRFields(issue)
	if f == nil {
		f = &beads.MRFields{}
	}
	mutate(f)
	desc := beads.SetMRFields(issue, f)
	return e.beads.Update(mrID, beads.UpdateOptions{Description: &desc})
}

// --- escalation / notification ---

// blockAudit hard-blocks an MR: it stamps the terminal audit-blocked label, drops
// audit-pending, and escalates to the Mayor / a human at HIGH severity. The MR
// will never merge until a human clears the label. Idempotent in effect because
// auditGate short-circuits on the audit-blocked label before reaching here again.
func (e *Engineer) blockAudit(mr *MRInfo, reason string) {
	if err := e.beads.Update(mr.ID, beads.UpdateOptions{
		AddLabels:    []string{auditLabelBlocked},
		RemoveLabels: []string{auditLabelPending},
	}); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to label MR %s audit-blocked: %v\n", mr.ID, err)
	}
	msg := fmt.Sprintf("AUDIT_BLOCKED: MR %s issue=%s branch=%s worker=%s — %s; merge hard-blocked, needs human/Mayor intervention to clear the audit-blocked label",
		mr.ID, mr.SourceIssue, mr.Branch, mr.Worker, reason)
	if err := e.auditMayorNotify(mr, "high", msg); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to escalate audit-blocked MR %s to Mayor: %v\n", mr.ID, err)
	}
	_, _ = fmt.Fprintf(e.output, "[Engineer] Audit HARD-BLOCKED MR %s — %s; escalated to Mayor/human, parked (will not merge)\n", mr.ID, reason)
}
