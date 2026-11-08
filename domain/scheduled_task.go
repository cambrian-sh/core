package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Principal-owned recurring tasks
// (T-20261106-the-assistant-cannot-set-up-a-recurring-task).
//
// An assistant may PROPOSE a recurring task for the principal it serves ("every
// Monday at 8 send me a summary of X"). The kernel asks that principal's approval
// through the one approval hub (ApprovalKindSchedule — the console Inbox) and
// creates the task only on an explicit approve. A created task is a premium
// schedule watch (REACT-06 / ADR-0072 — ONE scheduler, the ReactiveEngine; no
// second one) whose action, ScheduledTurnAction, runs one chat turn AS THE OWNER
// in a conversation the owner holds. The run principal is therefore the owner by
// the ordinary conversation-owner rule (ADR-0139 D14), so their notes, their
// per-user credentials and their read scope apply, unattended.
//
// The owner is never a request field: the agent plane resolves it from the
// presented lease (conversation owner -> task beneficiary -> configured default).

// ApprovalKindSchedule is the ApprovalRequest.Kind of a scheduled-task proposal.
const ApprovalKindSchedule = "schedule"

// ScheduledTurnAction is the WatchAction.Type a principal-owned schedule runs.
// Contributed by the premium reactive plugin (ADR-0104), never a built-in.
const ScheduledTurnAction = "scheduled_turn"

// Delivery modes for a scheduled task's result.
const (
	// ScheduleDeliverConversation: the reply lands in the conversation the owner
	// holds for this task (the default — it is what "inbox" means here).
	ScheduleDeliverConversation = "conversation"
	// ScheduleDeliverNotify: as conversation, plus a notification event.
	ScheduleDeliverNotify = "notify"
	// ScheduleDeliverEmail: the run is asked to email the result through the
	// owner's OWN mail integration (a per-user credential, ADR-0140), so it fails
	// closed when the owner has connected none.
	ScheduleDeliverEmail = "email"
)

// ValidScheduleDelivery reports whether d is a known delivery mode ("" = default).
func ValidScheduleDelivery(d string) bool {
	switch d {
	case "", ScheduleDeliverConversation, ScheduleDeliverNotify, ScheduleDeliverEmail:
		return true
	}
	return false
}

// ScheduledTaskSpec is what a proposal asks for. Cron is the ONE schedule
// vocabulary the scheduler runs; an RRULE is translated (RRuleToCron) before a
// spec is built.
type ScheduledTaskSpec struct {
	OwnerID  string
	Title    string
	Goal     string
	Cron     string
	Timezone string
	AgentID  string
	Delivery string
	// ProposedBy is the agent that proposed it; ApprovedBy who approved it.
	ProposedBy string
	ApprovedBy string
}

// ScheduledTask is one created task.
type ScheduledTask struct {
	ID             string
	OwnerID        string
	Title          string
	Goal           string
	Cron           string
	Timezone       string
	AgentID        string
	Delivery       string
	Active         bool
	ConversationID string
	CreatedAt      time.Time
	ProposedBy     string
	ApprovedBy     string
}

// ScheduleLimits are the per-deployment caps (execution.schedules.*), passed on
// every check so the store enforces exactly what the operator configured.
type ScheduleLimits struct {
	// MaxPerPrincipal bounds how many tasks one principal may hold. 0 = no cap.
	MaxPerPrincipal int
	// MinInterval is the shortest allowed gap between two firings. 0 = no floor.
	MinInterval time.Duration
}

// Refusals a PrincipalSchedules implementation returns; the agent plane maps each
// onto a ScheduledTaskRejection the model can read.
var (
	ErrScheduleInvalid     = errors.New("scheduled task: invalid")
	ErrScheduleLimit       = errors.New("scheduled task: per-principal limit reached")
	ErrScheduleTooFrequent = errors.New("scheduled task: fires more often than the minimum interval")
	ErrScheduleNotFound    = errors.New("scheduled task: not found")
)

// PrincipalSchedules is the store of principal-owned recurring tasks. The premium
// reactive plugin implements it on its schedule watches; a kernel without one
// does not advertise "scheduled-tasks" and the agent-plane RPCs are Unimplemented.
type PrincipalSchedules interface {
	// Check validates a spec and the caps WITHOUT creating anything, so a
	// proposal that could never be created is refused before the user is asked.
	Check(ctx context.Context, spec ScheduledTaskSpec, limits ScheduleLimits) error
	// Create creates the task (idempotent on ScheduledTaskID: an identical
	// re-proposal returns the existing task).
	Create(ctx context.Context, spec ScheduledTaskSpec, limits ScheduleLimits) (ScheduledTask, error)
	// List returns ownerID's tasks; "" = every principal's (the operator view).
	List(ctx context.Context, ownerID string) ([]ScheduledTask, error)
	// SetActive pauses/resumes one task. ownerID must own it (else
	// ErrScheduleNotFound); "" = an operator acting on any task.
	SetActive(ctx context.Context, ownerID, id string, active bool) error
	// Delete removes one task, with SetActive's ownership rule.
	Delete(ctx context.Context, ownerID, id string) error
}

// ScheduledTaskID is the deterministic id of a spec (AGENTS.md §2.6): the same
// owner asking for the same thing on the same schedule is the same task, so a
// retried proposal never creates a duplicate.
func ScheduledTaskID(s ScheduledTaskSpec) string {
	h := sha256.New()
	for _, f := range []string{s.OwnerID, strings.TrimSpace(s.Cron), s.Timezone,
		strings.TrimSpace(s.Goal), s.AgentID, s.Delivery} {
		h.Write([]byte(f))
		h.Write([]byte{0})
	}
	return "sched-" + hex.EncodeToString(h.Sum(nil))[:16]
}

// ScheduledTaskConversationID is the conversation a task's runs land in, owned
// by the task's owner. Derived from the task id, so every firing of one task
// writes to the same thread.
func ScheduledTaskConversationID(taskID string) string { return "sched:" + taskID }

// OwnedTurn is one chat turn run FOR a principal by the kernel itself (a
// scheduled task firing): the conversation is created owned by OwnerID if absent,
// and refused if it exists under anyone else.
type OwnedTurn struct {
	ConversationID string
	OwnerID        string
	Title          string
	Text           string
	// Policy is the conversation's system/policy prompt, stored at creation.
	Policy string
	// ClientID is the turn's idempotency key (a re-fired instant replays).
	ClientID string
}

// RRuleToCron translates the RFC 5545 RRULE subset a recurring task needs into
// the 5-field cron the scheduler runs, so there is ONE schedule vocabulary at
// runtime. Supported: FREQ=HOURLY|DAILY|WEEKLY|MONTHLY, BYMINUTE, BYHOUR, BYDAY
// (plain MO..SU, no ordinals), BYMONTHDAY, and INTERVAL only where cron can say it
// (HOURLY). Anything else — COUNT, UNTIL, BYSETPOS, ordinal BYDAY, an INTERVAL
// cron cannot express — is refused rather than approximated: a schedule that
// silently fires on different days than the user asked for is worse than "no".
func RRuleToCron(rrule string) (string, error) {
	r := strings.TrimSpace(rrule)
	r = strings.TrimPrefix(strings.TrimPrefix(r, "RRULE:"), "rrule:")
	if r == "" {
		return "", fmt.Errorf("%w: empty rrule", ErrScheduleInvalid)
	}
	parts := map[string]string{}
	for _, kv := range strings.Split(r, ";") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return "", fmt.Errorf("%w: rrule part %q", ErrScheduleInvalid, kv)
		}
		parts[strings.ToUpper(strings.TrimSpace(k))] = strings.ToUpper(strings.TrimSpace(v))
	}
	for k := range parts {
		switch k {
		case "FREQ", "BYMINUTE", "BYHOUR", "BYDAY", "BYMONTHDAY", "INTERVAL", "WKST":
		default:
			return "", fmt.Errorf("%w: rrule %s is not supported", ErrScheduleInvalid, k)
		}
	}
	interval := 1
	if v, ok := parts["INTERVAL"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return "", fmt.Errorf("%w: rrule INTERVAL %q", ErrScheduleInvalid, v)
		}
		interval = n
	}
	list := func(key string, lo, hi int, def string) (string, error) {
		v, ok := parts[key]
		if !ok {
			return def, nil
		}
		var out []int
		for _, x := range strings.Split(v, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(x))
			if err != nil || n < lo || n > hi {
				return "", fmt.Errorf("%w: rrule %s value %q", ErrScheduleInvalid, key, x)
			}
			out = append(out, n)
		}
		sort.Ints(out)
		s := make([]string, len(out))
		for i, n := range out {
			s[i] = strconv.Itoa(n)
		}
		return strings.Join(s, ","), nil
	}
	minute, err := list("BYMINUTE", 0, 59, "0")
	if err != nil {
		return "", err
	}
	hour, err := list("BYHOUR", 0, 23, "0")
	if err != nil {
		return "", err
	}
	dom, err := list("BYMONTHDAY", 1, 31, "*")
	if err != nil {
		return "", err
	}
	dow := "*"
	if v, ok := parts["BYDAY"]; ok {
		days := map[string]int{"SU": 0, "MO": 1, "TU": 2, "WE": 3, "TH": 4, "FR": 5, "SA": 6}
		var out []int
		for _, d := range strings.Split(v, ",") {
			n, ok := days[strings.TrimSpace(d)]
			if !ok {
				return "", fmt.Errorf("%w: rrule BYDAY %q (ordinals are not supported)", ErrScheduleInvalid, d)
			}
			out = append(out, n)
		}
		sort.Ints(out)
		s := make([]string, len(out))
		for i, n := range out {
			s[i] = strconv.Itoa(n)
		}
		dow = strings.Join(s, ",")
	}
	freq := parts["FREQ"]
	if interval != 1 && freq != "HOURLY" {
		return "", fmt.Errorf("%w: rrule INTERVAL=%d with FREQ=%s cannot be expressed", ErrScheduleInvalid, interval, freq)
	}
	switch freq {
	case "HOURLY":
		if _, set := parts["BYHOUR"]; set {
			return "", fmt.Errorf("%w: rrule FREQ=HOURLY with BYHOUR", ErrScheduleInvalid)
		}
		h := "*"
		if interval > 1 {
			if interval > 23 {
				return "", fmt.Errorf("%w: rrule INTERVAL=%d hours", ErrScheduleInvalid, interval)
			}
			h = "*/" + strconv.Itoa(interval)
		}
		return strings.Join([]string{minute, h, dom, "*", dow}, " "), nil
	case "DAILY":
		return strings.Join([]string{minute, hour, dom, "*", dow}, " "), nil
	case "WEEKLY":
		if dow == "*" {
			return "", fmt.Errorf("%w: rrule FREQ=WEEKLY needs BYDAY", ErrScheduleInvalid)
		}
		return strings.Join([]string{minute, hour, "*", "*", dow}, " "), nil
	case "MONTHLY":
		if dom == "*" {
			return "", fmt.Errorf("%w: rrule FREQ=MONTHLY needs BYMONTHDAY", ErrScheduleInvalid)
		}
		return strings.Join([]string{minute, hour, dom, "*", "*"}, " "), nil
	case "":
		return "", fmt.Errorf("%w: rrule FREQ is required", ErrScheduleInvalid)
	default:
		return "", fmt.Errorf("%w: rrule FREQ=%s is not supported", ErrScheduleInvalid, freq)
	}
}
