package skill

import (
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/skill/spec"
)

// TestSetFrontmatterStatusMetadataForm: activate/deactivate record state
// under metadata.waffle/status, migrating the legacy top-level status away,
// and the result still validates under the shared validator (#396).
func TestSetFrontmatterStatusMetadataForm(t *testing.T) {
	raw := "---\nname: reviewer\ndescription: Reviews changes.\nstatus: active\nowner: matt\n---\n\n# Review\n"
	updated, err := setFrontmatterStatus(raw, StatusInactive)
	if err != nil {
		t.Fatalf("setFrontmatterStatus: %v", err)
	}
	fields, body, err := spec.ParseFrontmatter(updated)
	if err != nil {
		t.Fatalf("rewritten frontmatter unparseable: %v\n%s", err, updated)
	}
	if fields[spec.WaffleStatusKey] != StatusInactive {
		t.Errorf("metadata status = %q, want inactive\n%s", fields[spec.WaffleStatusKey], updated)
	}
	if _, legacy := fields["status"]; legacy {
		t.Errorf("legacy top-level status not migrated away:\n%s", updated)
	}
	if fields["name"] != "reviewer" || fields["owner"] != "matt" {
		t.Errorf("other fields lost: %v", fields)
	}
	if err := spec.Validate(fields["name"], fields["description"], fields, body, "reviewer"); err != nil {
		t.Errorf("rewritten file fails the shared validator: %v", err)
	}
}

// TestSetFrontmatterStatusLeavesLegacyFilesUntouched: a frontmatter-less
// file must never become a status-only frontmatter block, and a file
// missing required fields is not corrupted (#396); activation state is
// authoritative in the skill_status table.
func TestSetFrontmatterStatusLeavesLegacyFilesUntouched(t *testing.T) {
	bare := "# Just instructions, no frontmatter.\n"
	got, err := setFrontmatterStatus(bare, StatusInactive)
	if err != nil || got != bare {
		t.Errorf("frontmatter-less = %q, %v; want untouched", got, err)
	}
	if strings.HasPrefix(got, "---") {
		t.Errorf("frontmatter-less file gained a status-only block:\n%s", got)
	}

	noDescription := "---\nname: reviewer\nstatus: inactive\n---\n\n# Review\n"
	got, err = setFrontmatterStatus(noDescription, StatusActive)
	if err != nil || got != noDescription {
		t.Errorf("missing-description = %q, %v; want untouched", got, err)
	}
}

// TestIsActiveFrontmatterMetadataAndLegacy: reads the waffle metadata key
// first, then the legacy top-level status; missing status defaults active.
func TestIsActiveFrontmatterMetadataAndLegacy(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "metadata inactive", raw: "---\nname: x\ndescription: d\nmetadata:\n  waffle/status: inactive\n---\n", want: false},
		{name: "metadata active", raw: "---\nname: x\ndescription: d\nmetadata:\n  waffle/status: active\n---\n", want: true},
		{name: "legacy inactive", raw: "---\nname: x\ndescription: d\nstatus: inactive\n---\n", want: false},
		{name: "missing defaults active", raw: "---\nname: x\ndescription: d\n---\n", want: true},
		{name: "frontmatter-less defaults active", raw: "# no frontmatter\n", want: true},
		// Newer metadata form wins over the legacy field.
		{name: "metadata wins over legacy", raw: "---\nname: x\ndescription: d\nmetadata:\n  waffle/status: inactive\nstatus: active\n---\n", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isActiveFrontmatter(tc.raw); got != tc.want {
				t.Errorf("isActiveFrontmatter = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestActivateDeactivateRoundTripValidates: a skill activated and then
// deactivated still validates under the shared validator, with activation
// state in the metadata key.
func TestActivateDeactivateRoundTripValidates(t *testing.T) {
	raw := "---\nname: round-trip\ndescription: Round trips.\n---\n\n# Body\n"
	for _, status := range []string{StatusActive, StatusInactive} {
		updated, err := setFrontmatterStatus(raw, status)
		if err != nil {
			t.Fatalf("setFrontmatterStatus(%s): %v", status, err)
		}
		fields, body, err := spec.ParseFrontmatter(updated)
		if err != nil {
			t.Fatalf("round-trip parse: %v", err)
		}
		if fields[spec.WaffleStatusKey] != status {
			t.Errorf("status = %q, want %q", fields[spec.WaffleStatusKey], status)
		}
		if err := spec.Validate(fields["name"], fields["description"], fields, body, "round-trip"); err != nil {
			t.Errorf("round-tripped skill fails validator: %v", err)
		}
	}
}
