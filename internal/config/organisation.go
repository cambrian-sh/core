package config

import (
	"fmt"
	"strings"
	"time"
	// The IANA zone database, embedded: a Windows host (and a scratch container) has
	// none, and time.LoadLocation("Europe/Istanbul") then fails at boot.
	_ "time/tzdata"
)

// OrganisationConfig is the `organisation` block: facts about the deployment's
// organisation that the kernel tells its models.
type OrganisationConfig struct {
	// Timezone is the IANA zone ("Europe/Istanbul", "America/New_York") the
	// organisation's calendar is in: the date the planner, the plan's steps and the
	// chat front desk are told is today, and the reference dates of relative asks
	// ("last two weeks", "this quarter") are computed in it (domain.Calendar,
	// T-20261107-planner-and-chat-agent-are-not-told-today-s). Empty ⇒ UTC. Read by
	// app → domain.NewCalendar.
	Timezone string `json:"timezone,omitempty"`
}

// Location resolves Timezone (empty ⇒ UTC).
func (o OrganisationConfig) Location() (*time.Location, error) {
	tz := strings.TrimSpace(o.Timezone)
	if tz == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("organisation.timezone %q is not an IANA timezone: %v", tz, err)
	}
	return loc, nil
}

// validationErrors reports an unresolvable timezone.
func (o OrganisationConfig) validationErrors() []string {
	if _, err := o.Location(); err != nil {
		return []string{err.Error()}
	}
	return nil
}
