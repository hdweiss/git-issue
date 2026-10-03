// The wire shapes, kept apart from the types the bridge consumes.
//
// Azure DevOps returns fields as an untyped map whose values are strings,
// numbers, booleans or identity objects depending on the field, and it spells
// some of them differently between server versions. Isolating that here means
// the WorkItem a bridge sees is already normalised, and that a server quirk is
// fixed in one place rather than at every use.

package adoapi

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// timestamp is a time that tolerates the values Azure DevOps actually sends.
type timestamp struct{ time.Time }

func (t *timestamp) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return err
	}
	t.Time = parsed
	return nil
}

// or returns the first of the two that is set, which is how an optional
// modified date falls back to the created one.
func (t timestamp) or(other timestamp) time.Time {
	if !t.IsZero() && !t.sentinel() {
		return t.Time
	}
	return other.Time
}

// sentinel reports whether this is Azure DevOps' "no end date" marker.
//
// The revisedDate of a work item's most recent revision comes back as
// 9999-01-01, because a revision is modelled as valid until the next one
// supersedes it and the latest has no successor. Taken literally it would
// become a Lamport clock eight thousand years in the future, permanently
// sorting that event above every other one in the entity.
func (t timestamp) sentinel() bool { return t.Year() >= 9000 }

// fieldValue is one value out of a work item's field map, normalised to a
// string. The map is untyped because the fields are: a title is a string, a
// priority is a number, a locked flag is a boolean, and an assignee is an
// object.
type fieldValue struct {
	raw json.RawMessage
}

func (f *fieldValue) UnmarshalJSON(data []byte) error {
	f.raw = append(f.raw[:0], data...)
	return nil
}

// String renders the value the way the field's own type implies: an identity
// as its unique name, a number without a trailing ".0", anything else as
// itself.
func (f fieldValue) String() string {
	if len(f.raw) == 0 || string(f.raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(f.raw, &s); err == nil {
		return s
	}
	var number float64
	if err := json.Unmarshal(f.raw, &number); err == nil {
		return strconv.FormatFloat(number, 'f', -1, 64)
	}
	var b bool
	if err := json.Unmarshal(f.raw, &b); err == nil {
		return strconv.FormatBool(b)
	}
	if id, ok := f.identity(); ok {
		if id.UniqueName != "" {
			return id.UniqueName
		}
		return id.DisplayName
	}
	return strings.TrimSpace(string(f.raw))
}

// identity reads the value as a person, reporting whether it was one.
func (f fieldValue) identity() (Identity, bool) {
	if len(f.raw) == 0 {
		return Identity{}, false
	}
	var id Identity
	if err := json.Unmarshal(f.raw, &id); err != nil || id.Empty() {
		return Identity{}, false
	}
	return id, true
}

func (f fieldValue) time() time.Time {
	var t timestamp
	if err := t.UnmarshalJSON(f.raw); err != nil {
		return time.Time{}
	}
	return t.Time
}

type fields map[string]fieldValue

func (f fields) str(name string) string { return f[name].String() }

func (f fields) identity(name string) Identity {
	id, _ := f[name].identity()
	return id
}

// workItemJSON is one entry of a batch response.
type workItemJSON struct {
	ID        int    `json:"id"`
	Rev       int    `json:"rev"`
	Fields    fields `json:"fields"`
	Relations []struct {
		Rel string `json:"rel"`
		URL string `json:"url"`
	} `json:"relations"`
}

func (w workItemJSON) workItem(t Target) WorkItem {
	item := WorkItem{
		ID:         w.ID,
		Rev:        w.Rev,
		Type:       w.Fields.str(FieldType),
		Title:      w.Fields.str(FieldTitle),
		State:      w.Fields.str(FieldState),
		Reason:     w.Fields.str(FieldReason),
		AreaPath:   w.Fields.str(FieldAreaPath),
		AssignedTo: w.Fields.identity(FieldAssignedTo),
		CreatedBy:  w.Fields.identity(FieldCreatedBy),
		CreatedAt:  w.Fields[FieldCreatedDate].time(),
		ChangedAt:  w.Fields[FieldChangedDate].time(),
		Tags:       SplitTags(w.Fields.str(FieldTags)),
		Iteration:  relativePath(w.Fields.str(FieldIteration), t.Project),
	}
	item.Description = w.Fields.str(BodyField(item.Type))

	// In the order they came back. A JSON-Patch removal addresses a link by its
	// index in this array and has no other way to name one, so the order is
	// load-bearing rather than incidental.
	for _, rel := range w.Relations {
		item.Relations = append(item.Relations, Relation{Rel: rel.Rel, URL: rel.URL})
	}
	return item
}

// BodyField is the field holding a work item's body, which is not the same
// field for every type: a Bug on the Agile process keeps its body in
// ReproSteps, and reading System.Description for one silently imports every
// bug with an empty description.
func BodyField(workItemType string) string {
	if strings.EqualFold(workItemType, "Bug") {
		return FieldReproSteps
	}
	return FieldDescription
}

// SplitTags turns System.Tags into the list it stands for. Azure DevOps stores
// tags as one semicolon-separated string and pads them with spaces.
func SplitTags(s string) []string {
	var tags []string
	for _, tag := range strings.Split(s, ";") {
		if tag = strings.TrimSpace(tag); tag != "" {
			tags = append(tags, tag)
		}
	}
	return tags
}

// JoinTags is SplitTags' inverse, for writing the field back.
func JoinTags(tags []string) string { return strings.Join(tags, "; ") }

// relativePath drops the leading project from an area or iteration path and
// renders the rest with forward slashes, which is how this tracker spells one.
func relativePath(path, project string) string {
	path = strings.ReplaceAll(path, `\`, "/")
	path = strings.TrimPrefix(path, project)
	return strings.Trim(path, "/")
}

// workItemID reads the id off a work item's API URL, which is how a relation
// names its target.
func workItemID(rawURL string) int {
	idx := strings.LastIndex(rawURL, "/")
	if idx < 0 {
		return 0
	}
	id, err := strconv.Atoi(rawURL[idx+1:])
	if err != nil {
		return 0
	}
	return id
}

// updateJSON is one entry of the updates feed.
type updateJSON struct {
	Rev         int       `json:"rev"`
	RevisedBy   Identity  `json:"revisedBy"`
	RevisedDate timestamp `json:"revisedDate"`
	Fields      map[string]struct {
		OldValue fieldValue `json:"oldValue"`
		NewValue fieldValue `json:"newValue"`
	} `json:"fields"`
	Relations struct {
		Added   []relationJSON `json:"added"`
		Removed []relationJSON `json:"removed"`
	} `json:"relations"`
}

type relationJSON struct {
	Rel string `json:"rel"`
	URL string `json:"url"`
}

func (u updateJSON) update() Update {
	update := Update{
		Rev:    u.Rev,
		By:     u.RevisedBy,
		Fields: make(map[string]FieldChange, len(u.Fields)),
	}
	for name, change := range u.Fields {
		update.Fields[name] = FieldChange{Old: change.OldValue.String(), New: change.NewValue.String()}
	}

	// revisedDate is the sentinel on the newest revision, so the revision's
	// own ChangedDate is the honest timestamp where it has one.
	update.At = u.RevisedDate.Time
	if u.RevisedDate.sentinel() || update.At.IsZero() {
		if changed, ok := u.Fields[FieldChangedDate]; ok {
			update.At = changed.NewValue.time()
		}
	}
	if update.At.IsZero() {
		if created, ok := u.Fields[FieldCreatedDate]; ok {
			update.At = created.NewValue.time()
		}
	}

	for _, r := range u.Relations.Added {
		update.RelationsAdded = append(update.RelationsAdded, Relation{Rel: r.Rel, URL: r.URL})
	}
	for _, r := range u.Relations.Removed {
		update.RelationsRemoved = append(update.RelationsRemoved, Relation{Rel: r.Rel, URL: r.URL})
	}
	return update
}

// commentJSON is one entry of the comments response.
type commentJSON struct {
	ID           int       `json:"id"`
	Version      int       `json:"version"`
	Text         string    `json:"text"`
	CreatedBy    Identity  `json:"createdBy"`
	CreatedDate  timestamp `json:"createdDate"`
	ModifiedBy   Identity  `json:"modifiedBy"`
	ModifiedDate timestamp `json:"modifiedDate"`
	IsDeleted    bool      `json:"isDeleted"`
}

func (c commentJSON) comment() Comment {
	return Comment{
		ID:        c.ID,
		Text:      c.Text,
		CreatedBy: c.CreatedBy,
		CreatedAt: c.CreatedDate.Time,
		Deleted:   c.IsDeleted,
	}
}

type commentVersionJSON struct {
	Version      int       `json:"version"`
	Text         string    `json:"text"`
	CreatedBy    Identity  `json:"createdBy"`
	CreatedDate  timestamp `json:"createdDate"`
	ModifiedBy   Identity  `json:"modifiedBy"`
	ModifiedDate timestamp `json:"modifiedDate"`
}

// or returns the first identity that names somebody.
func (i Identity) or(other Identity) Identity {
	if !i.Empty() {
		return i
	}
	return other
}

// String is the identity in the form a commit byline wants. It is deliberately
// not what an event's author becomes: that spelling is part of the bridge's
// specification, because it is hashed into every event id.
func (i Identity) String() string {
	switch {
	case i.DisplayName != "" && i.UniqueName != "":
		return fmt.Sprintf("%s <%s>", i.DisplayName, i.UniqueName)
	case i.DisplayName != "":
		return i.DisplayName
	case i.UniqueName != "":
		return i.UniqueName
	}
	return i.ID
}
