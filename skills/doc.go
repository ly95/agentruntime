// Package skills loads host-selected Skill directories into immutable,
// content-addressed snapshots. Sources acquire files; LoadSet applies one
// parser, resource limits, conflict rules, and deterministic hashing to every
// source. The package never discovers user directories or executes Skill files.
package skills
