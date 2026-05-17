// Package envcfg has small env-var helpers — defaulted reads for
// string, int, bool, and duration values. Mirror of root
// internal/envcfg/ — keep them in lockstep (or extract to a shared
// module via go.work later).
package envcfg

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// String reads key from the env. Returns def if unset or empty.
func String(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Int reads key as an integer. Returns def on unset / empty / parse error.
func Int(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// Bool reads key as a boolean. Accepts "true"/"1" (case-insensitive) as
// true; everything else (including unset) returns def.
func Bool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return strings.EqualFold(v, "true") || v == "1"
}

// Duration reads key as a Go-syntax duration (e.g. "5s", "1h30m").
// Returns def on unset / empty / parse error.
func Duration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
