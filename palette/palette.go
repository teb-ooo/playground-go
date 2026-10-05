// Package palette builds and checks the `x-palette` operation extension that generates Cmd+K commands from an app's OpenAPI
// document (ui decision 0004; the web side is `PaletteFromApi` in @teb-ooo/ui/cmdk). The tag is generated into the document through
// Huma Extensions, never hand-edited:
//
//	huma.Operation{OperationID: "disable-user", Method: http.MethodPost, Path: "/api/users/{id}/disable", ...,
//		Extensions: palette.Action{
//			Title: "Disable {username}", Group: "Users",
//			When:  &palette.When{Route: "/users", Needs: "selection"},
//			Args:  map[string]string{"id": "selection.id"},
//			Confirm: true,
//			After: &palette.After{Invalidate: []string{"/api/users"}},
//		}.Ext()}
//
//	huma.Operation{OperationID: "list-users", ..., Extensions: palette.Source{Group: "Users", Title: "{username}", Route: "/users?id={id}"}.Ext()}
//
//	huma.Operation{OperationID: "rotate-key", ..., Extensions: palette.None("needs a secret shown once; has its own screen")}
//
// Ext panics on a malformed tag (a programming error that shows at start and in every test), and Check, called from a test, fails on a
// malformed tag and on every non-GET operation that has neither a tag nor None with a reason, so a gap fails a test instead of waiting for
// a rule review (rule UI-yze).
package palette

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
)

// Key is the operation extension the web palette reads; ReasonKey carries the reason of None (Go side only).
const (
	Key       = "x-palette"
	ReasonKey = "x-palette-reason"
)

// When says where an action applies: a router route pattern (`/rule/$code`) and whether a selected row is needed.
type When struct {
	Route string `json:"route,omitempty"`
	Needs string `json:"needs,omitempty"` // "" or "selection"
}

// After says what happens after a success: refetch lists whose address starts with Invalidate, and/or go to Navigate.
type After struct {
	Invalidate []string `json:"invalidate,omitempty"`
	Navigate   string   `json:"navigate,omitempty"`
}

// Action is the tag of a mutating operation (not GET): it becomes a command. Args maps each path, query or body field to
// `route.<param>`, `selection.<field>` or a literal (`prompt` arrives with the web side's phase 2 and is refused now).
// Title and ConfirmText fill `{name}` from the selection, then the route parameters; a command whose names do not resolve is hidden.
// Confirm asks "<title>?"; ConfirmText asks its own question; NoConfirm turns off the default confirm of a DELETE.
type Action struct {
	Title       string
	Group       string
	When        *When
	Args        map[string]string
	Confirm     bool
	ConfirmText string
	NoConfirm   bool
	After       *After
	Hint        string
	Keywords    []string
}

// Source is the tag of a list or search operation (GET): the palette searches it while typing. Param is the query parameter that
// carries the text (default q), MinChars the typed characters needed (default 2), Limit the most results (default 8).
type Source struct {
	Group    string
	Title    string // {field} is filled from each result
	Hint     string
	Route    string // where choosing a result goes; {field} is filled and URL-encoded
	Param    string
	MinChars int
	Limit    int
}

// Validate reports what is wrong with the action, or nil.
func (a Action) Validate() error {
	var p []string
	if strings.TrimSpace(a.Title) == "" {
		p = append(p, "an action needs a title")
	}
	if strings.TrimSpace(a.Group) == "" {
		p = append(p, "an action needs a group")
	}
	if a.When != nil && a.When.Needs != "" && a.When.Needs != "selection" {
		p = append(p, fmt.Sprintf("when.needs is %q, not \"selection\"", a.When.Needs))
	}
	if a.Confirm && a.NoConfirm {
		p = append(p, "Confirm and NoConfirm contradict each other")
	}
	for _, k := range sortedKeys(a.Args) {
		switch v := a.Args[k]; {
		case v == "":
			p = append(p, fmt.Sprintf("argument %s is empty", k))
		case v == "prompt":
			p = append(p, fmt.Sprintf("argument %s is \"prompt\", which is not supported yet", k))
		}
	}
	return problems(p)
}

// Validate reports what is wrong with the source, or nil.
func (s Source) Validate() error {
	var p []string
	if s.Group == "" || s.Title == "" || s.Route == "" {
		p = append(p, "a source needs group, title and route")
	}
	if s.MinChars < 0 || s.Limit < 0 {
		p = append(p, "minChars and limit are not negative")
	}
	return problems(p)
}

// Ext returns the operation Extensions for the action; it panics when the action is malformed.
func (a Action) Ext() map[string]any {
	if err := a.Validate(); err != nil {
		panic("palette: " + err.Error())
	}
	m := map[string]any{"title": a.Title, "group": a.Group}
	if a.When != nil {
		m["when"] = a.When
	}
	if len(a.Args) > 0 {
		m["args"] = a.Args
	}
	switch {
	case a.ConfirmText != "":
		m["confirm"] = a.ConfirmText
	case a.Confirm:
		m["confirm"] = true
	case a.NoConfirm:
		m["confirm"] = false
	}
	if a.After != nil {
		m["after"] = a.After
	}
	if a.Hint != "" {
		m["hint"] = a.Hint
	}
	if len(a.Keywords) > 0 {
		m["keywords"] = a.Keywords
	}
	return map[string]any{Key: m}
}

// Ext returns the operation Extensions for the source; it panics when the source is malformed.
func (s Source) Ext() map[string]any {
	if err := s.Validate(); err != nil {
		panic("palette: " + err.Error())
	}
	m := map[string]any{"group": s.Group, "title": s.Title, "route": s.Route}
	if s.Hint != "" {
		m["hint"] = s.Hint
	}
	if s.Param != "" {
		m["param"] = s.Param
	}
	if s.MinChars > 0 {
		m["minChars"] = s.MinChars
	}
	if s.Limit > 0 {
		m["limit"] = s.Limit
	}
	return map[string]any{Key: map[string]any{"source": m}}
}

// None says an operation deliberately has no command (`x-palette: false`) and why; the reason is required.
func None(reason string) map[string]any {
	if strings.TrimSpace(reason) == "" {
		panic("palette: None needs a reason")
	}
	return map[string]any{Key: false, ReasonKey: reason}
}

// Merge combines operation Extensions (for example a palette tag and openapimcp.ExemptExtension); later maps win.
func Merge(exts ...map[string]any) map[string]any {
	out := map[string]any{}
	for _, e := range exts {
		for k, v := range e {
			out[k] = v
		}
	}
	return out
}

// Check is the contract test (call it from a test with the app's Huma API after every operation is registered). It fails on a tag that
// is malformed in the document, on an action tag on a GET, on a source tag on anything but a GET, and on every operation that changes
// something (not GET) and has neither a tag nor None with a reason. The operations in exempt are left out (hidden operations are not in the
// document anyway). It reads the tags the way the web side does (as JSON), so it fails for the same reasons as `paletteProblems` and
// `untaggedActions` in @teb-ooo/ui/cmdk.
func Check(t testing.TB, api huma.API, exempt ...string) {
	t.Helper()
	for path, item := range api.OpenAPI().Paths {
		for method, op := range map[string]*huma.Operation{"GET": item.Get, "PUT": item.Put, "POST": item.Post, "DELETE": item.Delete,
			"PATCH": item.Patch, "OPTIONS": item.Options, "HEAD": item.Head, "TRACE": item.Trace} {
			if op == nil || slices.Contains(exempt, op.OperationID) {
				continue
			}
			label := fmt.Sprintf("operation %q (%s %s)", op.OperationID, method, path)
			if msg := checkOperation(method, op); msg != "" {
				t.Errorf("%s: %s; see docs/go-api.md (palette)", label, msg)
			}
		}
	}
}

func checkOperation(method string, op *huma.Operation) string {
	raw, has := op.Extensions[Key]
	if !has {
		if method == "GET" || method == "HEAD" || method == "OPTIONS" || method == "TRACE" {
			return ""
		}
		return "changes something but has no x-palette tag: add palette.Action{...}.Ext() or palette.None(reason)"
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return "x-palette cannot be encoded: " + err.Error()
	}
	var v any
	_ = json.Unmarshal(b, &v)
	switch x := v.(type) {
	case bool:
		if x {
			return "x-palette: true is not a tag (use palette.Action or palette.Source, or palette.None(reason))"
		}
		if r, _ := op.Extensions[ReasonKey].(string); strings.TrimSpace(r) == "" {
			return "x-palette: false needs a reason (palette.None(reason))"
		}
	case map[string]any:
		if _, ok := x["source"]; ok {
			if method != "GET" {
				return "a source must be a GET"
			}
			var s struct {
				Source struct{ Group, Title, Route string } `json:"source"`
			}
			_ = json.Unmarshal(b, &s)
			if s.Source.Group == "" || s.Source.Title == "" || s.Source.Route == "" {
				return "a source needs group, title and route"
			}
			return ""
		}
		if _, ok := x["title"]; !ok {
			return "x-palette is neither an action (title, group), a source (source) nor false"
		}
		if method == "GET" {
			return "an action must change something (use a source for a list)"
		}
		if g, _ := x["group"].(string); g == "" {
			return "an action needs a group"
		}
		if args, _ := x["args"].(map[string]any); args != nil {
			for k, a := range args {
				if a == "prompt" {
					return fmt.Sprintf("argument %s is \"prompt\", which is not supported yet", k)
				}
			}
		}
	default:
		return "x-palette is neither an action (title, group), a source (source) nor false"
	}
	return ""
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

func problems(p []string) error {
	if len(p) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(p, "; "))
}
