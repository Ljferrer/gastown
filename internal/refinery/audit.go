package refinery

// Nun audit gate (opt-in, fail-closed). After an MR passes pre-merge gates but
// BEFORE it is eligible for doMerge, a panel of read-only "Nun" seats
// independently reviews the exact SHA being merged. The merge proceeds only when
// the panel approves at the current HEAD. ZFC-clean: Go counts, agents decide —
// this file spawns seats, persists panel state on the MR bead, and tallies
// verdict wisps; the review judgment lives entirely in the mol-nun-audit formula.
//
// The gate is OFF by default (AuditConfig.Enabled=false): stock rigs see no
// behavior change. See docs/design/refinery-nun-audit-gate.md (gh#4168).

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// NunRoster is the dedicated, rotating name pool for audit seats (Nuns),
// disjoint from the worker-polecat roster so a Nun can never collide with a
// worker. Names are leased on spawn and released on teardown, giving a soft
// rig-wide concurrency ceiling of len(NunRoster) concurrent Nuns.
var NunRoster = []string{
	"Mary", "Teresa", "Gertrude", "Lucia", "Frances", "Agnes",
	"Beatrice", "Cecelia", "Dorothy", "Stella", "Elizabeth", "Imelda",
}

// Verdict label constants. Each seat writes exactly one verdict per round whose
// labels SHA-pin the verdict to the reviewed commit; a resubmit (new HEAD)
// automatically makes prior-SHA verdicts non-matching with no explicit
// invalidation.
const (
	VerdictApprove        = "approve"
	VerdictRequestChanges = "request_changes"

	// VerdictLabel tags every Nun verdict bead/wisp. Exported so the read-only
	// `gt audit status` surface can list and tally in-flight verdicts.
	VerdictLabel = "nun-verdict"
	verdictLabel = VerdictLabel

	verdictLabelPrefix = "verdict:"

	// AuditPendingLabel marks an MR whose panel is armed and awaiting verdicts.
	// Exported for the read-only status surface; the internal alias is retained
	// for existing call sites.
	AuditPendingLabel = "audit-pending"

	// auditLabelPending marks an MR whose panel is armed and awaiting verdicts.
	auditLabelPending = AuditPendingLabel

	// auditLabelCoven opts an MR into a coven audit: the panel scales to
	// coven_size seats AND deepens to full agent-judgment impact tracing
	// (depth=deep). Absent, an MR gets the default single-Nun neighbors-depth
	// review.
	auditLabelCoven = "audit:coven"

	// Audit depth tiers. neighbors (default) = the diff plus the definitions and
	// callers the changed lines directly reference (one hop), under a per-round
	// time budget; deep (coven) = follow the impact wherever it leads.
	auditDepthNeighbors = "neighbors"
	auditDepthDeep      = "deep"
)

// NunFlavors is the perspective-diversity roster: each coven seat is spun up with
// a distinct lens so the panel searches divergent angles rather than running N
// identical reads. Seat i is assigned NunFlavors[i % len(NunFlavors)], and that
// flavor is persisted on the MR bead so each Nun keeps her lens across rounds and
// across a Refinery restart. plan-faithfulness is intentionally last so that a
// single extra seat beyond {correctness, security} adds the plan lens.
var NunFlavors = []string{"correctness", "security", "plan-faithfulness"}

// flavorGeneral is the lens for a single-Nun (non-coven) review: a holistic read
// with no narrowed perspective. Perspective diversity only applies for N>1.
const flavorGeneral = "general"

// assignFlavors returns n per-seat lenses. A single seat gets the holistic
// flavorGeneral (one read needs no diversity); n>1 cycles the NunFlavors roster
// so each seat in the coven carries a distinct angle. Assignment is by position,
// so it is stable for a given seat index across rounds.
func assignFlavors(n int) []string {
	if n <= 1 {
		return []string{flavorGeneral}
	}
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = NunFlavors[i%len(NunFlavors)]
	}
	return out
}

// errSeatsExhausted is returned when the Nun roster or the max_seats quota is
// exhausted; the MR stays parked audit-pending and a fresh spawn is attempted
// next patrol cycle (never merges un-audited).
var errSeatsExhausted = errors.New("audit: no Nun seat available (roster or max_seats exhausted)")

// AuditConfig mirrors the per-rig [merge_queue.audit] config block.
type AuditConfig struct {
	Enabled      bool   `json:"enabled"`        // master switch — default false
	Formula      string `json:"formula"`        // bundled reference molecule
	Model        string `json:"model"`          // pinned model for Nuns (e.g. "opus")
	PanelSize    int    `json:"panel_size"`     // seats on a normal MR (default depth=neighbors)
	CovenSize    int    `json:"coven_size"`     // seats when the MR is labeled audit:coven (depth=deep)
	MaxSeats     int    `json:"max_seats"`      // rig-wide concurrent Nun quota
	RoundLimit   int    `json:"round_limit"`    // dissenting rounds before escalate-to-Mayor
	WallClockMin int    `json:"wall_clock_min"` // soft per-round deadline (minutes)
	Verdict      string `json:"verdict"`        // verdict transport: "wisp" | "mail"
}

// DefaultAuditConfig returns the documented defaults: disabled, single-seat,
// wisp transport. Enabling requires an explicit opt-in in the rig's config.json.
func DefaultAuditConfig() *AuditConfig {
	return &AuditConfig{
		Enabled:      false,
		Formula:      "mol-nun-audit",
		Model:        "opus",
		PanelSize:    1,
		CovenSize:    3,
		MaxSeats:     6,
		RoundLimit:   3,
		WallClockMin: 60,
		Verdict:      "wisp",
	}
}

// auditConfigRaw is the pointer-field shape used to merge config.json values
// over the defaults (nil = "not set, keep default"), mirroring LoadConfig's
// existing merge_queue parsing style.
type auditConfigRaw struct {
	Enabled      *bool   `json:"enabled"`
	Formula      *string `json:"formula"`
	Model        *string `json:"model"`
	PanelSize    *int    `json:"panel_size"`
	CovenSize    *int    `json:"coven_size"`
	MaxSeats     *int    `json:"max_seats"`
	RoundLimit   *int    `json:"round_limit"`
	WallClockMin *int    `json:"wall_clock_min"`
	Verdict      *string `json:"verdict"`
}

// apply merges non-nil raw values onto the given config.
func (raw *auditConfigRaw) apply(cfg *AuditConfig) {
	if raw == nil {
		return
	}
	if raw.Enabled != nil {
		cfg.Enabled = *raw.Enabled
	}
	if raw.Formula != nil {
		cfg.Formula = *raw.Formula
	}
	if raw.Model != nil {
		cfg.Model = *raw.Model
	}
	if raw.PanelSize != nil {
		cfg.PanelSize = *raw.PanelSize
	}
	if raw.CovenSize != nil {
		cfg.CovenSize = *raw.CovenSize
	}
	if raw.MaxSeats != nil {
		cfg.MaxSeats = *raw.MaxSeats
	}
	if raw.RoundLimit != nil {
		cfg.RoundLimit = *raw.RoundLimit
	}
	if raw.WallClockMin != nil {
		cfg.WallClockMin = *raw.WallClockMin
	}
	if raw.Verdict != nil {
		cfg.Verdict = *raw.Verdict
	}
}

// VerdictLabels builds the canonical label set for a single verdict wisp. The
// mol-nun-audit formula writes a bead/wisp carrying exactly these labels; the
// refinery tallies on them. round must be 1-based.
func VerdictLabels(mrID, sha, seat string, round int, verdict string) []string {
	return []string{
		verdictLabel,
		"mr:" + mrID,
		"sha:" + sha,
		"seat:" + seat,
		fmt.Sprintf("round:%d", round),
		verdictLabelPrefix + verdict,
	}
}

// ParseVerdict extracts the verdict value ("approve"/"request_changes") from a
// verdict wisp's labels, or "" if there is no verdict: label.
func ParseVerdict(labels []string) string {
	for _, l := range labels {
		if strings.HasPrefix(l, verdictLabelPrefix) {
			return strings.TrimPrefix(l, verdictLabelPrefix)
		}
	}
	return ""
}

// LeaseNun returns the first roster name not present in inUse, and whether one
// was available. inUse is the set of Nun names currently leased rig-wide.
func LeaseNun(inUse map[string]bool) (string, bool) {
	for _, name := range NunRoster {
		if !inUse[name] {
			return name, true
		}
	}
	return "", false
}

// parseSeats splits a comma-separated audit_seats field into trimmed names.
func parseSeats(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// formatSeats joins leased seat names into the audit_seats field value.
func formatSeats(names []string) string {
	return strings.Join(names, ",")
}

// SeatSpawnRequest carries everything the spawner needs to launch one read-only
// Nun seat against an MR's audit SHA.
type SeatSpawnRequest struct {
	MRID        string
	SourceIssue string
	Branch      string
	Target      string
	AuditSHA    string
	SeatName    string
	Round       int
	Formula     string
	Model       string
	Verdict     string
	Depth       string // audit depth tier: "neighbors" (default) or "deep" (coven)
	Flavor      string // perspective lens for this seat (e.g. "correctness"); "general" for a single-Nun review
}

// SeatSpawner launches and tears down restricted read-only audit seats. The
// concrete implementation lives in the command layer (it depends on the polecat
// session manager + internal/seat); the Engineer holds it as an interface so the
// audit gate is unit-testable with a fake. A nil spawner means "no spawn wiring"
// — panel state is still stamped on the bead (so verdicts written out-of-band
// still tally), but no agent is launched.
type SeatSpawner interface {
	// SpawnSeat launches a fresh seat against req.AuditSHA. Used only on the
	// first round (a brand-new panel).
	SpawnSeat(req SeatSpawnRequest) error
	// RearmSeat re-points an already-resident seat at a new SHA/round without
	// tearing it down, so the Nun keeps her context window (and flavor) and
	// re-reviews against her own prior reading. This is the persistent-context
	// re-audit the fix loop depends on: a dissenting Nun is never destroyed
	// between rounds, and prior approvers must re-confirm the new SHA.
	RearmSeat(req SeatSpawnRequest) error
	TeardownSeat(name string) error
}

// SeatLivenessChecker is an OPTIONAL capability a SeatSpawner may implement so the
// audit gate can detect a mid-audit Nun crash — a resident seat whose agent
// session died before it could write a verdict. When the installed spawner does
// not implement it (e.g. the nil-spawner tracer-bullet wiring), crash detection
// is skipped and the soft wall-clock deadline remains the backstop for a silently
// wedged audit. Kept separate from SeatSpawner so the concrete liveness probe can
// land with the session-manager spawn wiring without widening the core interface.
type SeatLivenessChecker interface {
	// SeatAlive reports whether the named seat's agent session is still running.
	// A non-nil error is treated by the gate as "assume alive" (fail-open — never
	// tear down a Nun we cannot prove is dead).
	SeatAlive(name string) (bool, error)
}

// SetSeatSpawner wires a concrete seat spawner into the Engineer.
func (e *Engineer) SetSeatSpawner(s SeatSpawner) {
	e.seatSpawner = s
}

// HasSeatSpawner reports whether a seat spawner is installed. Without one the
// audit gate cannot launch Nuns (audit.go guards spawn on e.seatSpawner != nil),
// so callers on the live merge path use this to verify the gate can actually
// convene before relying on it.
func (e *Engineer) HasSeatSpawner() bool {
	return e.seatSpawner != nil
}

// auditEnabled reports whether the Nun audit gate is configured on and active.
func (e *Engineer) auditEnabled() bool {
	return e.config != nil && e.config.Audit != nil && e.config.Audit.Enabled
}

// panelParams returns the effective seat count and audit depth for an MR given
// its labels. The audit:coven label scales the panel to coven_size seats and
// deepens the review to full impact tracing (depth=deep); without it, the
// default single-Nun (panel_size) neighbors-depth review applies. The returned
// size is the unanimity threshold the tally enforces, so size and depth always
// travel together.
func (e *Engineer) panelParams(labels []string) (size int, depth string) {
	cfg := e.config.Audit
	for _, l := range labels {
		if l == auditLabelCoven {
			return cfg.CovenSize, auditDepthDeep
		}
	}
	return cfg.PanelSize, auditDepthNeighbors
}

// leasedNuns returns the set of Nun names currently leased across all open MR
// beads, excluding excludeMRID (so an MR re-arming its own panel does not count
// its own about-to-be-released seats against the quota). The bead is the source
// of truth, so this survives a Refinery restart.
func (e *Engineer) leasedNuns(excludeMRID string) (map[string]bool, error) {
	inUse := map[string]bool{}
	mrs, err := e.beads.ListMergeRequests(beads.ListOptions{
		Label:    "gt:merge-request",
		Status:   "open",
		Priority: -1,
	})
	if err != nil {
		return nil, err
	}
	for _, mr := range mrs {
		if mr.ID == excludeMRID {
			continue
		}
		f := beads.ParseMRFields(mr)
		if f == nil {
			continue
		}
		for _, n := range parseSeats(f.AuditSeats) {
			inUse[n] = true
		}
	}
	return inUse, nil
}

// writeAuditState persists the panel state onto the MR bead (audit_sha,
// audit_round, audit_deadline, audit_seats, audit_flavors). The bead is the
// source of truth, so the seat→flavor pairing survives a Refinery restart and
// each Nun keeps her lens across rounds. seats and flavors are positionally
// parallel.
func (e *Engineer) writeAuditState(mrID, sha string, round int, deadline string, seats, flavors []string) error {
	issue, err := e.beads.Show(mrID)
	if err != nil {
		return err
	}
	f := beads.ParseMRFields(issue)
	if f == nil {
		f = &beads.MRFields{}
	}
	f.AuditSHA = sha
	f.AuditRound = round
	f.AuditDeadline = deadline
	f.AuditSeats = formatSeats(seats)
	f.AuditFlavors = formatSeats(flavors)
	// A clean (re)arm clears the transient backpressure debounces: the panel is
	// live at this SHA, so any prior spawn-failure streak or seat-exhaustion park
	// is resolved. The consecutive-dissent counter (AuditDissentRounds) is left
	// untouched — it must persist across re-arms to enforce the hard round limit.
	f.AuditSpawnAttempts = 0
	f.AuditParkEscalated = false
	desc := beads.SetMRFields(issue, f)
	return e.beads.Update(mrID, beads.UpdateOptions{Description: &desc})
}

// seatRequest builds the spawn/re-arm request for one seat at the given SHA,
// round, depth tier and perspective lens, filling in the formula/model/verdict
// knobs from config.
func (e *Engineer) seatRequest(mr *MRInfo, sha string, round int, name, depth, flavor string) SeatSpawnRequest {
	cfg := e.config.Audit
	return SeatSpawnRequest{
		MRID:        mr.ID,
		SourceIssue: mr.SourceIssue,
		Branch:      mr.Branch,
		Target:      mr.Target,
		AuditSHA:    sha,
		SeatName:    name,
		Round:       round,
		Formula:     cfg.Formula,
		Model:       cfg.Model,
		Verdict:     cfg.Verdict,
		Depth:       depth,
		Flavor:      flavor,
	}
}

// armFreshPanel arms a brand-new panel for mr at headSHA on round 1: it leases
// `size` Nun names (respecting MaxSeats and the roster), assigns each seat a
// distinct perspective lens, stamps panel state on the bead, labels the MR
// audit-pending, and spawns each seat at the given depth. size and depth are the
// effective coven/default params from panelParams. Returns errSeatsExhausted
// (parked, retry next cycle) when no seat is available. This is the round-1 path
// only; subsequent SHAs reuse the resident seats via rearmPanelForNewSHA so a
// Nun's context (and lens) survives across the fix loop.
func (e *Engineer) armFreshPanel(mr *MRInfo, headSHA string, size int, depth string, now time.Time) error {
	cfg := e.config.Audit

	inUse, err := e.leasedNuns(mr.ID)
	if err != nil {
		return err
	}
	active := len(inUse)

	var leased []string
	for i := 0; i < size; i++ {
		if active+len(leased) >= cfg.MaxSeats {
			return errSeatsExhausted
		}
		name, ok := LeaseNun(inUse)
		if !ok {
			return errSeatsExhausted
		}
		inUse[name] = true
		leased = append(leased, name)
	}

	flavors := assignFlavors(len(leased))

	// Spawn every seat BEFORE persisting the armed panel. A spawn failure must
	// leave no half-armed panel on the bead (audit_sha stays unset) so the next
	// cycle cleanly re-enters arm-fresh and retries; seats already spawned this
	// attempt are torn down so a partial attempt leaks no resident Nun. The error
	// is tagged errSpawnFailed so auditGate can drive the bounded spawn-retry path
	// (distinct from seat exhaustion and from generic bead/git faults).
	if e.seatSpawner != nil {
		var spawned []string
		for i, name := range leased {
			if err := e.seatSpawner.SpawnSeat(e.seatRequest(mr, headSHA, 1, name, depth, flavors[i])); err != nil {
				for _, s := range spawned {
					_ = e.seatSpawner.TeardownSeat(s)
				}
				return fmt.Errorf("spawning seat %s for MR %s: %w: %v", name, mr.ID, errSpawnFailed, err)
			}
			spawned = append(spawned, name)
		}
	}

	deadline := now.Add(time.Duration(cfg.WallClockMin) * time.Minute).UTC().Format(time.RFC3339)
	if err := e.writeAuditState(mr.ID, headSHA, 1, deadline, leased, flavors); err != nil {
		return err
	}
	if err := e.beads.Update(mr.ID, beads.UpdateOptions{AddLabels: []string{auditLabelPending}}); err != nil {
		return err
	}
	return nil
}

// rearmPanelForNewSHA re-arms the existing resident panel against a new branch
// HEAD after the worker pushed a fix. The SAME seats are kept — neither
// dissenters nor prior approvers are torn down, so each Nun keeps her context
// window and flavor (persistent-context re-audit). audit_round is incremented,
// audit_sha is advanced to the new HEAD, and the deadline resets. Each resident
// seat is re-pointed at the new SHA via RearmSeat. SHA-pinning makes every
// prior-SHA verdict (including prior approvals) non-matching automatically, so
// convergent unanimity is always measured on a single current SHA — a prior
// approver must re-confirm the new HEAD before the panel can pass.
//
// AuditFixSentRound is intentionally left untouched: incrementing the round
// makes it strictly less than the new audit_round, which re-enables exactly one
// aggregated FIX_NEEDED if this new SHA also dissents.
func (e *Engineer) rearmPanelForNewSHA(mr *MRInfo, f *beads.MRFields, headSHA, depth string, now time.Time) error {
	cfg := e.config.Audit
	seats := parseSeats(f.AuditSeats)
	round := f.AuditRound + 1

	// Each resident Nun keeps her assigned lens across rounds (persisted parallel
	// to the seat names). Fall back to a fresh assignment if the pairing is
	// missing or out of sync (e.g. a panel armed before flavors were persisted).
	flavors := parseSeats(f.AuditFlavors)
	if len(flavors) != len(seats) {
		flavors = assignFlavors(len(seats))
	}

	deadline := now.Add(time.Duration(cfg.WallClockMin) * time.Minute).UTC().Format(time.RFC3339)
	if err := e.writeAuditState(mr.ID, headSHA, round, deadline, seats, flavors); err != nil {
		return err
	}

	if e.seatSpawner != nil {
		for i, name := range seats {
			if err := e.seatSpawner.RearmSeat(e.seatRequest(mr, headSHA, round, name, depth, flavors[i])); err != nil {
				return fmt.Errorf("re-arming seat %s for MR %s: %w", name, mr.ID, err)
			}
		}
	}
	return nil
}

// roundOutcome is the panel's collective verdict at a single SHA.
type roundOutcome int

const (
	roundPending  roundOutcome = iota // verdicts not all in yet; no dissent
	roundApproved                     // >= panelSize approvals, zero dissent
	roundDissent                      // at least one request_changes — round ends now
)

// classifyRound classifies the panel's verdicts pinned to (mrID, sha) and
// returns the dissenting verdict beads (for findings aggregation). Any single
// request_changes makes the round roundDissent immediately (fail-closed); a
// round is roundApproved only when at least panelSize approvals and zero
// dissents are pinned to the SHA. Verdicts for another MR or SHA are ignored, so
// a stale-SHA approval never counts toward the current HEAD.
func classifyRound(verdicts []*beads.Issue, mrID, sha string, panelSize int) (roundOutcome, []*beads.Issue) {
	mrLabel := "mr:" + mrID
	shaLabel := "sha:" + sha
	approve := 0
	var dissents []*beads.Issue
	for _, v := range verdicts {
		if !beads.HasLabel(v, mrLabel) || !beads.HasLabel(v, shaLabel) {
			continue
		}
		switch ParseVerdict(v.Labels) {
		case VerdictRequestChanges:
			dissents = append(dissents, v)
		case VerdictApprove:
			approve++
		}
	}
	if len(dissents) > 0 {
		return roundDissent, dissents
	}
	if approve >= panelSize {
		return roundApproved, nil
	}
	return roundPending, nil
}

// tallyVerdicts reports whether the panel unanimously approved (mrID, headSHA).
// It is the approve-only view of classifyRound, retained for callers/tests that
// only need the merge-eligibility boolean.
func tallyVerdicts(verdicts []*beads.Issue, mrID, headSHA string, panelSize int) bool {
	outcome, _ := classifyRound(verdicts, mrID, headSHA, panelSize)
	return outcome == roundApproved
}

// auditAction is the single next step the audit gate must take for an MR,
// computed purely from persisted panel state, the live HEAD, and the verdicts.
type auditAction int

const (
	actionArmFresh auditAction = iota // no panel yet — lease seats, round 1
	actionRearm                       // HEAD moved (worker pushed a fix) — re-arm resident seats, round++
	actionApproved                    // unanimous live approval at HEAD — teardown and merge
	actionDissent                     // a Nun requested changes — aggregate findings, FIX_NEEDED, park
	actionPending                     // verdicts not all in — park silently
)

// decideAuditAction is the pure decision core of the fix loop (ZFC: Go counts,
// agents decide). Given the panel SHA on the bead and the live branch HEAD plus
// the panel's verdicts, it returns the action the Refinery must take and, for a
// dissenting round, the dissenting verdict beads. A moved HEAD always wins over
// a verdict tally so approvals/dissents are only ever read against the SHA the
// panel is currently armed on.
func decideAuditAction(auditSHA, head string, verdicts []*beads.Issue, mrID string, panelSize int) (auditAction, []*beads.Issue) {
	if auditSHA == "" {
		return actionArmFresh, nil
	}
	if auditSHA != head {
		return actionRearm, nil
	}
	outcome, dissents := classifyRound(verdicts, mrID, head, panelSize)
	switch outcome {
	case roundApproved:
		return actionApproved, nil
	case roundDissent:
		return actionDissent, dissents
	default:
		return actionPending, nil
	}
}

// seatLabelPrefix tags a verdict wisp with the Nun seat that authored it.
const seatLabelPrefix = "seat:"

// parseSeatLabel extracts the seat name from a verdict bead's labels, or "" if
// none is present.
func parseSeatLabel(labels []string) string {
	for _, l := range labels {
		if strings.HasPrefix(l, seatLabelPrefix) {
			return strings.TrimPrefix(l, seatLabelPrefix)
		}
	}
	return ""
}

// aggregateFindings flattens every dissenting Nun's findings into one body for
// the single per-round FIX_NEEDED. Each dissent is attributed to its seat so the
// worker can address all reviewers' points in one fix and push a single SHA.
func aggregateFindings(dissents []*beads.Issue) string {
	var b strings.Builder
	for _, d := range dissents {
		seat := parseSeatLabel(d.Labels)
		if seat == "" {
			seat = "Nun"
		}
		findings := strings.TrimSpace(d.Description)
		if findings == "" {
			findings = "(no findings recorded)"
		}
		fmt.Fprintf(&b, "- [%s] %s\n", seat, findings)
	}
	return strings.TrimRight(b.String(), "\n")
}

// fixNeededDue reports whether the single aggregated FIX_NEEDED for the current
// dissenting round still needs to be sent. It is true exactly once per round:
// after a send, recordDissentRound advances AuditFixSentRound to the round (in
// lockstep with the dissent counter); a re-arm (worker pushed a fix) increments
// AuditRound past it, re-enabling the next round's send. This is the idempotency
// guard behind "exactly one FIX_NEEDED per round, sent only by the Refinery".
func fixNeededDue(fixSentRound, round int) bool {
	return fixSentRound < round
}

// verdictWisps lists candidate verdict beads. Default transport is wisp
// (ephemeral); the mail transport writes durable beads. We query both so a
// rig configured with verdict="mail" still tallies.
func (e *Engineer) verdictWisps() ([]*beads.Issue, error) {
	ephemeral, err := e.beads.List(beads.ListOptions{
		Label:     verdictLabel,
		Status:    "all",
		Priority:  -1,
		Ephemeral: true,
	})
	if err != nil {
		return nil, err
	}
	durable, err := e.beads.List(beads.ListOptions{
		Label:    verdictLabel,
		Status:   "all",
		Priority: -1,
	})
	if err != nil {
		// Durable query failure is non-fatal when we already have wisps.
		return ephemeral, nil
	}
	return append(ephemeral, durable...), nil
}

// teardownAuditPanel releases the MR's leased seats and clears the audit-pending
// label after the panel has approved. The audit_* fields are left on the bead as
// an audit trail (the MR is about to merge and close anyway).
func (e *Engineer) teardownAuditPanel(mrID string, f *beads.MRFields) {
	if f != nil && e.seatSpawner != nil {
		for _, n := range parseSeats(f.AuditSeats) {
			_ = e.seatSpawner.TeardownSeat(n)
		}
	}
	_ = e.beads.Update(mrID, beads.UpdateOptions{RemoveLabels: []string{auditLabelPending}})
}

// soloShrinkPanel enforces a stamped de-escalation (gt audit solo) on an
// already-armed panel: if more than one Nun is resident, it tears down every seat
// after the first and truncates the persisted roster/flavors to that single
// retained seat. It returns the retained seat name (or "" when no panel is armed
// yet, in which case the fresh panel is armed at size 1 from the start).
// Idempotent: a panel already at one seat is left untouched.
func (e *Engineer) soloShrinkPanel(mrID string, f *beads.MRFields) (string, error) {
	seats := parseSeats(f.AuditSeats)
	if len(seats) == 0 {
		return "", nil
	}
	retained := seats[0]
	if len(seats) == 1 {
		return retained, nil
	}
	if e.seatSpawner != nil {
		for _, n := range seats[1:] {
			_ = e.seatSpawner.TeardownSeat(n)
		}
	}
	f.AuditSeats = retained
	if flavors := parseSeats(f.AuditFlavors); len(flavors) > 0 {
		f.AuditFlavors = flavors[0]
	}
	issue, err := e.beads.Show(mrID)
	if err != nil {
		return "", err
	}
	desc := beads.SetMRFields(issue, f)
	if err := e.beads.Update(mrID, beads.UpdateOptions{Description: &desc}); err != nil {
		return "", err
	}
	_, _ = fmt.Fprintf(e.output, "[Engineer] Audit de-escalated to solo on MR %s by %s — retained seat %s, tore down %d seat(s)\n", mrID, f.AuditSolo, retained, len(seats)-1)
	return retained, nil
}

// filterVerdictsBySeat returns only the verdict beads authored by the named seat.
// Used under a solo de-escalation so the lone retained Nun's verdict is the only
// one that gates, ignoring any verdict a now-torn-down seat left behind for the
// SHA.
func filterVerdictsBySeat(verdicts []*beads.Issue, seat string) []*beads.Issue {
	out := make([]*beads.Issue, 0, len(verdicts))
	for _, v := range verdicts {
		if parseSeatLabel(v.Labels) == seat {
			out = append(out, v)
		}
	}
	return out
}

// resolveMRHead resolves the tip commit of an MR's branch. The merge-queue branch
// lives on the remote and, in the refinery worktree, is typically present only as
// a remote-tracking ref — the local ref does not exist — so it is resolved against
// the remote-tracking ref first, falling back to the bare name for the local-branch
// case. Resolving the bare name alone fails closed with "unknown revision" and parks
// the merge queue on every MR in an audit-enabled rig (lgt-6g4).
//
// The remote ref is fully qualified as refs/remotes/origin/<branch> rather than the
// short origin/<branch> form (lgt-8u1). mq branches carry an @<session> suffix
// (e.g. polecat/furiosa/gtt-zem@mqedg15y); the short form is subject to git's DWIM
// ref-resolution, which (a) can fail outright on '@'-suffixed names in some git
// versions and (b) — worse — silently resolves to a shadowing local ref of the same
// name with only an "ambiguous" warning to stderr (exit 0), SHA-pinning the audit
// verdict to the wrong commit. The fully-qualified ref is unambiguous and resolves
// the same '@'-suffixed mq ref deterministically across git versions.
func (e *Engineer) resolveMRHead(branch string) (string, error) {
	if head, err := e.git.Rev("refs/remotes/origin/" + branch); err == nil {
		return head, nil
	}
	return e.git.Rev(branch)
}

// auditGate enforces the Nun audit before an MR may merge. A zero ProcessResult
// means the audit passed (or is disabled) and the merge may proceed; a result
// with AuditPending=true means the MR must wait. It drives the refinery-mediated
// batched fix loop: a panel is armed on round 1; a dissenting round ends
// immediately (a single request_changes is enough), the dissenting seats stay
// resident, and the Refinery sends exactly one aggregated FIX_NEEDED per round
// to the worker; when the worker pushes a fix the HEAD moves and the same
// resident panel is re-armed against the new SHA with audit_round++; the loop
// ends only when all N seats hold a live approve for the same current HEAD.
//
// This runs BEFORE doMerge for every MR — including the pre-verified skipGates
// fast-path, which the Nun exists to distrust — so an un-audited change can
// never land.
func (e *Engineer) auditGate(mr *MRInfo, now time.Time) ProcessResult {
	if !e.auditEnabled() {
		return ProcessResult{}
	}

	issue, err := e.beads.Show(mr.ID)
	if err != nil {
		return ProcessResult{AuditPending: true, Error: fmt.Sprintf("audit: reading MR %s: %v", mr.ID, err)}
	}
	f := beads.ParseMRFields(issue)
	if f == nil {
		f = &beads.MRFields{}
	}

	// Trusted operator override (gt audit override): a witness/Mayor force-approved
	// this MR despite unresolved dissent. This is the escape valve that keeps a
	// fail-closed gate from deadlocking a rig — so it is checked FIRST, ahead of
	// even the terminal audit-blocked hard-block, since a round-limit deadlock is
	// exactly the state an operator overrides. The field is honored only because
	// the role-authenticated command stamped it (the action is also recorded as a
	// wisp for the audit trail). Teardown any resident panel and let the merge
	// proceed.
	if f.AuditOverride != "" {
		e.teardownAuditPanel(mr.ID, f)
		_, _ = fmt.Fprintf(e.output, "[Engineer] Audit OVERRIDE on MR %s by %s — force-approved past the gate, eligible to merge\n", mr.ID, f.AuditOverride)
		return ProcessResult{}
	}

	// Terminal hard-block: an MR that already hit the round limit (or exhausted
	// spawn/crash retries) is parked forever, fail-closed. It never re-spawns,
	// re-tallies or re-escalates — a human/Mayor must clear the audit-blocked
	// label (or stamp an override, handled above) after intervening. This check
	// precedes all panel work so a blocked MR is cheap to skip every cycle.
	if beads.HasLabel(issue, auditLabelBlocked) {
		_, _ = fmt.Fprintf(e.output, "[Engineer] MR %s is audit-blocked — parked pending human/Mayor intervention\n", mr.ID)
		return ProcessResult{AuditPending: true}
	}

	head, err := e.resolveMRHead(mr.Branch)
	if err != nil {
		return ProcessResult{AuditPending: true, Error: fmt.Sprintf("audit: resolving HEAD of %s: %v", mr.Branch, err)}
	}

	// The audit:coven label scales the panel to coven_size seats at depth=deep;
	// otherwise the default panel_size/neighbors review applies. size is also the
	// unanimity threshold the tally enforces, and depth/flavor travel with each
	// seat (assigned on arm, persisted, kept across rounds).
	size, depth := e.panelParams(issue.Labels)

	verdicts, err := e.verdictWisps()
	if err != nil {
		return ProcessResult{AuditPending: true, Error: fmt.Sprintf("audit: listing verdicts for %s: %v", mr.ID, err)}
	}

	// Trusted operator de-escalation (gt audit solo): a witness/Mayor reduced this
	// MR to a single-Nun review. This wins over the free audit:coven label —
	// scaling up needs no permission, but scaling down does. Force the unanimity
	// threshold to 1; if a larger panel is already resident, shrink it to the
	// first seat and tally only that seat's verdict so a single Nun truly gates.
	if f.AuditSolo != "" {
		size, depth = 1, auditDepthNeighbors
		retained, err := e.soloShrinkPanel(mr.ID, f)
		if err != nil {
			return ProcessResult{AuditPending: true, Error: fmt.Sprintf("audit: de-escalating %s to solo: %v", mr.ID, err)}
		}
		if retained != "" {
			verdicts = filterVerdictsBySeat(verdicts, retained)
		}
	}

	action, dissents := decideAuditAction(f.AuditSHA, head, verdicts, mr.ID, size)
	switch action {
	case actionArmFresh:
		err := e.armFreshPanel(mr, head, size, depth, now)
		switch {
		case err == nil:
			_, _ = fmt.Fprintf(e.output, "[Engineer] Audit panel armed for MR %s at %s (round 1, %d seat(s), depth=%s) — parking until verdicts land\n", mr.ID, shortSHA(head), size, depth)
			return ProcessResult{AuditPending: true}
		case errors.Is(err, errSeatsExhausted):
			return e.handleSeatsExhausted(mr, f)
		case errors.Is(err, errSpawnFailed):
			return e.handleSpawnFailure(mr, f)
		default:
			return ProcessResult{AuditPending: true, Error: fmt.Sprintf("audit: arming panel for %s: %v", mr.ID, err)}
		}

	case actionRearm:
		if err := e.rearmPanelForNewSHA(mr, f, head, depth, now); err != nil {
			return ProcessResult{AuditPending: true, Error: fmt.Sprintf("audit: re-arming panel for %s: %v", mr.ID, err)}
		}
		_, _ = fmt.Fprintf(e.output, "[Engineer] HEAD of MR %s moved to %s — re-armed resident panel (round %d), parking\n", mr.ID, shortSHA(head), f.AuditRound+1)
		return ProcessResult{AuditPending: true}

	case actionApproved:
		e.teardownAuditPanel(mr.ID, f)
		_, _ = fmt.Fprintf(e.output, "[Engineer] Audit approved for MR %s at %s — eligible to merge\n", mr.ID, shortSHA(head))
		return ProcessResult{}

	case actionDissent:
		// Exactly one aggregated FIX_NEEDED per round, sent only by the Refinery.
		// Seats stay resident (persistent context) until the worker pushes a fix.
		if fixNeededDue(f.AuditFixSentRound, f.AuditRound) {
			// This is a genuine request_changes round — and only such a round —
			// so it advances the consecutive-dissent counter toward round_limit.
			// Infra faults never reach here (they return early with an Error).
			dissentRounds := f.AuditDissentRounds + 1

			if roundLimitReached(dissentRounds, e.config.Audit.RoundLimit) {
				// Hard limit hit: stop asking for fixes. Record the final dissent
				// round (audit trail) and hard-block + escalate to a human/Mayor.
				if err := e.recordDissentRound(mr.ID, f.AuditRound, dissentRounds); err != nil {
					_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to record final dissent round for %s: %v\n", mr.ID, err)
				}
				e.blockAudit(mr, fmt.Sprintf("audit dissented %d consecutive rounds (round_limit=%d)", dissentRounds, e.config.Audit.RoundLimit))
				return ProcessResult{AuditPending: true}
			}

			findings := aggregateFindings(dissents)
			if err := e.auditFixNotify(mr, f.AuditRound, findings); err != nil {
				// Not recorded — the dissent counter and FIX_NEEDED advance in
				// lockstep, so a failed notify retries next cycle without
				// double-counting the round toward round_limit.
				_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to send audit FIX_NEEDED for %s: %v (will retry next cycle)\n", mr.ID, err)
				return ProcessResult{AuditPending: true}
			}
			if err := e.recordDissentRound(mr.ID, f.AuditRound, dissentRounds); err != nil {
				_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to record FIX_NEEDED round for %s: %v\n", mr.ID, err)
			}
			_, _ = fmt.Fprintf(e.output, "[Engineer] Audit round %d dissent on MR %s (%d/%d) — sent one aggregated FIX_NEEDED to %s; seats held resident\n", f.AuditRound, mr.ID, dissentRounds, e.config.Audit.RoundLimit, mr.Worker)
		} else {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Audit round %d still dissenting on MR %s — FIX_NEEDED already sent, awaiting fix\n", f.AuditRound, mr.ID)
		}
		return ProcessResult{AuditPending: true}

	default: // actionPending
		// Verdicts are still outstanding. The MR stays parked (fail-closed — a
		// missing/slow verdict NEVER auto-rejects), but a wedged audit must not be
		// silent: before the soft deadline we try to revive a crashed Nun once;
		// after it we escalate slowness to the Mayor without ever blocking.
		if deadlineNotifyDue(f.AuditDeadline, f.AuditRound, f.AuditDeadlineNotifiedRound, now) {
			msg := fmt.Sprintf("AUDIT_SLOW: MR %s issue=%s — audit round %d passed its wall_clock_min (%dm) deadline with verdicts still outstanding; NOT blocked or force-merged, the audit keeps running",
				mr.ID, mr.SourceIssue, f.AuditRound, e.config.Audit.WallClockMin)
			if err := e.auditMayorNotify(mr, "", msg); err != nil {
				_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to notify Mayor of slow audit for %s: %v (will retry next cycle)\n", mr.ID, err)
			} else {
				if err := e.setDeadlineNotified(mr.ID, f.AuditRound); err != nil {
					_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to record slow-audit notify round for %s: %v\n", mr.ID, err)
				}
				_, _ = fmt.Fprintf(e.output, "[Engineer] Audit round %d on MR %s exceeded wall_clock_min — notified Mayor (non-blocking, audit continues)\n", f.AuditRound, mr.ID)
			}
		} else if checker, ok := e.seatSpawner.(SeatLivenessChecker); ok && !deadlinePassed(f.AuditDeadline, now) {
			// Mid-audit crash check only runs before the deadline (after it, the
			// slow-path notify above covers a wedged audit) and only when the
			// spawner can report seat liveness.
			if crashed := crashedSeats(parseSeats(f.AuditSeats), verdicts, mr.ID, head, checker.SeatAlive); len(crashed) > 0 {
				e.handleCrashedSeats(mr, f, head, depth, crashed, now)
			}
		}
		_, _ = fmt.Fprintf(e.output, "[Engineer] Audit not yet unanimous for MR %s at %s — parking\n", mr.ID, shortSHA(head))
		return ProcessResult{AuditPending: true}
	}
}

// handleSeatsExhausted parks an MR whose panel could not be armed because the Nun
// roster or the max_seats quota is exhausted. The MR is never merged un-audited;
// a fresh spawn is attempted next cycle. The Mayor is notified once per park event
// (debounced via AuditParkEscalated, cleared when a panel eventually arms).
func (e *Engineer) handleSeatsExhausted(mr *MRInfo, f *beads.MRFields) ProcessResult {
	if !f.AuditParkEscalated {
		msg := fmt.Sprintf("AUDIT_PARKED: MR %s issue=%s — no Nun seat available (roster/max_seats exhausted); parked audit-pending, will retry spawn next cycle (never merged un-audited)",
			mr.ID, mr.SourceIssue)
		if err := e.auditMayorNotify(mr, "", msg); err != nil {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to notify Mayor of parked audit for %s: %v\n", mr.ID, err)
		} else {
			if err := e.setParkEscalated(mr.ID, true); err != nil {
				_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to record park-escalation for %s: %v\n", mr.ID, err)
			}
			_, _ = fmt.Fprintf(e.output, "[Engineer] Audit seats exhausted for MR %s — parked, notified Mayor (debounced)\n", mr.ID)
		}
	} else {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Audit seats still exhausted for MR %s — parked, Mayor already notified this park\n", mr.ID)
	}
	return ProcessResult{AuditPending: true}
}

// handleSpawnFailure drives the bounded fresh-panel spawn-retry path. Each failure
// advances AuditSpawnAttempts; while the retry budget remains the MR is parked and
// re-attempted next cycle, and once the budget is exhausted the MR is hard-blocked
// + escalated. A clean arm resets the counter (in writeAuditState).
func (e *Engineer) handleSpawnFailure(mr *MRInfo, f *beads.MRFields) ProcessResult {
	attempts := f.AuditSpawnAttempts + 1
	if err := e.setSpawnAttempts(mr.ID, attempts); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to record spawn attempt for %s: %v\n", mr.ID, err)
	}
	if spawnRetriesExhausted(attempts, auditSpawnMaxRetries) {
		e.blockAudit(mr, fmt.Sprintf("Nun seat spawn failed %d times (retry budget %d exhausted)", attempts, auditSpawnMaxRetries))
		return ProcessResult{AuditPending: true}
	}
	_, _ = fmt.Fprintf(e.output, "[Engineer] Audit seat spawn failed for MR %s (attempt %d/%d) — parked, will retry next cycle\n", mr.ID, attempts, auditSpawnMaxRetries+1)
	return ProcessResult{AuditPending: true}
}

// seatsWithVerdict returns the set of seats that have written a verdict pinned to
// (mrID, sha) — used to tell a crashed-and-silent Nun from one that simply has not
// voted yet but is still alive.
func seatsWithVerdict(verdicts []*beads.Issue, mrID, sha string) map[string]bool {
	mrLabel := "mr:" + mrID
	shaLabel := "sha:" + sha
	voted := map[string]bool{}
	for _, v := range verdicts {
		if !beads.HasLabel(v, mrLabel) || !beads.HasLabel(v, shaLabel) {
			continue
		}
		if s := parseSeatLabel(v.Labels); s != "" {
			voted[s] = true
		}
	}
	return voted
}

// crashedSeats returns the seats that crashed mid-audit: their session is no
// longer alive AND they have not written a verdict pinned to the current SHA. A
// liveness-probe error is treated as "alive" (fail-open — never tear down a Nun
// we cannot prove is dead).
func crashedSeats(seats []string, verdicts []*beads.Issue, mrID, sha string, aliveFn func(string) (bool, error)) []string {
	voted := seatsWithVerdict(verdicts, mrID, sha)
	var crashed []string
	for _, s := range seats {
		if voted[s] {
			continue
		}
		alive, err := aliveFn(s)
		if err != nil || alive {
			continue
		}
		crashed = append(crashed, s)
	}
	return crashed
}

// handleCrashedSeats revives mid-audit-crashed Nuns. The first crash in a round
// respawns each dead seat fresh/clean against the current SHA (debounced via
// AuditRespawnedRound); a second crash in the same round is not transient, so it
// escalates while keeping the MR parked (fail-closed — never merges un-audited).
func (e *Engineer) handleCrashedSeats(mr *MRInfo, f *beads.MRFields, head, depth string, crashed []string, now time.Time) {
	names := strings.Join(crashed, ",")
	if crashRespawnExhausted(f.AuditRespawnedRound, f.AuditRound, auditCrashMaxRespawns) {
		msg := fmt.Sprintf("AUDIT_CRASH: MR %s issue=%s — Nun seat(s) %s crashed again in round %d after a respawn; the audit cannot complete itself, needs intervention (MR stays parked, not merged)",
			mr.ID, mr.SourceIssue, names, f.AuditRound)
		if err := e.auditMayorNotify(mr, "high", msg); err != nil {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to escalate repeat Nun crash for %s: %v\n", mr.ID, err)
		}
		_, _ = fmt.Fprintf(e.output, "[Engineer] Audit MR %s — repeat crash of %s after respawn; escalated to Mayor, parked\n", mr.ID, names)
		return
	}

	seats := parseSeats(f.AuditSeats)
	flavors := parseSeats(f.AuditFlavors)
	if e.seatSpawner != nil {
		for _, name := range crashed {
			if err := e.seatSpawner.SpawnSeat(e.seatRequest(mr, head, f.AuditRound, name, depth, seatFlavor(seats, flavors, name))); err != nil {
				_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to respawn crashed Nun %s for MR %s: %v\n", name, mr.ID, err)
			}
		}
	}
	if err := e.setRespawnedRound(mr.ID, f.AuditRound); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to record crash-respawn round for %s: %v\n", mr.ID, err)
	}
	_, _ = fmt.Fprintf(e.output, "[Engineer] Audit MR %s — respawned crashed Nun(s) %s fresh against %s (round %d)\n", mr.ID, names, shortSHA(head), f.AuditRound)
}

// seatFlavor returns the perspective lens persisted for seat `name` (seats and
// flavors are positionally parallel), falling back to the holistic flavorGeneral
// when the pairing is missing or out of sync.
func seatFlavor(seats, flavors []string, name string) string {
	for i, s := range seats {
		if s == name && i < len(flavors) {
			return flavors[i]
		}
	}
	return flavorGeneral
}
