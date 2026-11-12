package domain

// HandoffKeyAnswerRef is the step-response metadata key by which a worker answers
// with a ledger reference instead of re-typing a tool output (owner decision
// 2026-11-12, T-20261112-summarise-a-document-step-gets-only-the-first): the id of
// a tool_result entry from the step's OWN calls. The SDK sets it from final_answer's
// answer_ref argument; the DAG executor validates it (a tool result of this run and
// this step) and makes the entry's readable text (ResultText) the step's output, so
// a 38 kB "extract the full content" step costs no 38 kB of generated tokens. It is
// kernel-consumed: removed from the response before the merge.
const HandoffKeyAnswerRef = "_answer_ref"
