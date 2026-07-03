package secret

import "strings"

// minRedactLen guards against degenerate secrets ("1", "ok") turning the
// redactor into a text shredder. Anything shorter isn't much of a secret.
const minRedactLen = 6

// Redactor scrubs known secret values from text. The gateway runs every
// tool result, log line, and trace attribute through one of these so a
// stray `env` or `cat` can't leak a stored key into the transcript.
type Redactor struct {
	replacer *strings.Replacer
}

// NewRedactor builds a Redactor over every value currently in the store.
// Rebuild after mutations; for phase 0 scale (a handful of secrets) that is
// perfectly cheap.
func NewRedactor(store Store) (*Redactor, error) {
	names, err := store.List()
	if err != nil {
		return nil, err
	}
	pairs := make([]string, 0, len(names)*2)
	for _, name := range names {
		v, err := store.Get(name)
		if err != nil {
			return nil, err
		}
		if len(v) < minRedactLen {
			continue
		}
		pairs = append(pairs, v, "[redacted:"+name+"]")
	}
	return &Redactor{replacer: strings.NewReplacer(pairs...)}, nil
}

// Redact returns s with every known secret value replaced by its
// [redacted:name] marker.
func (r *Redactor) Redact(s string) string { return r.replacer.Replace(s) }
