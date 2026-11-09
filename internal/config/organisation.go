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
	// Holidays is the organisation's holiday calendar for working-day
	// calculations (ADR-0145 D4, the calc_working_days / calc_date_add kernel
	// tools): "YYYY-MM-DD" for a dated holiday, "MM-DD" for one that falls on the
	// same day every year. Empty ⇒ no holidays. Read by app → internal/calc.
	Holidays []string `json:"holidays,omitempty"`
	// Weekend names the weekdays that are not working days ("saturday",
	// "sunday", ...). Empty ⇒ Saturday and Sunday.
	Weekend []string `json:"weekend,omitempty"`
}

// WeekendDays resolves Weekend (empty ⇒ Saturday and Sunday).
func (o OrganisationConfig) WeekendDays() ([]time.Weekday, error) {
	if len(o.Weekend) == 0 {
		return []time.Weekday{time.Saturday, time.Sunday}, nil
	}
	var out []time.Weekday
	for _, w := range o.Weekend {
		d, ok := weekdayNames[strings.ToLower(strings.TrimSpace(w))]
		if !ok {
			return nil, fmt.Errorf("organisation.weekend %q is not a weekday name", w)
		}
		out = append(out, d)
	}
	return out, nil
}

var weekdayNames = map[string]time.Weekday{
	"sunday": time.Sunday, "monday": time.Monday, "tuesday": time.Tuesday, "wednesday": time.Wednesday,
	"thursday": time.Thursday, "friday": time.Friday, "saturday": time.Saturday,
}

// holidayErrors reports holidays that are neither YYYY-MM-DD nor MM-DD.
func (o OrganisationConfig) holidayErrors() []string {
	var errs []string
	for _, h := range o.Holidays {
		h = strings.TrimSpace(h)
		if _, err := time.Parse("2006-01-02", h); err == nil {
			continue
		}
		if _, err := time.Parse("01-02", h); err == nil {
			continue
		}
		errs = append(errs, fmt.Sprintf("organisation.holidays %q must be YYYY-MM-DD or MM-DD", h))
	}
	return errs
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

// validationErrors reports an unresolvable timezone, an unknown weekend day and
// a malformed holiday.
func (o OrganisationConfig) validationErrors() []string {
	var errs []string
	if _, err := o.Location(); err != nil {
		errs = append(errs, err.Error())
	}
	if _, err := o.WeekendDays(); err != nil {
		errs = append(errs, err.Error())
	}
	return append(errs, o.holidayErrors()...)
}
