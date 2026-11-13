-- The older release has no EXECUTING: an approval caught mid-change is
-- recorded as FAILED, which is what it would have shown after a crash.
UPDATE governance_approvals SET state = 'FAILED', result = 'interrupted while executing; check whether the change was made'
WHERE state = 'EXECUTING';
ALTER TABLE governance_approvals DROP CONSTRAINT IF EXISTS governance_approvals_state_check;
ALTER TABLE governance_approvals ADD CONSTRAINT governance_approvals_state_check
    CHECK (state IN ('PENDING', 'EXECUTED', 'REJECTED', 'EXPIRED', 'FAILED'));
