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

	verdictLabel       = "nun-verdict"
	verdictLabelPrefix = "verdict:"

	// auditLabelPending marks an MR whose panel is armed and awaiting verdicts.
	auditLabelPending = "audit-pending"

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

// SetSeatSpawner wires a concrete seat spawner into the Engineer.
func (e *Engineer) SetSeatSpawner(s SeatSpawner) {
	e.seatSpawner = s
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

	deadline := now.Add(time.Duration(cfg.WallClockMin) * time.Minute).UTC().Format(time.RFC3339)
	if err := e.writeAuditState(mr.ID, headSHA, 1, deadline, leased, flavors); err != nil {
		return err
	}
	if err := e.beads.Update(mr.ID, beads.UpdateOptions{AddLabels: []string{auditLabelPending}}); err != nil {
		return err
	}

	if e.seatSpawner != nil {
		for i, name := range leased {
			if err := e.seatSpawner.SpawnSeat(e.seatRequest(mr, headSHA, 1, name, depth, flavors[i])); err != nil {
				return fmt.Errorf("spawning seat %s for MR %s: %w", name, mr.ID, err)
			}
		}
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
// after a send, markFixSent advances AuditFixSentRound to the round; a re-arm
// (worker pushed a fix) increments AuditRound past it, re-enabling the next
// round's send. This is the idempotency guard behind "exactly one FIX_NEEDED
// per round, sent only by the Refinery".
func fixNeededDue(fixSentRound, round int) bool {
	return fixSentRound < round
}

// markFixSent records that the aggregated FIX_NEEDED for the given round has
// already been dispatched, so it is never re-sent while the panel waits for the
// worker's fix.
func (e *Engineer) markFixSent(mrID string, round int) error {
	issue, err := e.beads.Show(mrID)
	if err != nil {
		return err
	}
	f := beads.ParseMRFields(issue)
	if f == nil {
		f = &beads.MRFields{}
	}
	f.AuditFixSentRound = round
	desc := beads.SetMRFields(issue, f)
	return e.beads.Update(mrID, beads.UpdateOptions{Description: &desc})
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

	head, err := e.git.Rev(mr.Branch)
	if err != nil {
		return ProcessResult{AuditPending: true, Error: fmt.Sprintf("audit: resolving HEAD of %s: %v", mr.Branch, err)}
	}

	issue, err := e.beads.Show(mr.ID)
	if err != nil {
		return ProcessResult{AuditPending: true, Error: fmt.Sprintf("audit: reading MR %s: %v", mr.ID, err)}
	}
	f := beads.ParseMRFields(issue)
	if f == nil {
		f = &beads.MRFields{}
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

	action, dissents := decideAuditAction(f.AuditSHA, head, verdicts, mr.ID, size)
	switch action {
	case actionArmFresh:
		if err := e.armFreshPanel(mr, head, size, depth, now); err != nil {
			return ProcessResult{AuditPending: true, Error: fmt.Sprintf("audit: arming panel for %s: %v", mr.ID, err)}
		}
		_, _ = fmt.Fprintf(e.output, "[Engineer] Audit panel armed for MR %s at %s (round 1, %d seat(s), depth=%s) — parking until verdicts land\n", mr.ID, shortSHA(head), size, depth)
		return ProcessResult{AuditPending: true}

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
			findings := aggregateFindings(dissents)
			if err := e.auditFixNotify(mr, f.AuditRound, findings); err != nil {
				_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to send audit FIX_NEEDED for %s: %v (will retry next cycle)\n", mr.ID, err)
				return ProcessResult{AuditPending: true}
			}
			if err := e.markFixSent(mr.ID, f.AuditRound); err != nil {
				_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to record FIX_NEEDED round for %s: %v\n", mr.ID, err)
			}
			_, _ = fmt.Fprintf(e.output, "[Engineer] Audit round %d dissent on MR %s — sent one aggregated FIX_NEEDED to %s; seats held resident\n", f.AuditRound, mr.ID, mr.Worker)
		} else {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Audit round %d still dissenting on MR %s — FIX_NEEDED already sent, awaiting fix\n", f.AuditRound, mr.ID)
		}
		return ProcessResult{AuditPending: true}

	default: // actionPending
		_, _ = fmt.Fprintf(e.output, "[Engineer] Audit not yet unanimous for MR %s at %s — parking\n", mr.ID, shortSHA(head))
		return ProcessResult{AuditPending: true}
	}
}
