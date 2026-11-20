//go:build race

package agentmgr

// raceBuild reports a -race test binary. The e2e abusers are this test binary
// itself, so a -race build carries the race runtime's startup reservations into
// the sandboxed agent (ENG-2276).
const raceBuild = true
