package seatspawn

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/refinery"
	"github.com/steveyegge/gastown/internal/rig"
)

func sampleReq() refinery.SeatSpawnRequest {
	return refinery.SeatSpawnRequest{
		MRID:        "lgt-mr1",
		SourceIssue: "lgt-21r",
		Branch:      "polecat/obsidian/lgt-21r",
		Target:      "origin/main",
		AuditSHA:    "abc123",
		SeatName:    "Mary",
		Round:       2,
		Formula:     "mol-nun-audit",
		Model:       "opus",
		Verdict:     "wisp",
		Depth:       "neighbors",
		Flavor:      "correctness",
	}
}

func TestAuditPrompt_CarriesContractVars(t *testing.T) {
	got := auditPrompt(sampleReq())
	for _, want := range []string{
		"mr_id        = lgt-mr1",
		"source_issue = lgt-21r",
		"branch       = polecat/obsidian/lgt-21r",
		"target       = origin/main",
		"audit_sha    = abc123",
		"round        = 2",
		"depth        = neighbors",
		"flavor       = correctness",
		"git diff origin/main...abc123",
		"git show abc123:<path>",
		"bd show lgt-21r",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q\n---\n%s", want, got)
		}
	}
}

func TestAuditPrompt_VerdictLabelsMatchGateTally(t *testing.T) {
	req := sampleReq()
	got := auditPrompt(req)

	// The verdict instruction MUST carry the exact label sets the Engineer tallies
	// (refinery.VerdictLabels). Drift here silently breaks the gate.
	approve := strings.Join(refinery.VerdictLabels(req.MRID, req.AuditSHA, req.SeatName, req.Round, refinery.VerdictApprove), ",")
	changes := strings.Join(refinery.VerdictLabels(req.MRID, req.AuditSHA, req.SeatName, req.Round, refinery.VerdictRequestChanges), ",")
	if !strings.Contains(got, approve) {
		t.Errorf("prompt missing approve labels %q", approve)
	}
	if !strings.Contains(got, changes) {
		t.Errorf("prompt missing request_changes labels %q", changes)
	}
}

func TestAuditPrompt_WispUsesEphemeral(t *testing.T) {
	got := auditPrompt(sampleReq())
	if !strings.Contains(got, "bd create --ephemeral") {
		t.Errorf("wisp transport must create an ephemeral bead\n%s", got)
	}
}

func TestAuditPrompt_MailUsesDurable(t *testing.T) {
	req := sampleReq()
	req.Verdict = "mail"
	got := auditPrompt(req)
	if strings.Contains(got, "--ephemeral") {
		t.Errorf("mail transport must NOT be ephemeral\n%s", got)
	}
	if !strings.Contains(got, "bd create -t") {
		t.Errorf("mail transport should still create a durable verdict bead\n%s", got)
	}
}

func TestAuditPrompt_DeepDepthGuidance(t *testing.T) {
	req := sampleReq()
	req.Depth = "deep"
	got := auditPrompt(req)
	if !strings.Contains(got, "deep depth") {
		t.Errorf("deep depth guidance missing\n%s", got)
	}
	if strings.Contains(got, "neighbors depth: review") {
		t.Errorf("deep request should not show neighbors guidance\n%s", got)
	}
}

func TestAppendModelFlag(t *testing.T) {
	tests := []struct {
		name, cmd, model, want string
	}{
		{"appends when absent", "claude --settings x", "opus", "claude --settings x --model opus"},
		{"empty model no-op", "claude --settings x", "", "claude --settings x"},
		{"existing model preserved", "claude --model sonnet", "opus", "claude --model sonnet"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := appendModelFlag(tc.cmd, tc.model); got != tc.want {
				t.Errorf("appendModelFlag(%q, %q) = %q, want %q", tc.cmd, tc.model, got, tc.want)
			}
		})
	}
}

func TestSeatPaths_DisjointFromPolecats(t *testing.T) {
	s := &Spawner{rig: &rig.Rig{Name: "lgt", Path: "/town/lgt"}}

	home := s.seatHome("Mary")
	if want := "/town/lgt/seats/Mary"; home != want {
		t.Errorf("seatHome = %q, want %q", home, want)
	}
	if strings.Contains(home, "/polecats/") {
		t.Errorf("seat home must not live under polecats/: %q", home)
	}
	if want := "/town/lgt/seats/Mary/lgt"; s.seatWorktree("Mary") != want {
		t.Errorf("seatWorktree = %q, want %q", s.seatWorktree("Mary"), want)
	}
	if want := "/town/lgt/seats/Mary/.gt-seat/settings.json"; s.settingsPath("Mary") != want {
		t.Errorf("settingsPath = %q, want %q", s.settingsPath("Mary"), want)
	}
}

func TestSessionName_SeatSegmentAndLowercase(t *testing.T) {
	s := &Spawner{rig: &rig.Rig{Name: "lgt", Path: "/town/lgt"}}
	got := s.sessionName("Mary")
	if !strings.Contains(got, "-seat-") {
		t.Errorf("session name must carry a seat segment to avoid polecat collision: %q", got)
	}
	if !strings.HasSuffix(got, "mary") {
		t.Errorf("session name should lowercase the seat name: %q", got)
	}
}

func TestSeatEnv_SeatScopedIdentity(t *testing.T) {
	s := &Spawner{rig: &rig.Rig{Name: "lgt", Path: "/town/lgt"}, townRoot: "/town"}
	env := s.seatEnv("Mary", "/town/lgt/seats/Mary/lgt")

	if want := "lgt/seats/Mary"; env["BD_ACTOR"] != want {
		t.Errorf("BD_ACTOR = %q, want %q (verdict attribution must be seat-scoped)", env["BD_ACTOR"], want)
	}
	if env["GT_ROLE"] != "lgt/seats/Mary" {
		t.Errorf("GT_ROLE = %q, want lgt/seats/Mary", env["GT_ROLE"])
	}
	if env["GT_AGENT"] != auditAgent {
		t.Errorf("GT_AGENT = %q, want %q", env["GT_AGENT"], auditAgent)
	}
	if env["GT_PROCESS_NAMES"] == "" {
		t.Error("GT_PROCESS_NAMES must be set for liveness detection")
	}
}

func TestTeardownSeat_EmptyNameNoop(t *testing.T) {
	s := &Spawner{rig: &rig.Rig{Name: "lgt", Path: "/town/lgt"}}
	if err := s.TeardownSeat(""); err != nil {
		t.Errorf("teardown of empty name should be a no-op, got %v", err)
	}
}
