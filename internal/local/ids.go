package local

import "github.com/google/uuid"

// Deterministic IDs let client and server converge on the same UUID for a
// given natural key without coordination. Both sides compute the v5 UUID
// from the namespace + key bytes, so a row inserted offline gets the
// identical id once it reaches Postgres on push. ON CONFLICT then dedupes
// trivially, and references from other tables (observations.entity_id,
// relations.from_entity_id, …) don't need rewriting after sync.
//
// Observations and projects use random UUIDs: observations are append-only
// (each is distinct, dedupe isn't useful), and projects are bootstrapped
// once via the unique slug.
var (
	NamespaceEntities  = uuid.MustParse("d2cb15c5-f6df-4f02-9b1d-3f06f6c1c1e0")
	NamespaceRelations = uuid.MustParse("a3a59d6e-6240-4b15-9d6b-5a9b71c4a2e5")
	NamespaceProjects  = uuid.MustParse("8e2b0d56-5e2f-4f08-b1ad-2c93f1ffe10c")
)

// nul is the separator between key components in v5 input bytes. A NUL byte
// can't appear inside a name/slug/relation_type, so this is unambiguous.
const nul = "\x00"

// EntityID returns the deterministic UUID for an entity given its project
// and name. Both must be normalized before calling (no case-insensitive
// matching here — the caller decides if names are case-sensitive).
func EntityID(projectID, name string) string {
	return uuid.NewSHA1(NamespaceEntities, []byte(projectID+nul+name)).String()
}

// RelationID returns the deterministic UUID for a relation.
func RelationID(projectID, fromName, toName, relationType string) string {
	return uuid.NewSHA1(NamespaceRelations, []byte(projectID+nul+fromName+nul+toName+nul+relationType)).String()
}

// ProjectID returns the deterministic UUID for a project slug. Slugs are
// globally unique so this is safe for cross-device convergence.
func ProjectID(slug string) string {
	return uuid.NewSHA1(NamespaceProjects, []byte(slug)).String()
}
