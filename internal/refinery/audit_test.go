package refinery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/rig"
)

// --- AuditConfig defaults & opt-in ---

func TestDefaultAuditConfig_DisabledByDefault(t *testing.T) {
	c := DefaultAuditConfig()
	if c.Enabled {
		t.Fatal("audit must be disabled by default (no behavior change until opt-in)")
	}
	if c.Formula != "mol-nun-audit" {
		t.Errorf("formula default = %q, want mol-nun-audit", c.Formula)
	}
	if c.Model != "opus" {
		t.Errorf("model default = %q, want opus", c.Model)
	}
	if c.PanelSize != 1 {
		t.Errorf("panel_size default = %d, want 1", c.PanelSize)
	}
	if c.MaxSeats != 6 {
		t.Errorf("max_seats default = %d, want 6", c.MaxSeats)
	}
	if c.RoundLimit != 3 {
		t.Errorf("round_limit default = %d, want 3", c.RoundLimit)
	}
	if c.WallClockMin != 60 {
		t.Errorf("wall_clock_min default = %d, want 60", c.WallClockMin)
	}
	if c.Verdict != "wisp" {
		t.Errorf("verdict default = %q, want wisp", c.Verdict)
	}
}

func TestAuditConfigRaw_Apply_MergesOverDefaults(t *testing.T) {
	cfg := DefaultAuditConfig()
	enabled := true
	panel := 3
	verdict := "mail"
	raw := &auditConfigRaw{Enabled: &enabled, PanelSize: &panel, Verdict: &verdict}
	raw.apply(cfg)

	if !cfg.Enabled {
		t.Error("Enabled should be overridden to true")
	}
	if cfg.PanelSize != 3 {
		t.Errorf("PanelSize = %d, want 3", cfg.PanelSize)
	}
	if cfg.Verdict != "mail" {
		t.Errorf("Verdict = %q, want mail", cfg.Verdict)
	}
	// Untouched fields keep defaults.
	if cfg.Model != "opus" {
		t.Errorf("Model = %q, want default opus", cfg.Model)
	}
	if cfg.MaxSeats != 6 {
		t.Errorf("MaxSeats = %d, want default 6", cfg.MaxSeats)
	}
}

func TestAuditConfigRaw_Apply_Nil(t *testing.T) {
	cfg := DefaultAuditConfig()
	var raw *auditConfigRaw
	raw.apply(cfg) // must not panic
	if cfg.Enabled {
		t.Error("nil raw must not change config")
	}
}

func TestLoadConfig_AuditDefaultsDisabled(t *testing.T) {
	tmpDir := t.TempDir()
	config := map[string]interface{}{
		"type": "rig", "version": 1, "name": "test-rig",
		"merge_queue": map[string]interface{}{"enabled": true},
	}
	data, _ := json.MarshalIndent(config, "", "  ")
	if err := os.WriteFile(filepath.Join(tmpDir, "config.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	e := NewEngineer(&rig.Rig{Name: "test-rig", Path: tmpDir})
	if err := e.LoadConfig(); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if e.config.Audit == nil {
		t.Fatal("Audit config should default to non-nil")
	}
	if e.config.Audit.Enabled {
		t.Error("audit must stay disabled when no audit block in config")
	}
	if e.auditEnabled() {
		t.Error("auditEnabled() should be false by default")
	}
}

func TestLoadConfig_AuditOptIn(t *testing.T) {
	tmpDir := t.TempDir()
	config := map[string]interface{}{
		"type": "rig", "version": 1, "name": "test-rig",
		"merge_queue": map[string]interface{}{
			"enabled": true,
			"audit": map[string]interface{}{
				"enabled":    true,
				"panel_size": 2,
				"max_seats":  4,
				"verdict":    "mail",
			},
		},
	}
	data, _ := json.MarshalIndent(config, "", "  ")
	if err := os.WriteFile(filepath.Join(tmpDir, "config.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	e := NewEngineer(&rig.Rig{Name: "test-rig", Path: tmpDir})
	if err := e.LoadConfig(); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !e.auditEnabled() {
		t.Fatal("audit should be enabled after opt-in")
	}
	if e.config.Audit.PanelSize != 2 {
		t.Errorf("PanelSize = %d, want 2", e.config.Audit.PanelSize)
	}
	if e.config.Audit.MaxSeats != 4 {
		t.Errorf("MaxSeats = %d, want 4", e.config.Audit.MaxSeats)
	}
	if e.config.Audit.Verdict != "mail" {
		t.Errorf("Verdict = %q, want mail", e.config.Audit.Verdict)
	}
	// Unspecified audit fields keep defaults.
	if e.config.Audit.Formula != "mol-nun-audit" {
		t.Errorf("Formula = %q, want default mol-nun-audit", e.config.Audit.Formula)
	}
	if e.config.Audit.RoundLimit != 3 {
		t.Errorf("RoundLimit = %d, want default 3", e.config.Audit.RoundLimit)
	}
}

func TestDefaultAuditConfig_CovenSize(t *testing.T) {
	if c := DefaultAuditConfig(); c.CovenSize != 3 {
		t.Errorf("coven_size default = %d, want 3", c.CovenSize)
	}
}

func TestAuditConfigRaw_Apply_CovenSize(t *testing.T) {
	cfg := DefaultAuditConfig()
	coven := 5
	raw := &auditConfigRaw{CovenSize: &coven}
	raw.apply(cfg)
	if cfg.CovenSize != 5 {
		t.Errorf("CovenSize = %d, want 5", cfg.CovenSize)
	}
	if cfg.PanelSize != 1 {
		t.Errorf("PanelSize = %d, want default 1 (untouched)", cfg.PanelSize)
	}
}

// --- panelParams: coven label scales seats + deepens ---

func enginePanel(panel, coven int) *Engineer {
	e := &Engineer{}
	e.config = &MergeQueueConfig{Audit: &AuditConfig{
		Enabled: true, PanelSize: panel, CovenSize: coven,
	}}
	return e
}

func TestPanelParams_DefaultIsPanelNeighbors(t *testing.T) {
	size, depth := enginePanel(1, 3).panelParams([]string{"gt:merge-request"})
	if size != 1 || depth != auditDepthNeighbors {
		t.Errorf("default panel = (%d,%q), want (1,neighbors)", size, depth)
	}
}

func TestPanelParams_CovenLabelScalesAndDeepens(t *testing.T) {
	size, depth := enginePanel(1, 3).panelParams([]string{"gt:merge-request", auditLabelCoven})
	if size != 3 || depth != auditDepthDeep {
		t.Errorf("coven panel = (%d,%q), want (3,deep)", size, depth)
	}
}

// --- Flavor assignment: distinct lenses, stable by position ---

func TestAssignFlavors_SingleSeatIsGeneral(t *testing.T) {
	got := assignFlavors(1)
	if len(got) != 1 || got[0] != flavorGeneral {
		t.Errorf("assignFlavors(1) = %v, want [general]", got)
	}
}

func TestAssignFlavors_CovenIsDistinct(t *testing.T) {
	got := assignFlavors(3)
	want := []string{"correctness", "security", "plan-faithfulness"}
	if len(got) != 3 {
		t.Fatalf("assignFlavors(3) = %v, want 3 lenses", got)
	}
	seen := map[string]bool{}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("flavor[%d] = %q, want %q", i, got[i], want[i])
		}
		if seen[got[i]] {
			t.Errorf("flavor %q repeated — coven must search divergent angles", got[i])
		}
		seen[got[i]] = true
	}
}

func TestAssignFlavors_StableByPosition(t *testing.T) {
	// Each Nun keeps her flavor across rounds: seat i always maps to the same lens.
	a, b := assignFlavors(3), assignFlavors(3)
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("flavor for seat %d not stable: %q vs %q", i, a[i], b[i])
		}
	}
}

func TestAssignFlavors_OverflowCyclesRoster(t *testing.T) {
	got := assignFlavors(4)
	if got[3] != NunFlavors[0] {
		t.Errorf("flavor[3] = %q, want roster wrap %q", got[3], NunFlavors[0])
	}
}

// --- Verdict labels ---

func TestVerdictLabels_RoundTrip(t *testing.T) {
	labels := VerdictLabels("lgt-mr1", "abc123", "Mary", 2, VerdictApprove)
	want := []string{"nun-verdict", "mr:lgt-mr1", "sha:abc123", "seat:Mary", "round:2", "verdict:approve"}
	if len(labels) != len(want) {
		t.Fatalf("labels = %v, want %v", labels, want)
	}
	for i := range want {
		if labels[i] != want[i] {
			t.Errorf("label[%d] = %q, want %q", i, labels[i], want[i])
		}
	}
	if got := ParseVerdict(labels); got != VerdictApprove {
		t.Errorf("ParseVerdict = %q, want approve", got)
	}
}

func TestParseVerdict_RequestChanges(t *testing.T) {
	if got := ParseVerdict([]string{"nun-verdict", "verdict:request_changes"}); got != VerdictRequestChanges {
		t.Errorf("ParseVerdict = %q, want request_changes", got)
	}
	if got := ParseVerdict([]string{"nun-verdict", "mr:x"}); got != "" {
		t.Errorf("ParseVerdict with no verdict label = %q, want empty", got)
	}
}

// --- Nun roster leasing ---

func TestLeaseNun_FirstFree(t *testing.T) {
	name, ok := LeaseNun(map[string]bool{})
	if !ok {
		t.Fatal("expected a free name from an empty roster")
	}
	if name != NunRoster[0] {
		t.Errorf("first lease = %q, want %q", name, NunRoster[0])
	}
}

func TestLeaseNun_SkipsInUse(t *testing.T) {
	inUse := map[string]bool{NunRoster[0]: true, NunRoster[1]: true}
	name, ok := LeaseNun(inUse)
	if !ok || name != NunRoster[2] {
		t.Errorf("lease = %q (ok=%v), want %q", name, ok, NunRoster[2])
	}
}

func TestLeaseNun_Exhausted(t *testing.T) {
	inUse := map[string]bool{}
	for _, n := range NunRoster {
		inUse[n] = true
	}
	if _, ok := LeaseNun(inUse); ok {
		t.Error("expected exhaustion when whole roster is in use")
	}
}

func TestSeats_RoundTrip(t *testing.T) {
	if got := parseSeats("Mary, Teresa ,Agnes"); len(got) != 3 || got[0] != "Mary" || got[2] != "Agnes" {
		t.Errorf("parseSeats = %v", got)
	}
	if got := parseSeats("  "); got != nil {
		t.Errorf("parseSeats(blank) = %v, want nil", got)
	}
	if got := formatSeats([]string{"Mary", "Teresa"}); got != "Mary,Teresa" {
		t.Errorf("formatSeats = %q, want Mary,Teresa", got)
	}
}

// --- Verdict tally (pure) ---

func verdictIssue(id string, labels ...string) *beads.Issue {
	return &beads.Issue{ID: id, Labels: labels}
}

func TestTallyVerdicts_ApprovesAtHead(t *testing.T) {
	verdicts := []*beads.Issue{
		verdictIssue("v1", VerdictLabels("mr1", "head", "Mary", 1, VerdictApprove)...),
	}
	if !tallyVerdicts(verdicts, "mr1", "head", 1) {
		t.Error("single approve at head should satisfy panel_size 1")
	}
}

func TestTallyVerdicts_RequestChangesFailsClosed(t *testing.T) {
	verdicts := []*beads.Issue{
		verdictIssue("v1", VerdictLabels("mr1", "head", "Mary", 1, VerdictApprove)...),
		verdictIssue("v2", VerdictLabels("mr1", "head", "Teresa", 1, VerdictRequestChanges)...),
	}
	if tallyVerdicts(verdicts, "mr1", "head", 2) {
		t.Error("any request_changes must fail the tally (fail-closed)")
	}
}

func TestTallyVerdicts_StaleSHAIgnored(t *testing.T) {
	// An approval pinned to an old SHA must not count toward the current HEAD.
	verdicts := []*beads.Issue{
		verdictIssue("v1", VerdictLabels("mr1", "oldsha", "Mary", 1, VerdictApprove)...),
	}
	if tallyVerdicts(verdicts, "mr1", "newsha", 1) {
		t.Error("stale-SHA approval must not satisfy current HEAD")
	}
}

func TestTallyVerdicts_OtherMRIgnored(t *testing.T) {
	verdicts := []*beads.Issue{
		verdictIssue("v1", VerdictLabels("other", "head", "Mary", 1, VerdictApprove)...),
	}
	if tallyVerdicts(verdicts, "mr1", "head", 1) {
		t.Error("verdict for a different MR must not count")
	}
}

func TestTallyVerdicts_NeedsPanelSize(t *testing.T) {
	verdicts := []*beads.Issue{
		verdictIssue("v1", VerdictLabels("mr1", "head", "Mary", 1, VerdictApprove)...),
	}
	if tallyVerdicts(verdicts, "mr1", "head", 2) {
		t.Error("one approve must not satisfy panel_size 2")
	}
}
