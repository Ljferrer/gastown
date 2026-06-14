package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/rig"
)

// TestNewLiveEngineer_WiresConfigAndSpawner guards the lgt-i2h fix: the live
// merge path must both load the rig's merge_queue config (so an opt-in
// audit.enabled actually takes effect instead of the disabled default) AND
// install the concrete Nun seat spawner (so the audit gate can launch Nuns
// rather than parking every MR on a stamped-but-unspawned panel). Before the
// fix, runRefineryReady built a bare NewEngineer that never called LoadConfig,
// so audit.enabled=true was silently ignored and merges fell open.
func TestNewLiveEngineer_WiresConfigAndSpawner(t *testing.T) {
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

	eng := newLiveEngineer(&rig.Rig{Name: "test-rig", Path: tmpDir})

	if cfg := eng.Config(); cfg.Audit == nil || !cfg.Audit.Enabled {
		t.Fatal("newLiveEngineer must LoadConfig so opt-in audit.enabled takes effect")
	}
	if !eng.HasSeatSpawner() {
		t.Fatal("newLiveEngineer must install a seat spawner so the audit gate can launch Nuns")
	}
}

// TestNewLiveEngineer_MalformedConfigFallsBack ensures a broken config.json
// cannot wedge the merge queue: LoadConfig errors are warned, not fatal, and the
// engineer still comes back wired with a spawner and usable defaults.
func TestNewLiveEngineer_MalformedConfigFallsBack(t *testing.T) {
	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, "config.json"), []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}

	eng := newLiveEngineer(&rig.Rig{Name: "test-rig", Path: tmpDir})

	if eng == nil {
		t.Fatal("newLiveEngineer must return a usable engineer even when config.json is malformed")
	}
	if !eng.HasSeatSpawner() {
		t.Fatal("seat spawner must still be installed when config load fails")
	}
	if cfg := eng.Config(); cfg.Audit == nil || cfg.Audit.Enabled {
		t.Fatal("malformed config must fall back to audit-disabled defaults")
	}
}

func TestRefineryStartAgentFlag(t *testing.T) {
	flag := refineryStartCmd.Flags().Lookup("agent")
	if flag == nil {
		t.Fatal("expected refinery start to define --agent flag")
	}
	if flag.DefValue != "" {
		t.Errorf("expected default agent override to be empty, got %q", flag.DefValue)
	}
	if !strings.Contains(flag.Usage, "overrides town default") {
		t.Errorf("expected --agent usage to mention overrides town default, got %q", flag.Usage)
	}
}

func TestRefineryAttachAgentFlag(t *testing.T) {
	flag := refineryAttachCmd.Flags().Lookup("agent")
	if flag == nil {
		t.Fatal("expected refinery attach to define --agent flag")
	}
	if flag.DefValue != "" {
		t.Errorf("expected default agent override to be empty, got %q", flag.DefValue)
	}
	if !strings.Contains(flag.Usage, "overrides town default") {
		t.Errorf("expected --agent usage to mention overrides town default, got %q", flag.Usage)
	}
}

func TestRefineryRestartAgentFlag(t *testing.T) {
	flag := refineryRestartCmd.Flags().Lookup("agent")
	if flag == nil {
		t.Fatal("expected refinery restart to define --agent flag")
	}
	if flag.DefValue != "" {
		t.Errorf("expected default agent override to be empty, got %q", flag.DefValue)
	}
	if !strings.Contains(flag.Usage, "overrides town default") {
		t.Errorf("expected --agent usage to mention overrides town default, got %q", flag.Usage)
	}
}

func TestRefineryStartForegroundFlagHidden(t *testing.T) {
	flag := refineryStartCmd.Flags().Lookup("foreground")
	if flag == nil {
		t.Fatal("expected hidden compatibility --foreground flag")
	}
	if !flag.Hidden {
		t.Fatal("expected --foreground to be hidden")
	}
	if strings.Contains(refineryStartCmd.Long, "--foreground") {
		t.Fatalf("refinery start help should not advertise --foreground:\n%s", refineryStartCmd.Long)
	}
}
