package beads

import (
	"strings"
	"testing"
)

// Round-trip of the trusted operator audit fields (audit_solo / audit_override)
// through Format → Parse, plus SetMRFields preserving prose. These fields carry
// actor provenance and must survive a description rewrite intact.

func TestMRFields_AuditTrustedFieldsRoundTrip(t *testing.T) {
	in := &MRFields{
		Branch:          "polecat/Nux/lgt-xyz",
		AuditSolo:       "LokustGasTown/witness",
		AuditSoloAt:     "2026-06-14T16:00:00Z",
		AuditOverride:   "mayor/",
		AuditOverrideAt: "2026-06-14T16:05:00Z",
	}
	desc := FormatMRFields(in)
	out := ParseMRFields(&Issue{Description: desc})
	if out == nil {
		t.Fatal("ParseMRFields returned nil")
	}
	if out.AuditSolo != in.AuditSolo || out.AuditSoloAt != in.AuditSoloAt {
		t.Errorf("solo round-trip: got %q/%q want %q/%q", out.AuditSolo, out.AuditSoloAt, in.AuditSolo, in.AuditSoloAt)
	}
	if out.AuditOverride != in.AuditOverride || out.AuditOverrideAt != in.AuditOverrideAt {
		t.Errorf("override round-trip: got %q/%q want %q/%q", out.AuditOverride, out.AuditOverrideAt, in.AuditOverride, in.AuditOverrideAt)
	}
}

func TestMRFields_AuditTrustedFieldsOmittedWhenEmpty(t *testing.T) {
	desc := FormatMRFields(&MRFields{Branch: "b"})
	out := ParseMRFields(&Issue{Description: desc})
	if out == nil {
		t.Fatal("ParseMRFields returned nil")
	}
	if out.AuditSolo != "" || out.AuditOverride != "" {
		t.Errorf("expected empty trusted fields, got solo=%q override=%q", out.AuditSolo, out.AuditOverride)
	}
}

// SetMRFields must stamp a trusted field onto an existing MR description while
// preserving the prose body and the other MR fields already present.
func TestSetMRFields_StampSoloPreservesProse(t *testing.T) {
	orig := &Issue{Description: "branch: b\ntarget: main\n\nSome human prose about the MR."}
	f := ParseMRFields(orig)
	if f == nil {
		t.Fatal("ParseMRFields returned nil on seed")
	}
	f.AuditSolo = "LokustGasTown/witness"
	f.AuditSoloAt = "2026-06-14T16:00:00Z"
	newDesc := SetMRFields(orig, f)

	reparsed := ParseMRFields(&Issue{Description: newDesc})
	if reparsed.AuditSolo != "LokustGasTown/witness" {
		t.Errorf("AuditSolo = %q, want LokustGasTown/witness", reparsed.AuditSolo)
	}
	if reparsed.Branch != "b" || reparsed.Target != "main" {
		t.Errorf("existing fields lost: branch=%q target=%q", reparsed.Branch, reparsed.Target)
	}
	if !strings.Contains(newDesc, "Some human prose about the MR.") {
		t.Errorf("prose not preserved in %q", newDesc)
	}
}
