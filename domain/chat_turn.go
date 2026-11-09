package domain

// HandoffKeyAskBackPendingRef is the Execute request metadata key carrying the
// ledger id of the pending InputRequired (InputRequired.LedgerRef) a delegated
// request answers (ADR-0145 D5a resume). Kernel-written only: Server.Execute
// stamps it, with HandoffKeyAskBackRound, from the caller's turn lease binding
// (LeaseBinding.AskBackPendingRef / AskBackRound, set by the chat lane) and
// deletes whatever a caller sent under either key.
const HandoffKeyAskBackPendingRef = "_ask_back_pending"
