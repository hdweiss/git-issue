package adoissue

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	adoapi "github.com/hdweiss/git-issue/internal/bridge/ado/api"
)

// fakeADO is an in-memory Azure DevOps: enough of the API to write to and read
// back from, with revisions recorded as they are made.
//
// A stateful fake rather than recorded responses, because the write path is
// about what the tracker says *after* a write. Recorded responses can only
// prove that a request was well formed; they cannot prove that pushing a
// change and importing the result converges, which is the property the whole
// three-way comparison rests on.
type fakeADO struct {
	t        *testing.T
	srv      *httptest.Server
	nextID   int
	items    map[int]*fakeItem
	patches  [][]patchOp
	comments int
}

type fakeItem struct {
	id     int
	rev    int
	fields map[string]string
	// relations is the array a JSON-Patch removal indexes into, so the fake
	// keeps it in order and renumbers on removal exactly as the real one does.
	relations []adoapi.Relation
	updates   []fakeUpdate
	comments  []*fakeComment
}

// fakeUpdate is one revision as the updates feed reports it.
type fakeUpdate struct {
	fields  map[string][2]string
	added   []adoapi.Relation
	removed []adoapi.Relation
	by      string
	at      time.Time
}

type fakeComment struct {
	id      int
	text    string
	version int
	deleted bool
	at      time.Time
}

type patchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value"`
}

var (
	reWorkItem = regexp.MustCompile(`/wit/workitems/(\d+)$`)
	reCreate   = regexp.MustCompile(`/wit/workitems/\$(.+)$`)
	reComments = regexp.MustCompile(`/workItems/(\d+)/comments$`)
	reComment  = regexp.MustCompile(`/workItems/(\d+)/comments/(\d+)$`)
	reVersions = regexp.MustCompile(`/workItems/(\d+)/comments/(\d+)/versions$`)
	reUpdates  = regexp.MustCompile(`/workItems/(\d+)/updates$`)
)

func newFakeADO(t *testing.T) *fakeADO {
	f := &fakeADO{t: t, nextID: 100, items: map[int]*fakeItem{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeADO) target() adoapi.Target {
	target, err := adoapi.ParseTarget(f.srv.URL + "/DefaultCollection/MyProj/_git/repo")
	if err != nil {
		f.t.Fatal(err)
	}
	return target
}

func (f *fakeADO) client() *adoapi.Client { return adoapi.New("test-token") }

func (f *fakeADO) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	body, _ := io.ReadAll(r.Body)
	path := r.URL.Path

	switch {
	case reVersions.MatchString(path):
		m := reVersions.FindStringSubmatch(path)
		f.writeVersions(w, atoi(m[1]), atoi(m[2]))

	case reComment.MatchString(path):
		m := reComment.FindStringSubmatch(path)
		f.commentWrite(w, r.Method, atoi(m[1]), atoi(m[2]), body)

	case reComments.MatchString(path):
		m := reComments.FindStringSubmatch(path)
		if r.Method == http.MethodPost {
			f.addComment(w, atoi(m[1]), body)
			return
		}
		f.writeComments(w, atoi(m[1]))

	case reUpdates.MatchString(path):
		m := reUpdates.FindStringSubmatch(path)
		f.writeUpdates(w, atoi(m[1]), r.URL.Query().Get("$skip"))

	case strings.HasSuffix(path, "/wit/workitemsbatch"):
		f.writeBatch(w, body)

	case reCreate.MatchString(path) && r.Method == http.MethodPost:
		f.create(w, reCreate.FindStringSubmatch(path)[1], body)

	case reWorkItem.MatchString(path) && r.Method == http.MethodPatch:
		f.patch(w, atoi(reWorkItem.FindStringSubmatch(path)[1]), body)

	default:
		f.t.Errorf("unexpected %s %s", r.Method, path)
		w.WriteHeader(http.StatusNotFound)
	}
}

// file seeds a work item as though somebody had created it upstream.
func (f *fakeADO) file(fields map[string]string, links ...adoapi.Relation) int {
	f.nextID++
	id := f.nextID
	item := &fakeItem{id: id, rev: 1, fields: map[string]string{}, relations: links}
	for k, v := range fields {
		item.fields[k] = v
	}
	if item.fields[adoapi.FieldCreatedDate] == "" {
		item.fields[adoapi.FieldCreatedDate] = "2026-01-01T00:00:00Z"
	}
	if item.fields[adoapi.FieldCreatedBy] == "" {
		item.fields[adoapi.FieldCreatedBy] = "them@corp.example"
	}
	item.fields[adoapi.FieldChangedDate] = item.fields[adoapi.FieldCreatedDate]

	first := fakeUpdate{
		fields: map[string][2]string{},
		added:  links,
		by:     item.fields[adoapi.FieldCreatedBy],
		at:     parseTime(item.fields[adoapi.FieldCreatedDate]),
	}
	for k, v := range item.fields {
		first.fields[k] = [2]string{"", v}
	}
	item.updates = append(item.updates, first)
	f.items[id] = item
	return id
}

// link is the relation to a work item this fake holds, as a patch spells one.
func (f *fakeADO) link(rel string, id int) adoapi.Relation {
	return adoapi.Relation{Rel: rel, URL: f.target().WorkItemURL(id)}
}

// reciprocals is the far half of every link type this fake knows.
//
// Azure DevOps stores one edge on *both* work items, each spelled from its own
// point of view, and it writes the far half itself when a patch writes the near
// one. A fake that recorded only the near half would let the import's rule about
// forward halves pass for the wrong reason, and would never produce the two
// members a symmetric link really leaves behind.
var reciprocals = map[string]string{
	adoapi.RelParent:      adoapi.RelChild,
	adoapi.RelChild:       adoapi.RelParent,
	adoapi.RelPredecessor: adoapi.RelSuccessor,
	adoapi.RelSuccessor:   adoapi.RelPredecessor,
	adoapi.RelDuplicateOf: adoapi.RelDuplicate,
	adoapi.RelDuplicate:   adoapi.RelDuplicateOf,
	adoapi.RelRelated:     adoapi.RelRelated,
}

// mirrorLink writes the far half of a link, with the revision it earns there.
// A link to a work item this fake does not hold has no far half to write.
func (f *fakeADO) mirrorLink(near int, rel adoapi.Relation, added bool) {
	far, ok := f.items[workItemID(rel.URL)]
	if !ok {
		return
	}
	back, ok := reciprocals[rel.Rel]
	if !ok {
		return
	}

	u := fakeUpdate{fields: map[string][2]string{}, by: "system@corp.example"}
	other := adoapi.Relation{Rel: back, URL: f.target().WorkItemURL(near)}
	if added {
		far.relations = append(far.relations, other)
		u.added = []adoapi.Relation{other}
	} else {
		for i, r := range far.relations {
			if r.Rel != back || workItemID(r.URL) != near {
				continue
			}
			far.relations = append(far.relations[:i], far.relations[i+1:]...)
			u.removed = []adoapi.Relation{other}
			break
		}
		if len(u.removed) == 0 {
			return
		}
	}

	far.rev++
	u.at = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(far.rev) * time.Hour)
	far.fields[adoapi.FieldChangedDate] = u.at.Format(time.RFC3339)
	far.updates = append(far.updates, u)
}

func (f *fakeADO) create(w http.ResponseWriter, workItemType string, body []byte) {
	var ops []patchOp
	if err := json.Unmarshal(body, &ops); err != nil {
		f.t.Fatalf("create body: %v", err)
	}
	f.patches = append(f.patches, ops)

	fields := map[string]string{adoapi.FieldType: workItemType}
	var links []adoapi.Relation
	for _, op := range ops {
		if strings.HasPrefix(op.Path, "/relations/") {
			links = append(links, relationValue(f.t, op))
			continue
		}
		fields[strings.TrimPrefix(op.Path, "/fields/")] = fmt.Sprint(op.Value)
	}
	fields[adoapi.FieldCreatedBy] = "pusher@corp.example"
	fields[adoapi.FieldCreatedDate] = "2026-02-01T00:00:00Z"
	id := f.file(fields, links...)
	for _, rel := range links {
		f.mirrorLink(id, rel, true)
	}
	f.writeItem(w, f.items[id])
}

// relationValue reads a link out of a patch operation's value.
func relationValue(t *testing.T, op patchOp) adoapi.Relation {
	t.Helper()
	encoded, err := json.Marshal(op.Value)
	if err != nil {
		t.Fatalf("relation value: %v", err)
	}
	var rel adoapi.Relation
	if err := json.Unmarshal(encoded, &rel); err != nil {
		t.Fatalf("relation value %s: %v", encoded, err)
	}
	if rel.Rel == "" || rel.URL == "" {
		t.Fatalf("a link was written as %s; a relation needs both a rel and a url", encoded)
	}
	return rel
}

func (f *fakeADO) patch(w http.ResponseWriter, id int, body []byte) {
	var ops []patchOp
	if err := json.Unmarshal(body, &ops); err != nil {
		f.t.Fatalf("patch body: %v", err)
	}
	f.patches = append(f.patches, ops)

	item, ok := f.items[id]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	update := fakeUpdate{fields: map[string][2]string{}, by: "pusher@corp.example"}
	for _, op := range ops {
		switch {
		case op.Op == "test":
			// The concurrency check the real API makes. A stale rev fails the
			// whole document rather than overwriting.
			if op.Path == "/rev" && fmt.Sprint(op.Value) != strconv.Itoa(item.rev) {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"message":"stale rev","typeKey":"WorkItemUpdateFailure"}`))
				return
			}

		case op.Path == "/relations/-":
			rel := relationValue(f.t, op)
			item.relations = append(item.relations, rel)
			update.added = append(update.added, rel)

		case strings.HasPrefix(op.Path, "/relations/"):
			// By index, and the array closes up behind it — which is the whole
			// reason a document has to remove in descending order.
			i := atoi(strings.TrimPrefix(op.Path, "/relations/"))
			if i < 0 || i >= len(item.relations) {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, `{"message":"no relation at index %d","typeKey":"WorkItemUpdateFailure"}`, i)
				return
			}
			update.removed = append(update.removed, item.relations[i])
			item.relations = append(item.relations[:i], item.relations[i+1:]...)

		default:
			name := strings.TrimPrefix(op.Path, "/fields/")
			value := fmt.Sprint(op.Value)
			update.fields[name] = [2]string{item.fields[name], value}
			item.fields[name] = value
		}
	}

	item.rev++
	update.at = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(item.rev) * time.Hour)
	stamp := update.at.Format(time.RFC3339)
	update.fields[adoapi.FieldChangedDate] = [2]string{item.fields[adoapi.FieldChangedDate], stamp}
	item.fields[adoapi.FieldChangedDate] = stamp
	item.updates = append(item.updates, update)

	// The far half of every edge this document touched, which Azure DevOps
	// writes itself.
	for _, rel := range update.added {
		f.mirrorLink(id, rel, true)
	}
	for _, rel := range update.removed {
		f.mirrorLink(id, rel, false)
	}

	f.writeItem(w, item)
}

func (f *fakeADO) addComment(w http.ResponseWriter, id int, body []byte) {
	var payload struct {
		Text string `json:"text"`
	}
	json.Unmarshal(body, &payload)

	item := f.items[id]
	f.comments++
	c := &fakeComment{
		id: f.comments, text: payload.Text, version: 1,
		at: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(f.comments) * time.Hour),
	}
	item.comments = append(item.comments, c)
	fmt.Fprintf(w, `{"id":%d}`, c.id)
}

func (f *fakeADO) commentWrite(w http.ResponseWriter, method string, id, comment int, body []byte) {
	item := f.items[id]
	for _, c := range item.comments {
		if c.id != comment {
			continue
		}
		switch method {
		case http.MethodPatch:
			var payload struct {
				Text string `json:"text"`
			}
			json.Unmarshal(body, &payload)
			c.text = payload.Text
			c.version++
		case http.MethodDelete:
			c.deleted = true
		}
		w.Write([]byte(`{}`))
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func (f *fakeADO) writeBatch(w http.ResponseWriter, body []byte) {
	var payload struct {
		IDs []int `json:"ids"`
	}
	json.Unmarshal(body, &payload)

	var items []string
	for _, id := range payload.IDs {
		if item, ok := f.items[id]; ok {
			items = append(items, f.itemJSON(item))
		}
	}
	fmt.Fprintf(w, `{"count":%d,"value":[%s]}`, len(items), strings.Join(items, ","))
}

func (f *fakeADO) writeItem(w http.ResponseWriter, item *fakeItem) {
	w.Write([]byte(f.itemJSON(item)))
}

func (f *fakeADO) itemJSON(item *fakeItem) string {
	fields, _ := json.Marshal(item.fields)
	relations, _ := json.Marshal(relationsJSON(item.relations))
	return fmt.Sprintf(`{"id":%d,"rev":%d,"fields":%s,"relations":%s}`,
		item.id, item.rev, fields, relations)
}

func (f *fakeADO) writeUpdates(w http.ResponseWriter, id int, skip string) {
	if skip != "0" {
		w.Write([]byte(`{"count":0,"value":[]}`))
		return
	}
	item := f.items[id]
	var out []string
	for i, u := range item.updates {
		fields := map[string]map[string]string{}
		for name, change := range u.fields {
			entry := map[string]string{"newValue": change[1]}
			if change[0] != "" {
				entry["oldValue"] = change[0]
			}
			fields[name] = entry
		}
		encoded, _ := json.Marshal(fields)
		relations, _ := json.Marshal(map[string]any{
			"added":   relationsJSON(u.added),
			"removed": relationsJSON(u.removed),
		})
		out = append(out, fmt.Sprintf(
			`{"rev":%d,"revisedBy":{"uniqueName":%q},"revisedDate":%q,"fields":%s,"relations":%s}`,
			i+1, u.by, u.at.Format(time.RFC3339), encoded, relations))
	}
	fmt.Fprintf(w, `{"count":%d,"value":[%s]}`, len(out), strings.Join(out, ","))
}

// relationsJSON spells links the way Azure DevOps does on the wire, which is
// the same shape in a work item and in a revision.
func relationsJSON(rels []adoapi.Relation) []map[string]string {
	out := make([]map[string]string, 0, len(rels))
	for _, r := range rels {
		out = append(out, map[string]string{"rel": r.Rel, "url": r.URL})
	}
	return out
}

func (f *fakeADO) writeComments(w http.ResponseWriter, id int) {
	item := f.items[id]
	var out []string
	for _, c := range item.comments {
		out = append(out, fmt.Sprintf(
			`{"id":%d,"version":%d,"text":%q,"createdBy":{"uniqueName":"pusher@corp.example"},"createdDate":%q,"isDeleted":%t}`,
			c.id, c.version, c.text, c.at.Format(time.RFC3339), c.deleted))
	}
	fmt.Fprintf(w, `{"totalCount":%d,"count":%d,"comments":[%s]}`, len(out), len(out), strings.Join(out, ","))
}

func (f *fakeADO) writeVersions(w http.ResponseWriter, id, comment int) {
	item := f.items[id]
	for _, c := range item.comments {
		if c.id != comment {
			continue
		}
		var out []string
		for v := 1; v <= c.version; v++ {
			// Only the newest version's text is kept, which is enough: the
			// import reads the first version as the original and the rest as
			// edits, and this fake never needs to prove more than that.
			out = append(out, fmt.Sprintf(
				`{"version":%d,"text":%q,"createdBy":{"uniqueName":"pusher@corp.example"},"createdDate":%q}`,
				v, c.text, c.at.Format(time.RFC3339)))
		}
		fmt.Fprintf(w, `{"count":%d,"value":[%s]}`, len(out), strings.Join(out, ","))
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

// fieldOf is what the fake currently holds for one field, for assertions.
func (f *fakeADO) fieldOf(id int, name string) string { return f.items[id].fields[name] }

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }
func parseTime(s string) time.Time {
	at, _ := time.Parse(time.RFC3339, s)
	return at
}
