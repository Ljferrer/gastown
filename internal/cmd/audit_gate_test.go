package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/refinery"
)

func TestRoleFromAddress(t *testing.T) {
	cases := []struct {
		addr string
		want string
	}{
		{"mayor/", constants.RoleMayor},
		{"mayor", constants.RoleMayor},
		{"LokustGasTown/witness", constants.RoleWitness},
		{"LokustGasTown/refinery", constants.RoleRefinery},
		{"LokustGasTown/polecats/obsidian", constants.RolePolecat},
		{"LokustGasTown/crew/joe", constants.RoleCrew},
		{"deacon/", constants.RoleDeacon},
		{"deacon/dogs/rex", ""}, // a dog is not the deacon
		{"overseer", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := roleFromAddress(c.addr); got != c.want {
			t.Errorf("roleFromAddress(%q) = %q, want %q", c.addr, got, c.want)
		}
	}
}

// setRigAuditEnabled must flip merge_queue.audit.enabled while preserving every
// other key in config.json (the Refinery reads this exact path).
func TestSetRigAuditEnabled_PreservesOtherKeys(t *testing.T) {
	dir := t.TempDir()
	seed := map[string]any{
		"name": "test-rig",
		"merge_queue": map[string]any{
			"enabled":       true,
			"poll_interval": "30s",
			"audit": map[string]any{
				"panel_size": 1,
			},
		},
	}
	data, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := setRigAuditEnabled(dir, true); err != nil {
		t.Fatalf("setRigAuditEnabled: %v", err)
	}

	got := map[string]any{}
	out, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if got["name"] != "test-rig" {
		t.Errorf("top-level key lost: name=%v", got["name"])
	}
	mq, _ := got["merge_queue"].(map[string]any)
	if mq == nil {
		t.Fatal("merge_queue lost")
	}
	if mq["enabled"] != true || mq["poll_interval"] != "30s" {
		t.Errorf("merge_queue siblings lost: %v", mq)
	}
	audit, _ := mq["audit"].(map[string]any)
	if audit == nil || audit["enabled"] != true {
		t.Errorf("audit.enabled not set: %v", audit)
	}
	if audit["panel_size"] != float64(1) {
		t.Errorf("audit.panel_size lost: %v", audit["panel_size"])
	}
}

// setRigAuditEnabled must create the nested structure when config.json is absent.
func TestSetRigAuditEnabled_CreatesWhenMissing(t *testing.T) {
	dir := t.TempDir()
	if err := setRigAuditEnabled(dir, false); err != nil {
		t.Fatalf("setRigAuditEnabled: %v", err)
	}
	got := map[string]any{}
	out, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	mq, _ := got["merge_queue"].(map[string]any)
	audit, _ := mq["audit"].(map[string]any)
	if audit == nil || audit["enabled"] != false {
		t.Errorf("audit.enabled not written false: %v", audit)
	}
}

func TestTallyPanelVerdicts(t *testing.T) {
	mk := func(id string, labels ...string) *beads.Issue { return &beads.Issue{ID: id, Labels: labels} }
	verdicts := []*beads.Issue{
		mk("v1", refinery.VerdictLabels("mr1", "head", "Mary", 1, refinery.VerdictApprove)...),
		mk("v2", refinery.VerdictLabels("mr1", "head", "Teresa", 1, refinery.VerdictRequestChanges)...),
		mk("v3", refinery.VerdictLabels("mr2", "head", "Agnes", 1, refinery.VerdictApprove)...),  // other MR
		mk("v4", refinery.VerdictLabels("mr1", "stale", "Lucia", 1, refinery.VerdictApprove)...), // other SHA
	}
	approvals, requestChanges := tallyPanelVerdicts(verdicts, "mr1", "head")
	if approvals != 1 || requestChanges != 1 {
		t.Errorf("tallyPanelVerdicts = %d approve / %d request_changes, want 1/1", approvals, requestChanges)
	}
	// No SHA armed yet → zero counts.
	if a, r := tallyPanelVerdicts(verdicts, "mr1", ""); a != 0 || r != 0 {
		t.Errorf("empty SHA tally = %d/%d, want 0/0", a, r)
	}
}
