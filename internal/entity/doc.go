// Package entity is docs/blob-format.md in code: how an entity's events are
// serialized, identified, ordered, and resolved into current state.
//
// It is entity-agnostic on purpose. A caller supplies its own Vocabulary —
// which op names are scalars — so the same code serves issues, pull requests,
// and anything added later. Nothing here may import a specific entity type.
//
// Four files, in dependency order:
//
//   - canonical.go — RFC 8785 canonical JSON. An event's identity is the hash
//     of its own bytes, so two implementations that serialize differently fork
//     identity. encoding/json is never used to produce these bytes: it
//     HTML-escapes '<', '>' and '&' by default and marshals struct fields in
//     declaration order, and either alone would break id stability.
//   - event.go — the eight fields, derived ids, the (c, id) ordering rule.
//   - fold.go — the three field kinds: scalar, list, thread.
//   - store.go — reading a notes ref and folding what it holds.
//
// Two invariants from AGENTS.md live here and are easy to break by accident:
//
//   - An event's id is `git hash-object` of its canonical line, newline
//     included. No id field is ever stored.
//   - Order by (c, id). Never by ts, which is display metadata written by
//     someone else's clock.
package entity
