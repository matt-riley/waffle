package main

import (
	"encoding/json"
	"io"
)

// takeJSONFlag strips every --json token from args and reports whether any
// were present. Remaining arguments keep their original order.
func takeJSONFlag(args []string) (rest []string, jsonOut bool) {
	rest = make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "--json" {
			jsonOut = true
			continue
		}
		rest = append(rest, arg)
	}
	return rest, jsonOut
}

// writeJSON encodes v as a single JSON value to w (with a trailing newline).
func writeJSON(w io.Writer, v any) error {
	return json.NewEncoder(w).Encode(v)
}
