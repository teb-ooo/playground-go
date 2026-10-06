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
	"reflect"
	"regexp"
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
//
// Field shows the command only while the published selection holds the value: a map of selection field to a string, a number or a bool, or
// to a non-empty list of them (any of the values), for example Field: map[string]any{"status": "disabled"} or {"status": []string{"staged", "draft"}};
// it implies Needs "selection". Differs keeps the command off a row that equals a value of the viewer or the route: a map of selection field to
// "user.<field>" or "route.<param>", for example Differs: map[string]string{"id": "user.subject"} so "Disable" never shows on one's own row.
type When struct {
	Route   string            `json:"route,omitempty"`
	Needs   string            `json:"needs,omitempty"` // "" or "selection"
	Field   map[string]any    `json:"field,omitempty"`
	Differs map[string]string `json:"differs,omitempty"`
}

// After says what happens after a success: refetch lists whose address starts with Invalidate, and/or go to Navigate.
type After struct {
	Invalidate []string `json:"invalidate,omitempty"`
	Navigate   string   `json:"navigate,omitempty"`
}

// Form is the text of the form step an action with "prompt" arguments opens.
type Form struct {
	Submit string `json:"submit,omitempty"` // the submit button's label, for example "Invite"
}

// Action is the tag of a mutating operation (not GET): it becomes a command. Args maps each path, query or body field to
// `route.<param>`, `selection.<field>`, a literal, or "prompt": the person is asked for that field in a form step built from the operation's
// JSON request body schema (the field must be a property of the body: a string, integer, number, boolean or string enum), and Form names the
// form's submit button (@teb-ooo/ui 0.80).
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
	Form        *Form
	Hint        string
	Keywords    []string
	// Role limits who sees the command: "admin" or "owner" (hidden from everyone else); empty shows it to everyone.
	Role string
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
	// Role limits who sees the source: "admin" or "owner"; empty shows it to everyone. It travels beside `source` in the tag, not inside it.
	Role string
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
	p = append(p, roleProblem(a.Role)...)
	if a.When != nil {
		p = append(p, whenProblems(a.When)...)
	}
	for _, k := range sortedKeys(a.Args) {
		switch v := a.Args[k]; {
		case v == "":
			p = append(p, fmt.Sprintf("argument %s is empty", k))
		}
	}
	prompted := false
	for _, v := range a.Args {
		prompted = prompted || v == "prompt"
	}
	if a.Form != nil && !prompted {
		p = append(p, "Form needs at least one argument marked \"prompt\"")
	}
	if a.Form != nil && strings.TrimSpace(a.Form.Submit) == "" {
		p = append(p, "Form needs a Submit label")
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
	p = append(p, roleProblem(s.Role)...)
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
	if a.Form != nil {
		m["form"] = a.Form
	}
	if a.Hint != "" {
		m["hint"] = a.Hint
	}
	if len(a.Keywords) > 0 {
		m["keywords"] = a.Keywords
	}
	if a.Role != "" {
		m["role"] = a.Role
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
	tag := map[string]any{"source": m}
	if s.Role != "" {
		tag["role"] = s.Role // beside `source`, as @teb-ooo/ui reads it
	}
	return map[string]any{Key: tag}
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
			if msg := checkOperation(api, method, op); msg != "" {
				t.Errorf("%s: %s; see docs/go-api.md (palette)", label, msg)
			}
		}
	}
}

func checkOperation(api huma.API, method string, op *huma.Operation) string {
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
			if r, has := x["role"]; has {
				if rs, _ := r.(string); roleProblem(rs) != nil || rs == "" {
					return `role must be "admin" or "owner"`
				}
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
			var prompted []string
			for _, k := range sortedAnyKeys(args) {
				if args[k] == "prompt" {
					prompted = append(prompted, k)
				}
			}
			if len(prompted) > 0 {
				if msg := promptProblem(api, op, prompted); msg != "" {
					return fmt.Sprintf("cannot ask for %s: %s", strings.Join(prompted, ", "), msg)
				}
			} else if _, hasForm := x["form"]; hasForm {
				return `form needs at least one argument marked "prompt"`
			}
		}
		if r, has := x["role"]; has {
			if rs, _ := r.(string); roleProblem(rs) != nil || rs == "" {
				return `role must be "admin" or "owner"`
			}
		}
		if w, _ := x["when"].(map[string]any); w != nil {
			if d, _ := w["differs"].(map[string]any); d != nil {
				for _, k := range sortedAnyKeys(d) {
					if v, _ := d[k].(string); !differsRE.MatchString(v) {
						return fmt.Sprintf(`when.differs.%s must be "user.<field>" or "route.<param>"`, k)
					}
				}
			}
			if f, _ := w["field"].(map[string]any); f != nil {
				for _, k := range sortedAnyKeys(f) {
					if !fieldValueOK(f[k]) {
						return fmt.Sprintf("when.field.%s must be a string, number or bool, or a non-empty list of them", k)
					}
				}
			}
		}
	default:
		return "x-palette is neither an action (title, group), a source (source) nor false"
	}
	return ""
}

// promptProblem checks that every prompted argument is a property of the operation's JSON request body (an object schema) and a single
// value: a string, integer, number, boolean or string enum (not an array, an object or a oneOf/anyOf), as @teb-ooo/ui's paletteProblems does.
func promptProblem(api huma.API, op *huma.Operation, prompted []string) string {
	if op.RequestBody == nil {
		return "the operation has no JSON request body to ask for"
	}
	mt := op.RequestBody.Content["application/json"]
	if mt == nil || mt.Schema == nil {
		return "the operation has no JSON request body to ask for"
	}
	body := resolve(api, mt.Schema)
	if body == nil || (body.Type != "object" && body.Properties == nil) {
		return "the JSON request body is not an object"
	}
	for _, name := range prompted {
		prop := body.Properties[name]
		if prop == nil {
			return name + " is not a property of the request body"
		}
		ps := resolve(api, prop)
		switch {
		case ps == nil:
			return name + " has no schema"
		case len(ps.OneOf) > 0 || len(ps.AnyOf) > 0 || len(ps.AllOf) > 0:
			return name + " is a oneOf/anyOf/allOf, not a single value"
		case ps.Type == "array" || ps.Type == "object":
			return name + " is an " + ps.Type + ", not a single value"
		case ps.Type != "string" && ps.Type != "integer" && ps.Type != "number" && ps.Type != "boolean":
			return name + " has type " + fmt.Sprintf("%q", ps.Type) + ", not a string, integer, number, boolean or string enum"
		}
	}
	return ""
}

func resolve(api huma.API, s *huma.Schema) *huma.Schema {
	if s != nil && s.Ref != "" {
		if c := api.OpenAPI().Components; c != nil && c.Schemas != nil {
			return c.Schemas.SchemaFromRef(s.Ref)
		}
		return nil
	}
	return s
}

var differsRE = regexp.MustCompile(`^(user|route)\.\w+$`)

func roleProblem(role string) []string {
	if role != "" && role != "admin" && role != "owner" {
		return []string{fmt.Sprintf("role is %q, not \"admin\" or \"owner\"", role)}
	}
	return nil
}

// scalarOK reports whether v is a string, a number or a bool.
func scalarOK(v any) bool {
	switch v.(type) {
	case string, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, json.Number:
		return true
	}
	return false
}

// fieldValueOK: a scalar, or a non-empty list of scalars.
func fieldValueOK(v any) bool {
	if scalarOK(v) {
		return true
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return false
	}
	if rv.Len() == 0 {
		return false
	}
	for i := 0; i < rv.Len(); i++ {
		if !scalarOK(rv.Index(i).Interface()) {
			return false
		}
	}
	return true
}

func whenProblems(w *When) []string {
	var p []string
	for _, k := range sortedAnyKeys(w.Field) {
		if !fieldValueOK(w.Field[k]) {
			p = append(p, fmt.Sprintf("when.field.%s must be a string, number or bool, or a non-empty list of them", k))
		}
	}
	for _, k := range sortedKeys(w.Differs) {
		if !differsRE.MatchString(w.Differs[k]) {
			p = append(p, fmt.Sprintf("when.differs.%s must be \"user.<field>\" or \"route.<param>\"", k))
		}
	}
	return p
}

func sortedAnyKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
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
