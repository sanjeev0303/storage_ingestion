-- An approved change is EXECUTING while it runs, and EXECUTED or FAILED
-- once it has (docs/governance.md): a crash mid-change no longer leaves an
-- approval claiming a change that was never made.
ALTER TABLE governance_approvals DROP CONSTRAINT IF EXISTS governance_approvals_state_check;
ALTER TABLE governance_approvals ADD CONSTRAINT governance_approvals_state_check
    CHECK (state IN ('PENDING', 'EXECUTING', 'EXECUTED', 'REJECTED', 'EXPIRED', 'FAILED'));
