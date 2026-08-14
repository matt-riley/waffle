package plugin

import (
	"errors"
	"fmt"
	"strings"
)

// ErrInvalidName rejects a plugin whose manifest name violates the spec
// §5.5 constraints, or an install request for a badly named plugin.
var ErrInvalidName = errors.New("invalid plugin name")

const maxNameLength = 64

// ValidName reports whether name satisfies the spec §5.5 plugin-name
// constraints: 1–64 characters drawn from [a-z0-9.-], an alphanumeric first
// and last character, and no consecutive hyphens or periods. The character
// set is ASCII, so byte length equals character length for valid names.
func ValidName(name string) bool {
	if len(name) == 0 || len(name) > maxNameLength {
		return false
	}
	for i := 0; i < len(name); i++ {
		if !nameChar(name[i]) {
			return false
		}
	}
	if !nameAlnum(name[0]) || !nameAlnum(name[len(name)-1]) {
		return false
	}
	return !strings.Contains(name, "--") && !strings.Contains(name, "..")
}

func nameChar(c byte) bool {
	return nameAlnum(c) || c == '.' || c == '-'
}

func nameAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

// rejectName builds the manifest-rejection error for a name that fails
// ValidName.
func rejectName(name string) error {
	return fmt.Errorf("%w: %q violates the spec §5.5 name constraints", ErrInvalidName, name)
}
