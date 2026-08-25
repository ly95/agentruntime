// Package skills loads host-selected Skill directories into immutable,
// content-addressed snapshots. The built-in source is local and explicit;
// LoadSet applies one parser, resource limits, conflict rules, and deterministic
// hashing to every source. The package never discovers user directories, fetches
// remote Skills, or executes Skill files.
package skills
