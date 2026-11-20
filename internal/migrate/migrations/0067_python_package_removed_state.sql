-- 0067_python_package_removed_state.sql — a removed state for per-agent Python package
-- approvals (ADR-0157 D7.10.3, Amendment A1 stage P5; ENG-2012).
-- cambrian:not-destructive: widens one CHECK constraint; no row is changed or dropped.
-- ADDITIVE: an older binary never writes 'removed' and reads it as an unknown state
-- (it neither installs nor asks again for such a record).
--
-- An Operator may remove a runtime-requested package from an agent's layer
-- (RemovePythonPackage, operator contract 0119). The record moves from approved to
-- removed and the removal is audited in python_package_events (action 'removed', with
-- the layer generation built without it). Asking for the same package again raises a
-- fresh approval card.
--
-- Rollback: UPDATE python_package_approvals SET state = 'denied' WHERE state = 'removed';
--           then restore the 0057 constraint (state IN pending, approved, denied, expired).

ALTER TABLE python_package_approvals DROP CONSTRAINT IF EXISTS python_package_approvals_state_check;
ALTER TABLE python_package_approvals ADD CONSTRAINT python_package_approvals_state_check
    CHECK (state IN ('pending', 'approved', 'denied', 'expired', 'removed'));
