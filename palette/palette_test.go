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
		"no title":            func() { palette.Action{Group: "G"}.Ext() },
		"no group":            func() { palette.Action{Title: "T"}.Ext() },
		"bad needs":           func() { palette.Action{Title: "T", Group: "G", When: &palette.When{Needs: "row"}}.Ext() },
		"form without prompt": func() { palette.Action{Title: "T", Group: "G", Form: &palette.Form{Submit: "Go"}}.Ext() },
		"form without label": func() {
			palette.Action{Title: "T", Group: "G", Args: map[string]string{"x": "prompt"}, Form: &palette.Form{}}.Ext()
		},
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

// when.field, when.differs and role (@teb-ooo/ui 0.77 to 0.79): the tag shape the web side reads, and the same refusals as its paletteProblems.
func TestWhenFieldDiffersAndRole(t *testing.T) {
	a := palette.Action{
		Title: "Enable {username}", Group: "Users", Role: "admin",
		When: &palette.When{Route: "/users", Field: map[string]any{"status": "disabled", "kind": []string{"a", "b"}, "n": 3, "ok": true}, Differs: map[string]string{"id": "user.subject", "p": "route.code"}},
		Args: map[string]string{"id": "selection.id"},
	}
	b, _ := json.Marshal(a.Ext())
	var tag struct {
		P struct {
			Role string `json:"role"`
			When struct {
				Field   map[string]any    `json:"field"`
				Differs map[string]string `json:"differs"`
			} `json:"when"`
		} `json:"x-palette"`
	}
	if err := json.Unmarshal(b, &tag); err != nil {
		t.Fatal(err)
	}
	if tag.P.Role != "admin" || tag.P.When.Field["status"] != "disabled" || tag.P.When.Differs["id"] != "user.subject" || len(tag.P.When.Field) != 4 {
		t.Fatalf("shape: %s", b)
	}
	// a source's role travels beside source, not inside it
	sb, _ := json.Marshal(palette.Source{Group: "G", Title: "{n}", Route: "/x", Role: "owner"}.Ext())
	if string(sb) != `{"x-palette":{"role":"owner","source":{"group":"G","route":"/x","title":"{n}"}}}` {
		t.Errorf("source role: %s", sb)
	}
	for name, f := range map[string]func(){
		"empty field list": func() {
			palette.Action{Title: "T", Group: "G", When: &palette.When{Field: map[string]any{"s": []string{}}}}.Ext()
		},
		"object field value": func() {
			palette.Action{Title: "T", Group: "G", When: &palette.When{Field: map[string]any{"s": map[string]int{"a": 1}}}}.Ext()
		},
		"bad differs": func() {
			palette.Action{Title: "T", Group: "G", When: &palette.When{Differs: map[string]string{"id": "subject"}}}.Ext()
		},
		"bad role":        func() { palette.Action{Title: "T", Group: "G", Role: "root"}.Ext() },
		"bad source role": func() { palette.Source{Group: "G", Title: "T", Route: "/x", Role: "everyone"}.Ext() },
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

// Check reads the tags as JSON, like the web side, and refuses the same malformed conditions.
func TestCheckRefusesMalformedConditions(t *testing.T) {
	mk := func(tag map[string]any) huma.API {
		return api(op("do-thing", "POST", "/api/things", map[string]any{palette.Key: tag}))
	}
	for name, tag := range map[string]map[string]any{
		"empty field list": {"title": "T", "group": "G", "when": map[string]any{"field": map[string]any{"s": []any{}}}},
		"bad differs":      {"title": "T", "group": "G", "when": map[string]any{"differs": map[string]any{"id": "subject"}}},
		"bad role":         {"title": "T", "group": "G", "role": "root"},
		"bad source role":  {"source": map[string]any{"group": "G", "title": "T", "route": "/x"}, "role": "all"},
	} {
		method := "POST"
		a := mk(tag)
		if _, isSource := tag["source"]; isSource {
			method = "GET"
			a = api(op("list-things", method, "/api/things", map[string]any{palette.Key: tag}))
		}
		r := &rec{}
		palette.Check(r, a)
		if len(r.msgs) == 0 {
			t.Errorf("%s: Check accepted it", name)
		}
	}
	good := mk(map[string]any{"title": "T", "group": "G", "role": "admin", "when": map[string]any{"field": map[string]any{"s": []any{"a", 1.0, true}}, "differs": map[string]any{"id": "user.subject"}}})
	r := &rec{}
	palette.Check(r, good)
	if len(r.msgs) != 0 {
		t.Errorf("a valid tag: %v", r.msgs)
	}
}

type inviteBody struct {
	Username string             `json:"username"`
	Email    string             `json:"email"`
	Age      int                `json:"age"`
	Tags     []string           `json:"tags"`
	Nested   struct{ A string } `json:"nested"`
	Level    string             `json:"level" enum:"a,b"`
}

func apiWithBody(ext map[string]any) huma.API {
	a := humago.New(http.NewServeMux(), huma.DefaultConfig("t", "1"))
	huma.Register(a, op("invite-user", "POST", "/api/users", ext), func(context.Context, *struct{ Body inviteBody }) (*struct{ Body struct{} }, error) { return nil, nil })
	return a
}

// A prompted argument (form step, @teb-ooo/ui 0.80) must be a single-value property of the JSON request body; Form names the button.
func TestPromptArgumentsAndForm(t *testing.T) {
	ok := palette.Action{Title: "Invite", Group: "Users", Args: map[string]string{"username": "prompt", "email": "prompt", "age": "prompt", "level": "prompt"}, Form: &palette.Form{Submit: "Invite"}}.Ext()
	b, _ := json.Marshal(ok)
	if !strings.Contains(string(b), `"form":{"submit":"Invite"}`) || !strings.Contains(string(b), `"username":"prompt"`) {
		t.Fatalf("shape: %s", b)
	}
	r := &rec{}
	palette.Check(r, apiWithBody(ok))
	if len(r.msgs) != 0 {
		t.Fatalf("a valid form: %v", r.msgs)
	}
	for name, args := range map[string]map[string]string{
		"not a property": {"nope": "prompt"},
		"an array":       {"tags": "prompt"},
		"an object":      {"nested": "prompt"},
	} {
		tag := palette.Action{Title: "Invite", Group: "Users", Args: args}.Ext()
		r := &rec{}
		palette.Check(r, apiWithBody(tag))
		if len(r.msgs) == 0 || !strings.Contains(strings.Join(r.msgs, " "), "cannot ask for") {
			t.Errorf("%s: %v", name, r.msgs)
		}
	}
	// an operation without a JSON body cannot prompt
	noBody := api(op("do-thing", "POST", "/api/things", palette.Action{Title: "T", Group: "G", Args: map[string]string{"x": "prompt"}}.Ext()))
	r = &rec{}
	palette.Check(r, noBody)
	if len(r.msgs) == 0 || !strings.Contains(strings.Join(r.msgs, " "), "no JSON request body") {
		t.Errorf("no body: %v", r.msgs)
	}
	// a form key without prompt arguments in the document is refused too
	r = &rec{}
	palette.Check(r, api(op("do-thing", "POST", "/api/things", map[string]any{palette.Key: map[string]any{"title": "T", "group": "G", "form": map[string]any{"submit": "Go"}, "args": map[string]any{"x": "route.x"}}})))
	if len(r.msgs) == 0 {
		t.Error("a form without prompt arguments must be refused")
	}
}
