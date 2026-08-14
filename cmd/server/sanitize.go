package main

import (
	"fmt"
	"log"
)

// truncateWithWarning ensures a string fits a VARCHAR(N) type.
// If truncation happens, a warning is appended (and logged) with context.
func truncateWithWarning(warnings *[]string, context, field, value string, max int) string {
	if value == "" || max <= 0 {
		return value
	}
	r := []rune(value)
	if len(r) <= max {
		return value
	}
	truncated := string(r[:max])
	msg := fmt.Sprintf("%s: truncated %s from %d to %d chars", context, field, len(r), max)
	if warnings != nil {
		*warnings = append(*warnings, msg)
	}
	log.Printf("WARN: %s", msg)
	return truncated
}
