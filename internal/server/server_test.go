package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/excavador/homebox-mcp/internal/homebox"
)

// MCP requires structuredContent to be an object. Several HomeBox endpoints
// answer with a top-level array, and returning one straight through makes the
// client reject the response as malformed -- an error that names schema
// validation, not the tool that produced it.
//
// This shipped: /entities/tree, /tags, /entities/{id}/path and
// /entities/fields were all broken, while /entities and /groups/statistics
// worked. Testing only the two object-shaped endpoints is what let it out.
func TestCallAlwaysReturnsAnObject(t *testing.T) {
	cases := map[string]string{
		"top-level array":  `[{"name":"Attic"},{"name":"Garage"}]`,
		"array of strings": `["one","two"]`,
		"object":           `{"total":2,"items":[]}`,
		"empty array":      `[]`,
		"null":             `null`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()

			_, out, err := call(context.Background(), homebox.New(srv.URL, "t"), http.MethodGet, "/anything", nil, nil)
			if err != nil {
				t.Fatalf("call: %v", err)
			}

			if _, ok := out.(map[string]any); !ok {
				t.Fatalf("structuredContent is %T, must be an object; MCP rejects anything else", out)
			}
		})
	}
}

// A failing request must surface as an error rather than as an empty result
// the model would read as "there is nothing here".
func TestCallPropagatesFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"nope"}`))
	}))
	defer srv.Close()

	if _, _, err := call(context.Background(), homebox.New(srv.URL, "bad"), http.MethodGet, "/anything", nil, nil); err == nil {
		t.Fatal("a 401 must be an error, not an empty result")
	}
}

// A write must actually send its body and use the right method -- the whole
// point of the CRUD tools, and the easiest thing to get silently wrong.
func TestWritesSendMethodAndBody(t *testing.T) {
	var gotMethod, gotPath, gotBody string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotMethod, gotPath, gotBody = r.Method, r.URL.Path, string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"new"}`))
	}))
	defer srv.Close()

	c := homebox.New(srv.URL, "t")

	if _, _, err := call(context.Background(), c, http.MethodPost, "/entities", nil,
		map[string]any{"name": "Drill"}); err != nil {
		t.Fatalf("call: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}

	if gotPath != "/api/v1/entities" {
		t.Errorf("path = %s, want /api/v1/entities -- the /api basePath is easy to drop", gotPath)
	}

	if !strings.Contains(gotBody, `"name":"Drill"`) {
		t.Errorf("body = %q, want the object we passed", gotBody)
	}
}

// A DELETE answering 204 with no body must read as success, not as a decode
// failure.
func TestEmptyBodyIsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	_, out, err := call(context.Background(), homebox.New(srv.URL, "t"), http.MethodDelete, "/tags/x", nil, nil)
	if err != nil {
		t.Fatalf("204 must be success, got: %v", err)
	}

	if _, ok := out.(map[string]any); !ok {
		t.Fatalf("structuredContent is %T, must be an object", out)
	}
}

// Every tool must declare annotations, because that is how a client knows
// which calls destroy something before it makes one.
// connect drives the real registered tools over an in-memory transport, with
// hb standing in for HomeBox. Testing call() directly would only prove the
// helper works; the bugs have all been in how a tool wires itself to it.
func connect(t *testing.T, hb http.Handler) *mcp.ClientSession {
	t.Helper()

	srv := httptest.NewServer(hb)
	t.Cleanup(srv.Close)

	ct, st := mcp.NewInMemoryTransports()

	ss, err := New(homebox.New(srv.URL, "t"), "test").Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}

	t.Cleanup(func() { _ = ss.Close() })

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).
		Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	t.Cleanup(func() { _ = cs.Close() })

	return cs
}

// Annotations are how a client decides what it may call without asking. An
// unannotated tool defaults to the cautious reading, so a missing annotation
// is a silent loss of function rather than an error.
func TestEveryToolIsAnnotated(t *testing.T) {
	cs := connect(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))

	tools, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	if len(tools.Tools) < 30 {
		t.Fatalf("got %d tools, want the full API surface -- did a group fail to register?", len(tools.Tools))
	}

	var reads, writes int

	for _, tool := range tools.Tools {
		a := tool.Annotations
		if a == nil {
			t.Errorf("%s: no annotations", tool.Name)
			continue
		}

		switch {
		case a.ReadOnlyHint:
			reads++
		case a.DestructiveHint != nil:
			writes++
		default:
			t.Errorf("%s: neither read-only nor destructive-hinted", tool.Name)
		}

		if a.ReadOnlyHint && a.DestructiveHint != nil && *a.DestructiveHint {
			t.Errorf("%s: read-only and destructive at once", tool.Name)
		}
	}

	if reads == 0 || writes == 0 {
		t.Errorf("reads=%d writes=%d, want both -- the split is the point", reads, writes)
	}
}

// HomeBox decodes the request body unconditionally on /duplicate and answers
// 500 when it is absent, so the tool has to send an object even though it has
// nothing to say. Sending no body looks correct and fails only against a real
// server.
func TestDuplicateSendsABody(t *testing.T) {
	var got string

	cs := connect(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)

		if len(b) == 0 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"Unknown Error"}`))

			return
		}

		_, _ = w.Write([]byte(`{"id":"copy"}`))
	}))

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "homebox_duplicate_entity",
		Arguments: map[string]any{"id": "abc"},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}

	if res.IsError {
		t.Fatalf("duplicate failed: %v", res.Content)
	}

	if got != "{}" {
		t.Errorf("body = %q, want {} -- HomeBox 500s without one", got)
	}
}

// The reporting endpoints answer text/csv. Decoding that as JSON fails on the
// first byte of the header row, and the error names a decode problem rather
// than a wrong expectation, so it reads as a broken server.
func TestCSVReportIsNotDecodedAsJSON(t *testing.T) {
	cs := connect(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/csv")
		_, _ = w.Write([]byte("Name,Quantity\nDrill,1\n"))
	}))

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "homebox_bill_of_materials", Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}

	if res.IsError {
		t.Fatalf("report failed: %v", res.Content)
	}

	csv, ok := res.StructuredContent.(map[string]any)["csv"].(string)
	if !ok {
		t.Fatalf("structuredContent = %#v, want a csv string", res.StructuredContent)
	}

	if !strings.Contains(csv, "Drill,1") {
		t.Errorf("csv = %q, want the rows the server sent", csv)
	}
}

// homeboxStub imitates the three behaviours that made the write tools lie:
// create keeps only name/entityTypeId/parentId, GET never reports the parent,
// and PATCH accepts everything while writing nothing.
type homeboxStub struct {
	entity  map[string]any
	parent  string
	tags    []string
	puts    []map[string]any
	patches int
	posts   int
}

func (h *homeboxStub) handler(t *testing.T) http.Handler {
	t.Helper()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodPost:
			h.posts++

			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)

			h.entity = map[string]any{"id": "e1", "name": in["name"], "description": ""}
			if p, ok := in["parentId"].(string); ok {
				h.parent = p
			}

			_ = json.NewEncoder(w).Encode(h.entity)

		case r.Method == http.MethodPatch:
			h.patches++
			_ = json.NewEncoder(w).Encode(h.entity) // accepted, nothing written

		case r.Method == http.MethodPut:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			h.puts = append(h.puts, in)

			if p, ok := in["parentId"].(string); ok {
				h.parent = p
			} else {
				h.parent = "" // a PUT without parentId unparents, as HomeBox does
			}

			h.tags = nil // and one without tagIds strips the tags
			if ids, ok := in["tagIds"].([]any); ok {
				for _, id := range ids {
					if str, ok := id.(string); ok {
						h.tags = append(h.tags, str)
					}
				}
			}

			for k, v := range in {
				h.entity[k] = v
			}

			_ = json.NewEncoder(w).Encode(h.entity)

		case strings.HasSuffix(r.URL.Path, "/path"):
			path := []any{}
			if h.parent != "" {
				path = append(path, map[string]any{"id": h.parent, "name": "Box"})
			}

			path = append(path, map[string]any{"id": "e1", "name": h.entity["name"]})
			_ = json.NewEncoder(w).Encode(path)

		default: // GET one entity -- parent and tags come back as objects, not ids
			out := map[string]any{}
			for k, v := range h.entity {
				out[k] = v
			}

			if h.parent != "" {
				out["parent"] = map[string]any{"id": h.parent, "name": "Box"}
			}

			tags := []any{}
			for _, t := range h.tags {
				tags = append(tags, map[string]any{"id": t, "name": t})
			}

			out["tags"] = tags
			_ = json.NewEncoder(w).Encode(out)
		}
	})
}

// Create accepts a full body and HomeBox keeps three fields of it, answering
// 201 either way. The tool has to finish the job or the data is silently lost.
func TestCreateAppliesTheWholeBody(t *testing.T) {
	h := &homeboxStub{}
	cs := connect(t, h.handler(t))

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "homebox_create_entity",
		Arguments: map[string]any{"body": map[string]any{
			"name": "Switch", "entityTypeId": "t1", "parentId": "loc1", "manufacturer": "TP-Link",
		}},
	})
	if err != nil || res.IsError {
		t.Fatalf("create: %v %v", err, res)
	}

	if len(h.puts) != 1 {
		t.Fatalf("%d follow-up writes, want 1 -- the dropped fields were never applied", len(h.puts))
	}

	if h.puts[0]["manufacturer"] != "TP-Link" {
		t.Errorf("manufacturer = %v, want TP-Link", h.puts[0]["manufacturer"])
	}

	if h.parent != "loc1" {
		t.Errorf("parent = %q, want loc1 -- the follow-up PUT unparented it", h.parent)
	}
}

// The second write is not free, so it should only happen when the body has
// something create would have dropped.
func TestCreateSkipsTheSecondWriteWhenNothingWouldBeLost(t *testing.T) {
	h := &homeboxStub{}
	cs := connect(t, h.handler(t))

	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "homebox_create_entity",
		Arguments: map[string]any{"body": map[string]any{"name": "Box", "entityTypeId": "t1"}},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if len(h.puts) != 0 {
		t.Errorf("%d follow-up writes, want 0", len(h.puts))
	}
}

// Read-modify-write is the obvious way to use a PUT API, and here it silently
// moved things to the root, because GET does not return the parent.
func TestUpdateKeepsTheParent(t *testing.T) {
	h := &homeboxStub{entity: map[string]any{"id": "e1", "name": "Vacuum"}, parent: "floor1"}
	cs := connect(t, h.handler(t))

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "homebox_update_entity",
		Arguments: map[string]any{"id": "e1", "body": map[string]any{
			"id": "e1", "name": "Vacuum", "description": "changed",
		}},
	})
	if err != nil || res.IsError {
		t.Fatalf("update: %v %v", err, res)
	}

	if h.parent != "floor1" {
		t.Errorf("parent = %q, want floor1 -- an update moved the entity to the root", h.parent)
	}
}

// Carrying the parent forward must not stop a caller from deliberately moving
// something.
func TestUpdateHonoursAnExplicitParent(t *testing.T) {
	h := &homeboxStub{entity: map[string]any{"id": "e1", "name": "Vacuum"}, parent: "floor1"}
	cs := connect(t, h.handler(t))

	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "homebox_update_entity",
		Arguments: map[string]any{"id": "e1", "body": map[string]any{
			"name": "Vacuum", "parentId": "floor2",
		}},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	if h.parent != "floor2" {
		t.Errorf("parent = %q, want floor2 -- an explicit move was ignored", h.parent)
	}
}

// HomeBox's PATCH answers 200 and writes nothing, so a model told to prefer it
// would make changes that quietly evaporate.
func TestPatchActuallyWrites(t *testing.T) {
	h := &homeboxStub{
		entity: map[string]any{"id": "e1", "name": "Vacuum", "manufacturer": "Dreame"},
		parent: "floor1",
	}
	cs := connect(t, h.handler(t))

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "homebox_patch_entity",
		Arguments: map[string]any{"id": "e1", "body": map[string]any{"description": "patched"}},
	})
	if err != nil || res.IsError {
		t.Fatalf("patch: %v %v", err, res)
	}

	if h.patches != 0 {
		t.Errorf("issued %d PATCH requests -- HomeBox ignores those", h.patches)
	}

	if h.entity["description"] != "patched" {
		t.Errorf("description = %v, want patched -- the change was not written", h.entity["description"])
	}

	if h.entity["manufacturer"] != "Dreame" {
		t.Errorf("manufacturer = %v, want Dreame -- a partial change clobbered another field", h.entity["manufacturer"])
	}

	if h.parent != "floor1" {
		t.Errorf("parent = %q, want floor1 -- a partial change moved the entity", h.parent)
	}
}

// Tags are the same trap as the parent one level out: read gives objects,
// write takes ids, and a PUT without tagIds strips them. This stripped the
// tags off twenty-two items during a real bulk load before it was noticed.
func TestUpdateKeepsTheTags(t *testing.T) {
	h := &homeboxStub{
		entity: map[string]any{"id": "e1", "name": "Vacuum"},
		parent: "floor1", tags: []string{"tag-iot"},
	}
	cs := connect(t, h.handler(t))

	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "homebox_update_entity",
		Arguments: map[string]any{"id": "e1", "body": map[string]any{
			"id": "e1", "name": "Vacuum", "description": "changed",
		}},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	if len(h.tags) != 1 || h.tags[0] != "tag-iot" {
		t.Errorf("tags = %v, want [tag-iot] -- an update stripped them", h.tags)
	}
}

// Preserving tags must not stop a caller from setting them.
func TestUpdateHonoursExplicitTags(t *testing.T) {
	h := &homeboxStub{entity: map[string]any{"id": "e1", "name": "Vacuum"}, tags: []string{"old"}}
	cs := connect(t, h.handler(t))

	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "homebox_update_entity",
		Arguments: map[string]any{"id": "e1", "body": map[string]any{
			"name": "Vacuum", "tagIds": []any{"new"},
		}},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	if len(h.tags) != 1 || h.tags[0] != "new" {
		t.Errorf("tags = %v, want [new]", h.tags)
	}
}
