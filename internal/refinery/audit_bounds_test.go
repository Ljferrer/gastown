package refinery

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// Pure-logic tests for the audit-gate bounds/backpressure/escalation deciders
// (lgt-c37). These lock in the counting rules; the auditGate I/O shell wires them
// to bead persistence and the Mayor-escalation seam.

// --- round_limit (hard) ---

func TestRoundLimitReached(t *testing.T) {
	cases := []struct {
		dissent, limit int
		want           bool
	}{
		{1, 3, false},
		{2, 3, false},
		{3, 3, true},  // reached
		{4, 3, true},  // past
		{1, 0, false}, // limit 0 disables the hard block
		{5, 0, false},
		{1, -1, false}, // negative disables too
	}
	for _, c := range cases {
		if got := roundLimitReached(c.dissent, c.limit); got != c.want {
			t.Errorf("roundLimitReached(%d, %d) = %v, want %v", c.dissent, c.limit, got, c.want)
		}
	}
}

// --- wall_clock_min (soft) deadline ---

func TestDeadlinePassed(t *testing.T) {
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Minute).Format(time.RFC3339)
	future := now.Add(time.Minute).Format(time.RFC3339)

	if !deadlinePassed(past, now) {
		t.Error("past deadline should be passed")
	}
	if deadlinePassed(future, now) {
		t.Error("future deadline should not be passed")
	}
	// Empty / unparseable never trips (soft signal: notify only, never block).
	if deadlinePassed("", now) {
		t.Error("empty deadline must not be treated as passed")
	}
	if deadlinePassed("not-a-time", now) {
		t.Error("unparseable deadline must not be treated as passed")
	}
}

func TestDeadlineNotifyDue(t *testing.T) {
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Minute).Format(time.RFC3339)
	future := now.Add(time.Minute).Format(time.RFC3339)

	// Past deadline, round 2, never notified → due.
	if !deadlineNotifyDue(past, 2, 0, now) {
		t.Error("past deadline never notified should be due")
	}
	// Already notified this round → debounced.
	if deadlineNotifyDue(past, 2, 2, now) {
		t.Error("already notified this round must not re-notify")
	}
	// Notified an earlier round, new round past deadline → due again.
	if !deadlineNotifyDue(past, 3, 2, now) {
		t.Error("a new round past deadline should re-enable a notify")
	}
	// Deadline not yet passed → never due.
	if deadlineNotifyDue(future, 2, 0, now) {
		t.Error("future deadline must not be due")
	}
}

// --- spawn-failure bounded retries ---

func TestSpawnRetriesExhausted(t *testing.T) {
	// Budget 2 retries after the first attempt: exhausted on the 3rd failure.
	if spawnRetriesExhausted(1, 2) {
		t.Error("1 failure must not be exhausted")
	}
	if spawnRetriesExhausted(2, 2) {
		t.Error("2 failures must not be exhausted (still within retry budget)")
	}
	if !spawnRetriesExhausted(3, 2) {
		t.Error("3 failures must be exhausted")
	}
}

// --- mid-audit-crash respawn budget ---

func TestCrashRespawnExhausted(t *testing.T) {
	// No respawn yet this round → budget available.
	if crashRespawnExhausted(0, 2, 1) {
		t.Error("no respawn this round should leave budget")
	}
	// Already respawned in an earlier round, now a later round → budget available.
	if crashRespawnExhausted(1, 2, 1) {
		t.Error("respawn was a prior round; current round has its own budget")
	}
	// Already respawned THIS round → exhausted.
	if !crashRespawnExhausted(2, 2, 1) {
		t.Error("respawn already used this round must be exhausted")
	}
	// maxRespawns 0 disables respawn entirely.
	if !crashRespawnExhausted(0, 2, 0) {
		t.Error("maxRespawns 0 must report exhausted")
	}
}

// --- crashed-seat detection ---

func TestSeatsWithVerdict(t *testing.T) {
	verdicts := []*beads.Issue{
		verdictIssue("v1", VerdictLabels("mr1", "head", "Mary", 1, VerdictApprove)...),
		verdictIssue("v2", VerdictLabels("mr1", "oldsha", "Teresa", 1, VerdictApprove)...), // stale SHA
		verdictIssue("v3", VerdictLabels("other", "head", "Agnes", 1, VerdictApprove)...),  // other MR
	}
	voted := seatsWithVerdict(verdicts, "mr1", "head")
	if !voted["Mary"] {
		t.Error("Mary voted at head for mr1 — should be counted")
	}
	if voted["Teresa"] {
		t.Error("Teresa's vote is stale-SHA — must not count for head")
	}
	if voted["Agnes"] {
		t.Error("Agnes voted for another MR — must not count")
	}
}

func TestCrashedSeats_DeadAndSilentOnly(t *testing.T) {
	seats := []string{"Mary", "Teresa", "Agnes"}
	verdicts := []*beads.Issue{
		// Mary already voted → never crashed, even if reported dead.
		verdictIssue("v1", VerdictLabels("mr1", "head", "Mary", 1, VerdictApprove)...),
	}
	// Teresa dead+silent → crashed. Agnes alive+silent → just slow, not crashed.
	alive := map[string]bool{"Mary": false, "Teresa": false, "Agnes": true}
	aliveFn := func(name string) (bool, error) { return alive[name], nil }

	crashed := crashedSeats(seats, verdicts, "mr1", "head", aliveFn)
	if len(crashed) != 1 || crashed[0] != "Teresa" {
		t.Errorf("crashedSeats = %v, want [Teresa]", crashed)
	}
}

func TestCrashedSeats_LivenessErrorIsFailOpen(t *testing.T) {
	// A liveness-probe error must be treated as alive — never tear down a Nun we
	// cannot prove dead.
	seats := []string{"Mary"}
	aliveFn := func(string) (bool, error) { return false, errSpawnFailed }
	if crashed := crashedSeats(seats, nil, "mr1", "head", aliveFn); len(crashed) != 0 {
		t.Errorf("liveness error should be fail-open (no crash), got %v", crashed)
	}
}

// --- seatFlavor lookup ---

func TestSeatFlavor(t *testing.T) {
	seats := []string{"Mary", "Teresa", "Agnes"}
	flavors := []string{"correctness", "security", "plan-faithfulness"}
	if got := seatFlavor(seats, flavors, "Teresa"); got != "security" {
		t.Errorf("seatFlavor(Teresa) = %q, want security", got)
	}
	// Missing pairing → holistic general fallback.
	if got := seatFlavor(seats, nil, "Mary"); got != flavorGeneral {
		t.Errorf("seatFlavor with no flavors = %q, want %q", got, flavorGeneral)
	}
	if got := seatFlavor(seats, flavors, "Unknown"); got != flavorGeneral {
		t.Errorf("seatFlavor(unknown seat) = %q, want %q", got, flavorGeneral)
	}
}
