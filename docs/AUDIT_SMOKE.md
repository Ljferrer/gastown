# Audit Smoke Tests

End-to-end validation log for the Nun audit gate. Each line records a smoke-test
run that exercises the audit panel + seat spawn path before merge.

- 2026-06-14: LGT audit-gate validation with new binary (11d7f0b8) — verifies bare-repo config self-heal (worktreeConfig migration, version stays 1, core.bare → config.worktree) and that a Nun convenes and approves before merge.
