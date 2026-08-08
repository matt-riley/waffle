// Package flock provides cross-process advisory locking for the single-writer
// files waffle keeps outside SQLite — the age secret store and MEMORY.md.
// Both are read-modify-write files that more than one waffle process can hold
// open at once (a chat REPL beside serve), where a process-local mutex
// serializes nothing.
//
// On unix this is an exclusive flock on a sidecar lockfile, released by the
// kernel if the holder dies. Elsewhere it degrades to a process-local lock
// with the same signature and timeout semantics; cross-process coordination
// is not available there.
package flock
