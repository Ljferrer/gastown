# Refinery Nun Audit Gate

> Status: Design / plan (ready to decompose into beads)
> Tracking issue: [gastownhall/gastown#4168](https://github.com/gastownhall/gastown/issues/4168)
> Author: Overseer + crew/Zeno (grill session 2026-06-13)

## Summary

Add an opt-in, **fail-closed independent audit gate** to the Refinery. After a polecat
submits work and the Refinery confirms it against quality gates/tests, but **before** the
merge lands on the target branch, a panel of **Nuns** — fresh-context auditor polecats —
independently review the change. The merge proceeds only when **every** Nun **unanimously
approves** the exact SHA being merged. Review judgment lives in an agent formula; Go only
spawns seats, tallies verdicts, and gates. ZFC-clean: **Go counts, agents decide.**

Nuns are ordinary polecats running an audit formula in enforced read-only mode — no new
agent/role type. They draw names from a dedicated roster so they can never collide with
worker polecats.

The gate is **off by default**. No behavior change until a Mayor opts a rig in.

## Why

The Refinery today can block on command/test gates or a single PR approval, but neither
supports *"don't merge until N independent reviewers approve."* The single-approver path
also requires a real GitHub PR + branch protection, so it doesn't work for direct-merge
rigs. Operators fake panels with copy-paste town-formula shell logic that leaks
polecat-cap slots and is invisible to `doMerge`.

The independent-audit step reliably catches bugs that tests miss — the originating use case
for this feature (the Overseer has run it by hand for months with consistent results). This
makes it a first-class, configurable Refinery capability.

## Core model

- **Independent**: each Nun is a fresh polecat with a clean context window — no shared
  history with the implementer or the other Nuns. Independence is the whole value.
- **Unanimous, fail-closed**: all N seats must return `approve` against the current SHA.
  Any single `request_changes`, missing verdict, or hung seat means **no merge**.
- **Convergent unanimity on a single final SHA**: an approval is provisional — it is bound
  to a specific branch SHA. When HEAD moves (a fix lands), every seat — including prior
  approvers — must re-confirm against the new SHA. The merged commit is one that **all**
  Nuns approved simultaneously.
- **Dependency-blind**: each MR is audited in isolation. Cross-bead ordering is a
  Mayor/Crew concern upstream; the gate never reasons about unmerged dependencies.
- **Asynchronous**: auditing runs off the serial-merge critical path (see Architecture).
  Only the final merge is serial.

## What a Nun reviews

1. **Impact-aware code/diff audit** — the diff against the **single target branch this merge
   will land on** (`<target>...<branch>`, one comparison only), plus *everything the diff
   touches*. Depth is tiered (see below), not literally the whole repo.
2. **Plan faithfulness** — if a plan exists, check the slice against the relevant part of
   the plan. Plans are committed, easily findable files; one plan file typically maps to
   **many** beads/branches, so a Nun checks this slice against the relevant portion, not 1:1.
3. **Degrade gracefully** — if no plan file is discoverable, audit code/diff only.

**Plan discovery (agent judgment, inside the formula):** read the `source_issue` bead and
follow any linked plan beads / committed file paths it names; else look for plan-shaped docs
the branch adds/modifies; else code-only. No formal `plan_file` schema is introduced — Go
stays ZFC-clean and discovery is the agent's job.

## Tiered audit depth + perspective diversity

Depth scales with the scrutiny the operator already signaled via labels:

- **Default 1-Nun pass** (`depth = neighbors`): the diff plus the definitions/callers the
  changed lines directly reference (one hop), under a per-round time budget.
- **`audit:coven`** (`depth = deep`): full agent-judgment impact tracing — follow the
  impact wherever it leads.

**Perspective diversity (N > 1):** the Refinery spins each Nun up with a slightly different
"flavor"/lens (e.g. correctness, security, plan-faithfulness, …) generated on the fly, so a
coven searches from divergent angles rather than producing N identical reads. Each Nun keeps
her flavor across rounds (persistent context).

## Architecture: asynchronous audit phase

`doMerge` (`internal/refinery/engineer.go`) is synchronous and processes one MR at a time
(`MaxConcurrent: 1`, sequential rebasing). Blocking it on a panel that can run up to an hour
would stall the entire rig's merge queue per MR. Instead:

- The audit is a **separate phase evaluated before an MR is eligible to enter `doMerge`**.
  When an MR is first dequeued and passes pre-merge gates, the Refinery spawns the panel,
  stamps the MR `audit-pending`, and **moves on to the next MR**. Panels for many MRs run
  concurrently.
- On each patrol cycle the Refinery reconciles verdicts. Only MRs whose panels have reached
  unanimous approval at the current HEAD become eligible for `doMerge`. `doMerge` itself
  stays dumb and synchronous; it only ever sees pre-approved MRs.
- **The pre-verified fast-path does NOT skip the audit.** `doMerge`'s `skipGates`
  optimization (the polecat self-certified after rebasing) is exactly the self-certification
  the Nun exists to distrust. When audit is enabled, every qualifying merge gets a panel
  regardless of pre-verification.
- **Scope:** the gate fires on **whatever merge the Refinery processes** when enabled, each
  audit scoped to that merge's own single target branch. (Not main-only.)

### Panel state (persisted on the MR bead — survives Refinery restart)

- `audit_sha` — branch HEAD at spawn time (the SHA under review this round)
- `audit_deadline` — wall-clock deadline (default now + 60 min)
- `audit_seats` — leased seat names + assigned flavors
- `audit_round` — 1-based round counter (panel-wide)
- de-escalation override field (see `audit:solo`)

On restart the Refinery re-reads verdicts from beads rather than respawning — the bead is the
source of truth, not in-memory state.

## The fix loop (refinery-mediated, batched per round)

A **round** = one branch SHA evaluated by the whole panel.

1. Refinery spawns N flavored seats against `audit_sha`; each reviews.
2. **Persistent-context re-audit:** a Nun who returns `request_changes` is **not** torn down.
   She holds her context window and will re-review the next SHA against her own prior reading.
   Approvers also stay resident (convergent unanimity requires them to re-confirm new SHAs).
3. At round end, the Refinery collects **every** dissenting Nun's findings and sends **one**
   aggregated `FIX_NEEDED` to the still-alive polecat (reusing the existing event-driven
   `FIX_NEEDED` channel — polecats stay alive through the merge cycle and fix in-place).
4. Polecat fixes for all dissents at once, pushes SHA₂.
5. Refinery re-arms **all** seats against SHA₂; `audit_round++`; back to step 2.
6. Loop ends when all N hold a live `approve` for the **same** current HEAD → MR becomes
   merge-eligible. Teardown all seats, release names.

Batching (one `FIX_NEEDED` per round, refinery-mediated) guarantees exactly one new SHA per
round so convergent-unanimity-on-a-single-SHA is not a moving target, and keeps the Refinery
as the single sender of `FIX_NEEDED`. Nuns never message the worker directly.

## Bounds and escalation

- **Round limit (hard, panel-wide):** `round_limit = 3`. After 3 dissenting rounds in a row,
  **escalate to Mayor/human** and hard-block the MR (`audit-blocked`). A genuine
  `request_changes` is the only thing that advances this counter — infra faults do not.
- **Wall clock (soft):** `wall_clock_min = 60`. If the deadline passes *without* having hit
  the round limit, **notify the Mayor but do NOT block or force-merge** — the audit keeps
  running. (This overrides the issue's original "timed-out verdict = request-changes":
  slowness escalates to a human, never silently passes or fails.)
- **Fail-closed definition:** no merge without unanimous live approvals (a hung Nun simply
  parks the MR forever) **plus** 3-strikes → human. A missing/slow verdict never auto-rejects.

### Escalation resolution (`audit-blocked`)

A fail-closed gate with no human override can deadlock a rig, so the Mayor has three exits:

- **(a) Fix the code** — push a fix to the branch; the new SHA re-arms the panel fresh and
  the round counter resets. Default expectation.
- **(b) `gt audit override <mr>`** — witness/Mayor-only authenticated force-approval; the MR
  proceeds to merge despite unresolved dissent. **Recorded** (trusted field + wisp) for the
  audit trail. Ships in v1 — the required escape valve.
- **(c) Kill the MR** — close it / reassign the source bead; the work does not merge.

## Read-only enforcement (security property, v1)

A Nun is a polecat and by default could edit, commit, and push. For a gate that protects the
target branch — and reads diffs that may contain adversarial/prompt-injection content — that
is unacceptable. Enforced read-only ships in v1.

**Codebase reality (investigated 2026-06-13):** normal polecats run Claude with
`--dangerously-skip-permissions` (`internal/config/agents.go`), i.e. the permission system is
*bypassed*; and all worktrees share one bare repo `.repo.git` (`internal/polecat/manager.go`),
so branch refs are shared. Therefore a Nun **cannot reuse the stock polecat spawn unchanged** —
a **restricted "seat" spawn variant** is required. This is a real, well-scoped subtask, not a
footnote.

- **(c) Structural isolation — the load-bearing guarantee:** the seat worktree is checked out
  **detached** (at the target ref / `audit_sha`), **never** the live audited branch ref, and
  its `origin` **push is unset**. Pushing and ref-advancing become *physically* impossible
  regardless of agent behavior. A mutable checkout isn't even needed: `git diff
  <target>...<audit_sha>` and `git show <sha>:<path>` read straight from the shared bare repo.
  The Refinery owns all origin pushes anyway.
- **(b) Claude permission profile — defense-in-depth:** spawn Claude **without**
  `--dangerously-skip-permissions`, with a curated `settings.json`:
  - `permissions.allow`: the read/diff/verdict toolset (`Read`, `Grep`, `Glob`,
    `Bash(git diff:*)`, `Bash(git show:*)`, `Bash(git log:*)`, `bd` reads, the verdict-write
    command) so the **headless** Nun never hangs on a prompt.
  - `permissions.deny`: `Write`, `Edit`, `NotebookEdit`, `Bash(git push:*)`,
    `Bash(git commit:*)`, `Bash(git merge:*)`, `gt done`, etc.
  - This is Claude-specific — acceptable because `audit.model` is pinned to Opus. If an
    operator repoints `audit.model` at a non-Claude runtime, (b) degrades but **(c) still
    holds** as the security floor.
  - **Hidden work:** curating the allow-list so an autonomous Nun can actually function
    (read + diff + write-verdict) while hard-blocked from mutation. Dropping skip-permissions
    naively makes the Nun hang on the first tool prompt.
- Instruction-only ("you are read-only") is belt-and-suspenders, **never relied upon alone**.
- A Nun's **only** side effect is her verdict (wisp/mail). The only way code changes between
  rounds is the refinery-mediated `FIX_NEEDED` to the real polecat.

## Seat identity, resource model, and failure handling

### Nun roster (dedicated, rotating)

Seats draw names from a roster disjoint from the polecat roster, so a Nun's identity can
never collide with a worker:

`Mary, Teresa, Gertrude, Lucia, Frances, Agnes, Beatrice, Cecelia, Dorothy, Stella,
Elizabeth, Imelda`

Names are **leased on spawn, released on teardown** → a soft rig-wide concurrency limit of
12 concurrent Nuns. (More names can be added later; cheap.)

### Resource quota (separate from `max_polecats`)

- Nuns do **not** count against `max_polecats` — they use a distinct **`max_seats`** quota
  (default 6) so workers and audits never starve each other. The underlying real constraint
  is total concurrent agents / Dolt connections (`ErrDoltAtCapacity`).
- **Seat-allocation backpressure:** if `max_seats` or the name roster is exhausted, the MR
  parks `audit-pending` and re-attempts spawn next cycle — never merges un-audited.
- **On park-due-to-exhaustion, escalate to Mayor** (debounced to once per park event, not
  per retry tick) so a human can decide whether to raise `max_seats` or lower N.

### Spawn / crash faults (infra, not judgment)

Distinct from cap-exhaustion. Neither advances the 3-strike round counter.

- **Spawn failure** (`--create` errors, e.g. *getting pane*): bounded **2 retries**, then
  park + escalate to Mayor. (Depends on the v1.2.0 spawn shorthand giving
  `<rig>/audit-N --create` a real worktree + tmux session in the right cwd.)
- **Mid-audit crash** (session died, no verdict, before wall-clock): Refinery detects the
  dead session (no live tmux + no verdict), **respawns her once, fresh/clean** against the
  current SHA (her context is gone anyway; a fresh independent read is what the gate wants —
  her prior findings already reached the polecat via the batched `FIX_NEEDED`). Then escalate.
- Bounds: **2 spawn-retries / 1 mid-audit respawn**, then escalate.

## Verdict transport (configurable; default wisp)

Each seat writes exactly **one verdict per seat per round**:

- Labels: `nun-verdict, mr:<id>, sha:<audit_sha>, seat:<name>, round:<n>,
  verdict:approve|request_changes`; body = findings prose.
- **SHA-pinning** means a resubmit (new HEAD) makes prior-SHA verdicts non-matching
  automatically — no explicit invalidation needed.
- **Tally** each patrol cycle: query verdicts at `mr:<id>, sha:<audit_sha>`; require N
  `approve` and zero `request_changes`. Any `request_changes` ends the round immediately.

Transport is a knob, `verdict = "wisp" | "mail"`, **default `wisp`** (queryable, idempotent,
survives restart, mirrors the existing quality-review wisp pattern). `mail` produces a
durable, reviewable trail of rejections *and* approvals-that-later-proved-wrong, for
operators who want to inspect Nun behavior. Per-user setup QA is expected to choose this.

## Configuration

Per-rig Refinery config, mirroring `merge_queue`:

```toml
[merge_queue.audit]
enabled        = false            # default OFF — no behavior change until a Mayor opts in
formula        = "mol-nun-audit"  # the bundled reference molecule (formula-agnostic knob)
model          = "opus"           # pinned; Nuns always run this regardless of polecat model
panel_size     = 1                # seats on a normal MR
coven_size     = 3                # seats when MR is labeled audit:coven
max_seats      = 6                # rig-wide concurrent Nun quota (separate from max_polecats)
round_limit    = 3                # dissenting rounds before escalate-to-Mayor (hard block)
wall_clock_min = 60               # soft tripwire: notify Mayor, do NOT block
verdict        = "wisp"           # "wisp" (default) | "mail"
```

### Labels

- **`audit:coven`** — free, unprivileged escalation label (anyone): bump to `coven_size`,
  `depth = deep`. More scrutiny needs no permission.
- **`audit:solo`** — privileged de-escalation back to 1 seat. **Not** a raw label (bead labels
  carry no actor provenance). Applied via an authenticated command that verifies witness/Mayor
  role and stamps a **trusted field** the Refinery honors. Ships in v1.

### Command surface (all role-authenticated)

- `gt audit enable <rig>` / `gt audit disable <rig>` — Mayor-only rig-level opt-in/out.
- `gt audit solo <mr>` — witness/Mayor-only; stamps the trusted de-escalation field.
- `gt audit override <mr>` — witness/Mayor-only; force-approve an `audit-blocked` MR
  (recorded).
- `gt audit status [<rig>|<mr>]` — read-only view of in-flight panels, verdicts, rounds,
  deadline.

## Formula I/O contract (`mol-nun-audit`)

The gate passes each seat:

- **In:** `mr_id`, `source_issue`, `branch`, `target`, `audit_sha` (branch HEAD at spawn),
  `seat_name` (roster), `flavor` (assigned lens), `round` (1-based), `depth`
  (`neighbors` | `deep`). Plan discovery is the agent's job (read source bead → linked /
  committed plan files → else code-only).
- **Out:** exactly one verdict wisp/mail per seat per round (schema above). The verdict is the
  Nun's **only** side effect.

We ship `mol-nun-audit` as the bundled default; `formula` is a knob so operators can point at
their own. (The historical `mol-polecat-plan-audit` file is gone; the gate is a code/diff
audit at merge time, not a pre-implementation plan review.)

## Dependencies

- **v1.2.0 spawn shorthand** (`45fedb3d`): `<rig>/audit-N --create` must produce a real
  worktree + tmux session in the correct cwd. Older builds fail at *getting pane* or land the
  seat in the wrong directory so it can't `git diff` the branch.
- **Restricted "seat" spawn variant** (new subtask — see Read-only enforcement): the gate
  spawns Nuns differently from stock polecats — detached/no-push worktree + Claude without
  `--dangerously-skip-permissions` + curated allow/deny `settings.json`. This is the single
  biggest non-obvious build item; size the bead accordingly.
- Builds on `internal/refinery/engineer.go`: the audit phase sits alongside `runGates` and the
  `no_merge` check in `doMerge`; verdict tally mirrors `IsBeadOpen`-style bead reads.
- Reuses the existing `FIX_NEEDED` event-driven channel
  (`internal/protocol/refinery_handlers.go`).
- Related: #2630 (configurable merge strategy — same opt-in/default-preserving pattern),
  #4167 (sibling bead-state check in `doMerge`), #88 (conflict escalation centralization).

## Out of scope / deferred

- Expanding the Nun roster beyond 12 (trivial follow-up if `max_seats` pressure appears).
- Non-default verdict-transport tuning beyond wisp/mail.
- Any pre-implementation plan-review use of the formula (this gate is merge-time only).

## Open implementation notes (not blocking)

- Debounce the park-exhaustion Mayor escalation to once per park event.
- ~~Confirm whether the restricted-permission profile for read-only seats already exists.~~
  **Resolved 2026-06-13:** it does **not** — polecats run `--dangerously-skip-permissions`
  and share one bare repo. A restricted "seat" spawn variant (detached/no-push worktree +
  no-skip-permissions Claude + curated allow/deny `settings.json`) must be built. The
  `sandboxed-polecat-execution.md` exitbox/daytona work is heavier/future and **not** required
  for this — structural no-push + the Claude profile is sufficient.
- Define the exact `audit-pending` / `audit-blocked` MR states in the bead state machine.
