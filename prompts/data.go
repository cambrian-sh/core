package prompts

import (
	"regexp"
	"strings"
	"sync"
)

// NeutraliseClosingTags rewrites every closing tag `</name>` (any case, any
// spacing) of the named tags inside an untrusted value to `[/name]`, so a value
// rendered into a tagged data region cannot end that region early and have the
// rest of it read as instructions (SPEC.md §3.1: "The caller neutralises a
// closing tag inside the value before rendering").
func NeutraliseClosingTags(value string, tags ...string) string {
	for _, t := range tags {
		value = closingTag(t).ReplaceAllString(value, "[/"+t+"]")
	}
	return value
}

var closingTags sync.Map // tag name -> *regexp.Regexp

func closingTag(tag string) *regexp.Regexp {
	if re, ok := closingTags.Load(tag); ok {
		return re.(*regexp.Regexp)
	}
	re := regexp.MustCompile(`(?i)</\s*` + regexp.QuoteMeta(strings.TrimSpace(tag)) + `\s*>`)
	closingTags.Store(tag, re)
	return re
}
