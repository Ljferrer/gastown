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
	SpawnSeat(req SeatSpawnRequest) error
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

// ensureAuditPanel arms a fresh panel for mr at headSHA: it releases any prior
// seats this MR held, leases `size` new Nun names (respecting MaxSeats and the
// roster), assigns each seat a distinct perspective lens, stamps panel state on
// the bead, labels the MR audit-pending, and asks the spawner to launch each
// seat at the given depth. size and depth are the effective coven/default params
// from panelParams. Returns errSeatsExhausted (parked, retry next cycle) when no
// seat is available.
func (e *Engineer) ensureAuditPanel(mr *MRInfo, headSHA string, size int, depth string, now time.Time) error {
	cfg := e.config.Audit

	// Release any seats from a prior round/SHA before re-leasing.
	if issue, err := e.beads.Show(mr.ID); err == nil {
		if f := beads.ParseMRFields(issue); f != nil {
			for _, n := range parseSeats(f.AuditSeats) {
				if e.seatSpawner != nil {
					_ = e.seatSpawner.TeardownSeat(n)
				}
			}
		}
	}

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
			req := SeatSpawnRequest{
				MRID:        mr.ID,
				SourceIssue: mr.SourceIssue,
				Branch:      mr.Branch,
				Target:      mr.Target,
				AuditSHA:    headSHA,
				SeatName:    name,
				Round:       1,
				Formula:     cfg.Formula,
				Model:       cfg.Model,
				Verdict:     cfg.Verdict,
				Depth:       depth,
				Flavor:      flavors[i],
			}
			if err := e.seatSpawner.SpawnSeat(req); err != nil {
				return fmt.Errorf("spawning seat %s for MR %s: %w", name, mr.ID, err)
			}
		}
	}
	return nil
}

// auditApproved tallies verdict wisps for mrID at headSHA. It returns true only
// when at least panelSize approve verdicts and zero request_changes verdicts are
// pinned to headSHA. Any single request_changes ends the round immediately
// (fail-closed). SHA-pinning means verdicts for a stale SHA never match.
func (e *Engineer) auditApproved(mrID, headSHA string, panelSize int) (bool, error) {
	verdicts, err := e.verdictWisps()
	if err != nil {
		return false, err
	}
	return tallyVerdicts(verdicts, mrID, headSHA, panelSize), nil
}

// tallyVerdicts is the pure verdict-counting core: it returns true only when at
// least panelSize approve verdicts and zero request_changes verdicts are pinned
// to (mrID, headSHA). Any single request_changes returns false immediately
// (fail-closed). Verdicts for any other MR or SHA are ignored, so a stale-SHA
// approval never counts.
func tallyVerdicts(verdicts []*beads.Issue, mrID, headSHA string, panelSize int) bool {
	mrLabel := "mr:" + mrID
	shaLabel := "sha:" + headSHA
	approve := 0
	for _, v := range verdicts {
		if !beads.HasLabel(v, mrLabel) || !beads.HasLabel(v, shaLabel) {
			continue
		}
		switch ParseVerdict(v.Labels) {
		case VerdictRequestChanges:
			return false // dissent ends the round
		case VerdictApprove:
			approve++
		}
	}
	return approve >= panelSize
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
// with AuditPending=true means the MR must wait (a panel was just armed, the
// HEAD moved, or verdicts are not yet unanimous). This runs BEFORE doMerge for
// every MR — including the pre-verified skipGates fast-path, which the Nun
// exists to distrust — so an un-audited change can never land.
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
	auditSHA := ""
	if f != nil {
		auditSHA = f.AuditSHA
	}

	// The audit:coven label scales the panel to coven_size seats at depth=deep;
	// otherwise the default panel_size/neighbors review applies. size is also the
	// unanimity threshold the tally enforces below.
	size, depth := e.panelParams(issue.Labels)

	// No panel yet, or HEAD moved since the panel was armed → (re)arm a fresh
	// panel at the live HEAD and park. Approval is always against the current SHA.
	if auditSHA == "" || auditSHA != head {
		if err := e.ensureAuditPanel(mr, head, size, depth, now); err != nil {
			return ProcessResult{AuditPending: true, Error: fmt.Sprintf("audit: arming panel for %s: %v", mr.ID, err)}
		}
		_, _ = fmt.Fprintf(e.output, "[Engineer] Audit panel armed for MR %s at %s (%d seat(s), depth=%s) — parking until verdicts land\n", mr.ID, shortSHA(head), size, depth)
		return ProcessResult{AuditPending: true}
	}

	// Panel armed at the live HEAD — tally verdicts. Unanimity across all `size`
	// seats at this single SHA is required.
	ok, err := e.auditApproved(mr.ID, head, size)
	if err != nil {
		return ProcessResult{AuditPending: true, Error: fmt.Sprintf("audit: tallying %s: %v", mr.ID, err)}
	}
	if !ok {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Audit not yet unanimous for MR %s at %s — parking\n", mr.ID, shortSHA(head))
		return ProcessResult{AuditPending: true}
	}

	// Unanimous approval at the live HEAD → release seats and let the merge proceed.
	e.teardownAuditPanel(mr.ID, f)
	_, _ = fmt.Fprintf(e.output, "[Engineer] Audit approved for MR %s at %s — eligible to merge\n", mr.ID, shortSHA(head))
	return ProcessResult{}
}
