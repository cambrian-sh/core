# Bounded Decision Note: Operator Config Rollout State Transitions

## 1. Fixture Title
**Operator Config Rollout State Transition Fixture**

## 2. Constraint
The rollout must transition through states `pending → applied → verified` (or retryable failure with rollback) without creating duplicate rollout entries on retry. A retry of the same config version shall not produce a second `applied` entry for the same change; the state machine must resume from the last recorded state.

## 3. Acceptance Check
Given an operator config rollout that transitions to `retrying` exactly once and recovers to `pending`, then proceeds to `applied` and finally `verified`, verify that the final rollout history contains exactly one entry for the config change (i.e., no duplicate `applied` or `verified` records, and the total entry count for that change version is 1).