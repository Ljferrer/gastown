package polecat

import "errors"

// ErrPolecatNeedsRecovery marks an idle-looking polecat that must not be reset
// or advertised as reusable until its preserved work is recovered or submitted.
var ErrPolecatNeedsRecovery = errors.New("polecat needs recovery before reuse")

// SlotReuseInput is the shared input for deciding whether a polecat slot can be
// advertised as open and destructively reused for new work.
type SlotReuseInput struct {
	State            State
	HookBead         string
	CleanupStatus    CleanupStatus
	PushFailed       bool
	MRFailed         bool
	Branch           string
	GitDirty         bool
	StashCount       int
	UnpushedCommits  int
	GitCheckFailed   bool
	ActiveMRPending  bool
	ActiveMRReason   string
	StaleCleanupSafe bool
}

// SlotReuseDecision explains whether a polecat can be reused and why not.
type SlotReuseDecision struct {
	Reusable bool
	Reason   string
}

// DecideSlotReuse is the single source of truth for reuse safety. It fails
// closed: unknown cleanup/git state means the slot needs recovery, not reuse.
func DecideSlotReuse(in SlotReuseInput) SlotReuseDecision {
	if in.State != StateIdle {
		return SlotReuseDecision{Reason: "not-idle"}
	}
	if in.HookBead != "" {
		return SlotReuseDecision{Reason: "hook-still-set"}
	}
	if in.PushFailed {
		return SlotReuseDecision{Reason: "push-failed"}
	}
	if in.MRFailed {
		return SlotReuseDecision{Reason: "mr-failed"}
	}
	if in.ActiveMRPending {
		if in.ActiveMRReason != "" {
			return SlotReuseDecision{Reason: in.ActiveMRReason}
		}
		return SlotReuseDecision{Reason: "active-mr-pending"}
	}
	if !in.CleanupStatus.IsSafe() {
		if in.StaleCleanupSafe {
			// Continue: direct git/source/MR checks proved cleanup_status stale.
		} else {
			if in.CleanupStatus == "" {
				return SlotReuseDecision{Reason: "cleanup-unknown"}
			}
			return SlotReuseDecision{Reason: "cleanup-" + string(in.CleanupStatus)}
		}
	}
	if in.GitCheckFailed {
		return SlotReuseDecision{Reason: "git-check-failed"}
	}
	if in.GitDirty {
		return SlotReuseDecision{Reason: "git-dirty"}
	}
	if in.StashCount > 0 {
		return SlotReuseDecision{Reason: "git-stash"}
	}
	if in.UnpushedCommits > 0 {
		return SlotReuseDecision{Reason: "git-unpushed"}
	}
	return SlotReuseDecision{Reusable: true, Reason: "reusable"}
}
