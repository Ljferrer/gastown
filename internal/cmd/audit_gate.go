package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/refinery"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

// Audit-gate operator surface (gt audit enable|disable|solo|override|status).
//
// These subcommands sit alongside the existing work-history query (gt audit
// with no subcommand). They drive the Refinery's Nun audit gate:
//
//   - enable/disable <rig>  (Mayor-only)    toggle the per-rig opt-in
//   - solo <mr>             (witness/Mayor) de-escalate a panel to one Nun
//   - override <mr>         (witness/Mayor) force-approve past unresolved dissent
//   - status [<rig>|<mr>]   (read-only)     view in-flight panels
//
// solo/override stamp TRUSTED fields on the MR bead — not raw labels — because a
// label carries no actor provenance. The field is trusted precisely because its
// only writer is a role-authenticated command that verified the caller first.
// The free audit:coven label needs no command (more scrutiny needs no
// permission); only de-escalation and override are gated.

var auditGateStatusJSON bool

var auditEnableCmd = &cobra.Command{
	Use:   "enable <rig>",
	Short: "Enable the Nun audit gate for a rig (Mayor-only)",
	Long: `Turn on the opt-in Nun audit gate for a rig.

Writes merge_queue.audit.enabled=true to the rig's config.json. Restricted to
the Mayor: rig-level audit opt-in/out is a town-coordination decision.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error { return runAuditToggle(args[0], true) },
}

var auditDisableCmd = &cobra.Command{
	Use:   "disable <rig>",
	Short: "Disable the Nun audit gate for a rig (Mayor-only)",
	Long: `Turn off the Nun audit gate for a rig.

Writes merge_queue.audit.enabled=false to the rig's config.json. Restricted to
the Mayor.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error { return runAuditToggle(args[0], false) },
}

var auditSoloCmd = &cobra.Command{
	Use:   "solo <mr>",
	Short: "De-escalate an MR's audit panel to a single Nun (witness/Mayor-only)",
	Long: `De-escalate a merge request's audit to a single-Nun review.

Stamps a trusted de-escalation field (with actor provenance) on the MR bead that
the Refinery honors by reducing the panel to one seat — overriding the free
audit:coven label. Scaling scrutiny up needs no permission; scaling it down does,
so this is restricted to the witness or the Mayor.`,
	Args: cobra.ExactArgs(1),
	RunE: runAuditSolo,
}

var auditOverrideCmd = &cobra.Command{
	Use:   "override <mr>",
	Short: "Force-approve an audit-blocked MR past unresolved dissent (witness/Mayor-only)",
	Long: `Force-approve a merge request despite an unresolved audit dissent.

This is the escape valve that keeps a fail-closed gate from deadlocking a rig.
Stamps a trusted override field (with actor provenance) on the MR bead AND
records the action as a wisp for the audit trail. Restricted to the witness or
the Mayor.`,
	Args: cobra.ExactArgs(1),
	RunE: runAuditOverride,
}

var auditStatusCmd = &cobra.Command{
	Use:   "status [<rig>|<mr>]",
	Short: "Show in-flight audit panels, verdicts, rounds, and deadlines (read-only)",
	Long: `Show the live state of Nun audit panels.

With no argument, shows in-flight panels for the current rig. With a rig name,
shows that rig's panels; with an MR id, shows that single panel. Read-only — no
role restriction.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runAuditStatus,
}

func init() {
	auditStatusCmd.Flags().BoolVar(&auditGateStatusJSON, "json", false, "Output as JSON")

	auditCmd.AddCommand(auditEnableCmd)
	auditCmd.AddCommand(auditDisableCmd)
	auditCmd.AddCommand(auditSoloCmd)
	auditCmd.AddCommand(auditOverrideCmd)
	auditCmd.AddCommand(auditStatusCmd)
}

// detectActorRole returns the caller's address and normalized role, derived from
// GT_ROLE (or cwd-based detection). Role is one of constants.Role* or "" when it
// cannot be resolved (e.g. a human overseer at the terminal).
func detectActorRole() (actor, role string) {
	actor = detectSender()
	return actor, roleFromAddress(actor)
}

// roleFromAddress normalizes a Gas Town address into a role constant.
func roleFromAddress(addr string) string {
	a := strings.TrimSuffix(strings.TrimSpace(addr), "/")
	if a == "" {
		return ""
	}
	parts := strings.Split(a, "/")
	last := parts[len(parts)-1]
	switch last {
	case constants.RoleMayor:
		return constants.RoleMayor
	case constants.RoleWitness:
		return constants.RoleWitness
	case constants.RoleRefinery:
		return constants.RoleRefinery
	case constants.RoleDeacon:
		return constants.RoleDeacon
	}
	if len(parts) >= 2 {
		switch parts[len(parts)-2] {
		case "crew":
			return constants.RoleCrew
		case "polecats":
			return constants.RolePolecat
		}
	}
	return ""
}

// requireRole verifies the caller holds one of the allowed roles, returning an
// actionable error otherwise. The verified actor address is returned for stamping.
func requireRole(action string, allowed ...string) (actor string, err error) {
	actor, role := detectActorRole()
	for _, a := range allowed {
		if role == a {
			return actor, nil
		}
	}
	return "", fmt.Errorf("%s is restricted to %s (you are %q, role %q)",
		action, strings.Join(allowed, "/"), actor, role)
}

// resolveRig resolves a rig by name; when name is empty it falls back to the
// current rig (cwd or GT_RIG).
func resolveRig(townRoot, name string) (string, *rig.Rig, error) {
	if name == "" {
		return findCurrentRig(townRoot)
	}
	rigsConfig, err := config.LoadRigsConfig(filepath.Join(townRoot, "mayor", "rigs.json"))
	if err != nil {
		rigsConfig = &config.RigsConfig{Rigs: make(map[string]config.RigEntry)}
	}
	rigMgr := rig.NewManager(townRoot, rigsConfig, git.NewGit(townRoot))
	r, err := rigMgr.GetRig(name)
	if err != nil {
		return "", nil, fmt.Errorf("rig %q not found: %w", name, err)
	}
	return name, r, nil
}

func runAuditToggle(rigName string, enabled bool) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	if _, err := requireRole("gt audit enable/disable", constants.RoleMayor); err != nil {
		return err
	}
	name, r, err := resolveRig(townRoot, rigName)
	if err != nil {
		return err
	}
	if err := setRigAuditEnabled(r.Path, enabled); err != nil {
		return fmt.Errorf("updating audit config for rig %q: %w", name, err)
	}
	state := "disabled"
	mark := style.Dim.Render("○")
	if enabled {
		state, mark = "enabled", style.Success.Render("✓")
	}
	fmt.Printf("%s Nun audit gate %s for rig %s\n", mark, state, style.Bold.Render(name))
	fmt.Printf("  %s\n", style.Dim.Render("The Refinery reloads config.json each cycle; no restart required."))
	return nil
}

// setRigAuditEnabled toggles merge_queue.audit.enabled in the rig's config.json,
// preserving all other keys. The path matches the Refinery's loader
// (rig.Path/config.json).
func setRigAuditEnabled(rigPath string, enabled bool) error {
	configPath := filepath.Join(rigPath, "config.json")
	root := map[string]json.RawMessage{}
	if data, err := os.ReadFile(configPath); err == nil {
		if len(strings.TrimSpace(string(data))) > 0 {
			if err := json.Unmarshal(data, &root); err != nil {
				return fmt.Errorf("parsing %s: %w", configPath, err)
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	mq := map[string]json.RawMessage{}
	if raw, ok := root["merge_queue"]; ok && len(raw) > 0 {
		if err := json.Unmarshal(raw, &mq); err != nil {
			return fmt.Errorf("parsing merge_queue in %s: %w", configPath, err)
		}
	}
	audit := map[string]json.RawMessage{}
	if raw, ok := mq["audit"]; ok && len(raw) > 0 {
		if err := json.Unmarshal(raw, &audit); err != nil {
			return fmt.Errorf("parsing merge_queue.audit in %s: %w", configPath, err)
		}
	}

	enabledRaw, _ := json.Marshal(enabled)
	audit["enabled"] = enabledRaw
	auditRaw, _ := json.Marshal(audit)
	mq["audit"] = auditRaw
	mqRaw, _ := json.Marshal(mq)
	root["merge_queue"] = mqRaw

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, append(out, '\n'), 0o644)
}

func runAuditSolo(cmd *cobra.Command, args []string) error {
	return stampAuditTrustedField("solo", args[0])
}

func runAuditOverride(cmd *cobra.Command, args []string) error {
	return stampAuditTrustedField("override", args[0])
}

// stampAuditTrustedField writes a trusted de-escalation (solo) or override field
// onto the MR bead after verifying the caller is witness/Mayor. For override it
// also records a wisp for the audit trail.
func stampAuditTrustedField(kind, mrID string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	actor, err := requireRole("gt audit "+kind, constants.RoleWitness, constants.RoleMayor)
	if err != nil {
		return err
	}
	_, r, err := resolveRig(townRoot, "")
	if err != nil {
		return err
	}
	b := beads.New(r.Path)
	issue, err := b.Show(mrID)
	if err != nil {
		return fmt.Errorf("merge request %q not found in rig %q: %w", mrID, r.Name, err)
	}
	f := beads.ParseMRFields(issue)
	if f == nil {
		f = &beads.MRFields{}
	}
	now := time.Now().UTC().Format(time.RFC3339)

	switch kind {
	case "solo":
		f.AuditSolo = actor
		f.AuditSoloAt = now
	case "override":
		f.AuditOverride = actor
		f.AuditOverrideAt = now
	}

	desc := beads.SetMRFields(issue, f)
	if err := b.Update(mrID, beads.UpdateOptions{Description: &desc}); err != nil {
		return fmt.Errorf("stamping %s field on %s: %w", kind, mrID, err)
	}

	if kind == "override" {
		// Record the override as a wisp for the audit trail. Best-effort: the
		// trusted field is the source of truth the Refinery reads; the wisp is a
		// durable-enough breadcrumb. A failure here is non-fatal but reported.
		if _, werr := b.Create(beads.CreateOptions{
			Title:       fmt.Sprintf("Audit override: %s by %s", mrID, actor),
			Labels:      []string{"audit:override", "mr:" + mrID},
			Description: fmt.Sprintf("audit_override: %s\naudit_override_at: %s\nmr: %s", actor, now, mrID),
			Actor:       actor,
			Ephemeral:   true,
		}); werr != nil {
			fmt.Fprintf(os.Stderr, "%s could not record override wisp (trusted field still stamped): %v\n",
				style.Warning.Render("!"), werr)
		}
	}

	switch kind {
	case "solo":
		fmt.Printf("%s MR %s de-escalated to a solo Nun review by %s\n",
			style.Success.Render("✓"), style.Bold.Render(mrID), actor)
		fmt.Printf("  %s\n", style.Dim.Render("The Refinery will tally a single seat's verdict on its next cycle."))
	case "override":
		fmt.Printf("%s MR %s force-approved past the audit gate by %s\n",
			style.Success.Render("✓"), style.Bold.Render(mrID), actor)
		fmt.Printf("  %s\n", style.Dim.Render("Recorded as a trusted field + wisp; the Refinery will merge on its next cycle."))
	}
	return nil
}

// auditPanelStatus is the read-only view of one MR's audit panel.
type auditPanelStatus struct {
	MR            string `json:"mr"`
	SourceIssue   string `json:"source_issue,omitempty"`
	Branch        string `json:"branch,omitempty"`
	AuditSHA      string `json:"audit_sha,omitempty"`
	Round         int    `json:"round,omitempty"`
	Deadline      string `json:"deadline,omitempty"`
	Seats         string `json:"seats,omitempty"`
	Flavors       string `json:"flavors,omitempty"`
	Approvals     int    `json:"approvals"`
	RequestChange int    `json:"request_changes"`
	Solo          string `json:"solo,omitempty"`
	Override      string `json:"override,omitempty"`
}

func runAuditStatus(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	arg := ""
	if len(args) == 1 {
		arg = args[0]
	}

	// A single MR id (contains '-', e.g. "lgt-mr-abc") targets one panel; anything
	// else is treated as a rig name (or empty = current rig).
	var rigName string
	var singleMR string
	if arg != "" && strings.Contains(arg, "-") {
		singleMR = arg
	} else {
		rigName = arg
	}

	name, r, err := resolveRig(townRoot, rigName)
	if err != nil {
		return err
	}
	b := beads.New(r.Path)

	var mrs []*beads.Issue
	if singleMR != "" {
		issue, err := b.Show(singleMR)
		if err != nil {
			return fmt.Errorf("merge request %q not found in rig %q: %w", singleMR, name, err)
		}
		mrs = []*beads.Issue{issue}
	} else {
		mrs, err = b.List(beads.ListOptions{Label: "gt:merge-request", Status: "open", Priority: -1})
		if err != nil {
			return fmt.Errorf("listing merge requests for rig %q: %w", name, err)
		}
	}

	verdicts, _ := b.List(beads.ListOptions{Label: refinery.VerdictLabel, Status: "all", Priority: -1, Ephemeral: true})

	var panels []auditPanelStatus
	for _, mr := range mrs {
		f := beads.ParseMRFields(mr)
		if f == nil {
			continue
		}
		// In-flight = a panel is armed, or an operator field is stamped.
		inFlight := f.AuditSHA != "" || beads.HasLabel(mr, refinery.AuditPendingLabel) || f.AuditSolo != "" || f.AuditOverride != ""
		if singleMR == "" && !inFlight {
			continue
		}
		ps := auditPanelStatus{
			MR:          mr.ID,
			SourceIssue: f.SourceIssue,
			Branch:      f.Branch,
			AuditSHA:    f.AuditSHA,
			Round:       f.AuditRound,
			Deadline:    f.AuditDeadline,
			Seats:       f.AuditSeats,
			Flavors:     f.AuditFlavors,
			Solo:        f.AuditSolo,
			Override:    f.AuditOverride,
		}
		ps.Approvals, ps.RequestChange = tallyPanelVerdicts(verdicts, mr.ID, f.AuditSHA)
		panels = append(panels, ps)
	}

	if auditGateStatusJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(panels)
	}
	return printAuditStatus(name, panels)
}

// tallyPanelVerdicts counts approve/request_changes verdict wisps pinned to the
// given MR and SHA. A zero SHA (no panel armed yet) yields zero counts.
func tallyPanelVerdicts(verdicts []*beads.Issue, mrID, sha string) (approvals, requestChanges int) {
	if sha == "" {
		return 0, 0
	}
	mrLabel := "mr:" + mrID
	shaLabel := "sha:" + sha
	for _, v := range verdicts {
		if !beads.HasLabel(v, mrLabel) || !beads.HasLabel(v, shaLabel) {
			continue
		}
		switch refinery.ParseVerdict(v.Labels) {
		case refinery.VerdictApprove:
			approvals++
		case refinery.VerdictRequestChanges:
			requestChanges++
		}
	}
	return approvals, requestChanges
}

func printAuditStatus(rigName string, panels []auditPanelStatus) error {
	if len(panels) == 0 {
		fmt.Printf("%s No in-flight audit panels in rig %s\n", style.Dim.Render("○"), style.Bold.Render(rigName))
		return nil
	}
	fmt.Printf("%s\n", style.Bold.Render(fmt.Sprintf("Audit panels — rig %s", rigName)))
	for _, p := range panels {
		fmt.Printf("\n%s %s", style.Bold.Render(p.MR), style.Dim.Render(p.SourceIssue))
		if p.Override != "" {
			fmt.Printf("  %s", style.Warning.Render("OVERRIDDEN by "+p.Override))
		} else if p.Solo != "" {
			fmt.Printf("  %s", style.Warning.Render("SOLO by "+p.Solo))
		}
		fmt.Println()
		if p.AuditSHA != "" {
			fmt.Printf("  sha=%s round=%d  verdicts: %s / %s\n",
				shortAuditSHA(p.AuditSHA), p.Round,
				style.Success.Render(fmt.Sprintf("%d approve", p.Approvals)),
				style.Error.Render(fmt.Sprintf("%d request_changes", p.RequestChange)))
		} else {
			fmt.Printf("  %s\n", style.Dim.Render("panel not yet armed"))
		}
		if p.Seats != "" {
			fmt.Printf("  seats=%s flavors=%s\n", p.Seats, p.Flavors)
		}
		if p.Deadline != "" {
			fmt.Printf("  deadline=%s\n", p.Deadline)
		}
	}
	return nil
}

func shortAuditSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
