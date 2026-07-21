package main

import (
	"strings"
	"testing"
)

func TestTakeJSONFlag(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    []string
		jsonOut bool
	}{
		{name: "empty", args: nil, want: []string{}},
		{name: "no flag", args: []string{"ls"}, want: []string{"ls"}},
		{name: "flag only", args: []string{"--json"}, want: []string{}, jsonOut: true},
		{name: "flag after sub", args: []string{"ls", "--json"}, want: []string{"ls"}, jsonOut: true},
		{name: "flag before sub", args: []string{"--json", "ls"}, want: []string{"ls"}, jsonOut: true},
		{name: "duplicate flag", args: []string{"--json", "ls", "--json"}, want: []string{"ls"}, jsonOut: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rest, jsonOut := takeJSONFlag(tt.args)
			if jsonOut != tt.jsonOut {
				t.Fatalf("jsonOut = %v, want %v", jsonOut, tt.jsonOut)
			}
			if strings.Join(rest, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("rest = %q, want %q", rest, tt.want)
			}
		})
	}
}
