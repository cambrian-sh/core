## Bounded Decision Note: Async Ingestion Reliability (Latency Case 14)

### Fixture Title
**Async Ingestion Job Progress Fixture**

### Constraint
The job must transition through states accepted → indexed_chunks → extracted_triplets → success (or a retryable failure) without data duplication on retry.

### Acceptance Check
Given a job that transitions to the retrying state exactly once and then recovers to accepted, verify that the final indexed_chunks count equals the number of unique chunks in the original payload (i.e., no duplicate chunk indices appear).