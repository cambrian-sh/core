package config

import (
	"strings"
	"testing"
	"time"
)

func TestOrganisationLocation(t *testing.T) {
	loc, err := OrganisationConfig{}.Location()
	if err != nil || loc != time.UTC {
		t.Fatalf("empty timezone = %v, %v; want UTC", loc, err)
	}
	loc, err = OrganisationConfig{Timezone: " Europe/Istanbul "}.Location()
	if err != nil || loc.String() != "Europe/Istanbul" {
		t.Fatalf("Europe/Istanbul = %v, %v", loc, err)
	}
	if _, err := (OrganisationConfig{Timezone: "Mars/Olympus"}).Location(); err == nil {
		t.Fatal("an unknown zone must not resolve")
	}
}

// A wrong timezone is a validation error at load, not a silent UTC.
func TestOrganisationTimezoneIsValidated(t *testing.T) {
	c := &Config{}
	c.Organisation.Timezone = "Mars/Olympus"
	found := false
	for _, e := range c.validationErrors() {
		if strings.Contains(e, "organisation.timezone") {
			found = true
		}
	}
	if !found {
		t.Fatalf("validationErrors lacks the timezone error: %q", c.validationErrors())
	}
	c.Organisation.Timezone = "America/New_York"
	for _, e := range c.validationErrors() {
		if strings.Contains(e, "organisation.timezone") {
			t.Fatalf("a valid zone was rejected: %s", e)
		}
	}
}
