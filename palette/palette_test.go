package palette_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/teb-ooo/playground-go/palette"
)

func TestActionExtShapeMatchesTheWebTag(t *testing.T) {
	ext := palette.Action{
		Title: "Disable {username}", Group: "Users", When: &palette.When{Route: "/users", Needs: "selection"},
		Args: map[string]string{"id": "selection.id"}, ConfirmText: "Disable {username}?", After: &palette.After{Invalidate: []string{"/api/users"}},
	}.Ext()
	b, _ := json.Marshal(ext)
	want := `{"x-palette":{"after":{"invalidate":["/api/users"]},"args":{"id":"selection.id"},"confirm":"Disable {username}?","group":"Users","title":"Disable {username}","when":{"route":"/users","needs":"selection"}}}`
	if string(b) != want {
		t.Errorf("\n got %s\nwant %s", b, want)
	}
	src, _ := json.Marshal(palette.Source{Group: "Users", Title: "{username}", Route: "/users?id={id}", MinChars: 3}.Ext())
	if string(src) != `{"x-palette":{"source":{"group":"Users","minChars":3,"route":"/users?id={id}","title":"{username}"}}}` {
		t.Errorf("source: %s", src)
	}
	none, _ := json.Marshal(palette.None("has its own screen"))
	if string(none) != `{"x-palette":false,"x-palette-reason":"has its own screen"}` {
		t.Errorf("none: %s", none)
	}
}

func TestMalformedTagsPanicAtConstruction(t *testing.T) {
	for name, f := range map[string]func(){
		"no title":        func() { palette.Action{Group: "G"}.Ext() },
		"no group":        func() { palette.Action{Title: "T"}.Ext() },
		"bad needs":       func() { palette.Action{Title: "T", Group: "G", When: &palette.When{Needs: "row"}}.Ext() },
		"prompt arg":      func() { palette.Action{Title: "T", Group: "G", Args: map[string]string{"x": "prompt"}}.Ext() },
		"empty arg":       func() { palette.Action{Title: "T", Group: "G", Args: map[string]string{"x": ""}}.Ext() },
		"contradiction":   func() { palette.Action{Title: "T", Group: "G", Confirm: true, NoConfirm: true}.Ext() },
		"source no route": func() { palette.Source{Group: "G", Title: "T"}.Ext() },
		"none no reason":  func() { palette.None(" ") },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: no panic", name)
				}
			}()
			f()
		}()
	}
}

func api(ops ...huma.Operation) huma.API {
	a := humago.New(http.NewServeMux(), huma.DefaultConfig("t", "1"))
	for _, op := range ops {
		huma.Register(a, op, func(context.Context, *struct{}) (*struct{ Body struct{} }, error) { return nil, nil })
	}
	return a
}

type rec struct {
	testing.TB
	msgs []string
}

func (r *rec) Helper()                   {}
func (r *rec) Errorf(f string, a ...any) { r.msgs = append(r.msgs, fmt.Sprintf(f, a...)) }

func op(id, method, path string, ext map[string]any) huma.Operation {
	return huma.Operation{OperationID: id, Method: method, Path: path, Summary: "s", Description: "d", Extensions: ext}
}

func TestCheckFindsGapsAndMisuse(t *testing.T) {
	act := palette.Action{Title: "Do it", Group: "G"}.Ext()
	src := palette.Source{Group: "G", Title: "{n}", Route: "/x"}.Ext()
	good := api(op("list-things", "GET", "/api/things", src), op("get-thing", "GET", "/api/things/{id}", nil),
		op("create-thing", "POST", "/api/things", act), op("remove-thing", "DELETE", "/api/things/{id}", palette.None("needs a typed confirmation")))
	r := &rec{}
	palette.Check(r, good)
	if len(r.msgs) != 0 {
		t.Fatalf("good document: %v", r.msgs)
	}
	bad := api(
		op("update-thing", "PUT", "/api/things/{id}", nil),                                        // untagged mutation
		op("list-bad", "GET", "/api/bad", act),                                                    // action on a GET
		op("post-source", "POST", "/api/s", src),                                                  // source on a POST
		op("no-reason", "POST", "/api/n", map[string]any{palette.Key: false}),                     // false without a reason
		op("true-tag", "POST", "/api/tt", map[string]any{palette.Key: true}),                      // not a tag
		op("hand-written", "POST", "/api/h", map[string]any{palette.Key: map[string]any{"a": 1}}), // neither
	)
	r = &rec{}
	palette.Check(r, bad)
	joined := strings.Join(r.msgs, "\n")
	for _, want := range []string{"update-thing", "has no x-palette tag", "an action must change something", "a source must be a GET", "needs a reason", "is not a tag", "neither an action"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	r = &rec{}
	palette.Check(r, bad, "update-thing", "list-bad", "post-source", "no-reason", "true-tag", "hand-written")
	if len(r.msgs) != 0 {
		t.Errorf("exempt operations are skipped: %v", r.msgs)
	}
}

// The tag reaches the served OpenAPI document as an operation-level x-palette key.
func TestTagIsInTheOpenAPIDocument(t *testing.T) {
	a := api(op("create-thing", "POST", "/api/things", palette.Action{Title: "Make a thing", Group: "Things"}.Ext()),
		op("remove-thing", "DELETE", "/api/things/{id}", palette.None("typed confirmation")))
	b, err := json.Marshal(a.OpenAPI())
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]map[string]any `json:"paths"`
	}
	_ = json.Unmarshal(b, &doc)
	if tag, _ := doc.Paths["/api/things"]["post"]["x-palette"].(map[string]any); tag["title"] != "Make a thing" {
		t.Errorf("post tag: %v", doc.Paths["/api/things"]["post"]["x-palette"])
	}
	if v, ok := doc.Paths["/api/things/{id}"]["delete"]["x-palette"]; !ok || v != false {
		t.Errorf("delete tag: %v %v", v, ok)
	}
}
