package openapimcp_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/teb-ooo/playground-go/openapimcp"
)

type goodOut struct {
	ID        string    `json:"id" format:"uuid"`
	OwnerID   string    `json:"owner_id" format:"uuid"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

var session = []map[string][]string{{"session": {}}, {"bearer": {}}}

func goodOp() huma.Operation {
	return huma.Operation{
		OperationID: "get-widgets", Method: http.MethodGet, Path: "/api/widgets",
		Summary: "List widgets", Description: "Lists widgets.", Tags: []string{"widgets"}, Security: session,
	}
}

type fakeT struct {
	testing.TB
	msgs []string
}

func (f *fakeT) Helper() {}
func (f *fakeT) Errorf(format string, a ...any) {
	f.msgs = append(f.msgs, strings.TrimSpace(fmt.Sprintf(format, a...)))
}
func (f *fakeT) Fatalf(format string, a ...any) { f.Errorf(format, a...) }

func runContract[O any](op huma.Operation, opts ...openapimcp.ParityOption) []string {
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("t", "1"))
	huma.Register(api, op, func(context.Context, *struct{}) (*struct{ Body O }, error) { return nil, nil })
	ft := &fakeT{}
	openapimcp.ParityCheck(ft, api, openapimcp.Handler(api, mux, openapimcp.Options{}), opts...)
	return ft.msgs
}

func expect(t *testing.T, msgs []string, want ...string) {
	t.Helper()
	if len(want) == 0 {
		if len(msgs) != 0 {
			t.Fatalf("expected no failure, got %v", msgs)
		}
		return
	}
	all := strings.Join(msgs, "\n")
	for _, w := range want {
		if !strings.Contains(all, w) {
			t.Errorf("failure %q missing from:\n%s", w, all)
		}
	}
	for _, m := range msgs {
		if !strings.HasSuffix(m, "see docs/go-api.md") {
			t.Errorf("message does not end with the doc pointer: %s", m)
		}
	}
}

func TestContractHappyPath(t *testing.T) {
	expect(t, runContract[[]goodOut](goodOp()))
}

func TestContractOperationFailures(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*huma.Operation)
		want string
	}{
		{"operation id", func(o *huma.Operation) { o.OperationID = "listWidgets" }, "OperationID is not kebab-case"},
		{"single word id", func(o *huma.Operation) { o.OperationID = "widgets" }, "OperationID is not kebab-case"},
		{"path", func(o *huma.Operation) { o.Path = "/widgets" }, "Path does not start with /api/"},
		{"summary", func(o *huma.Operation) { o.Summary = "" }, "Summary is empty"},
		{"description", func(o *huma.Operation) { o.Description = " " }, "Description is empty"},
		{"tags", func(o *huma.Operation) { o.Tags = nil }, "Tags is empty"},
		{"security none", func(o *huma.Operation) { o.Security = nil }, "Security does not cover"},
		{"security bearer only", func(o *huma.Operation) { o.Security = session[1:] }, "Security does not cover"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			op := goodOp()
			c.mut(&op)
			msgs := runContract[goodOut](op)
			expect(t, msgs, c.want, "list-widgets"[:0]+"operation")
			if !strings.Contains(strings.Join(msgs, ""), "fix:") {
				t.Error("message does not say how to fix")
			}
		})
	}
}

func TestContractSchemaFailures(t *testing.T) {
	type camel struct {
		UserName string `json:"userName"`
	}
	type nested struct {
		Inner camel `json:"inner"`
	}
	type badTime struct {
		CreatedAt string `json:"created_at"`
	}
	type badID struct {
		ID string `json:"id"`
	}
	type badRefID struct {
		OwnerID string `json:"owner_id" format:"uuid"`
		UserID  string `json:"user_id"`
	}
	type intID struct {
		ID int `json:"id"`
	}
	expect(t, runContract[camel](goodOp()), `property "userName" is not snake_case`, "user_name", "response 200")
	expect(t, runContract[nested](goodOp()), `property "userName" is not snake_case`)
	expect(t, runContract[badTime](goodOp()), `timestamp property "created_at"`, "date-time")
	expect(t, runContract[badID](goodOp()), `id property "id"`, "neither format nor pattern", `format:"uuid"`, "pattern", "WithExempt", "docs/go-api.md")
	expect(t, runContract[badRefID](goodOp()), `id property "user_id"`)
	expect(t, runContract[intID](goodOp()), `id property "id"`, "not a string", "WithExempt")
}

func TestContractIDRule(t *testing.T) {
	type uuidID struct {
		ID string `json:"id" format:"uuid"`
	}
	type patternID struct {
		ID string `json:"id" pattern:"^[a-z]+-[a-z0-9.]+$"`
	}
	type formatID struct {
		JobID string `json:"job_id" format:"hostname"`
	}
	type bare struct {
		ID string `json:"id"`
	}
	type integer struct {
		ID int64 `json:"id"`
	}
	expect(t, runContract[uuidID](goodOp()))
	expect(t, runContract[patternID](goodOp()))
	expect(t, runContract[formatID](goodOp()))
	expect(t, runContract[bare](goodOp()), "neither format nor pattern")
	expect(t, runContract[integer](goodOp()), "not a string")

	exempt := goodOp()
	exempt.Extensions = map[string]any{openapimcp.ExemptExtension: "audit entry ids are database sequence numbers"}
	expect(t, runContract[integer](exempt))
	expect(t, runContract[bare](goodOp(), openapimcp.WithExempt("get-widgets")))
}

func TestContractRequestBodyChecked(t *testing.T) {
	type in struct {
		FooBar string `json:"fooBar"`
	}
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("t", "1"))
	op := goodOp()
	op.Method = http.MethodPost
	huma.Register(api, op, func(context.Context, *struct{ Body in }) (*struct{}, error) { return nil, nil })
	ft := &fakeT{}
	openapimcp.ParityCheck(ft, api, openapimcp.Handler(api, mux, openapimcp.Options{}))
	expect(t, ft.msgs, `request body property "fooBar"`)
}

func TestContractExemptions(t *testing.T) {
	op := goodOp()
	op.OperationID = "passkeyStart"
	op.Path = "/auth/passkey/start"
	op.Security = nil
	expect(t, runContract[goodOut](op), "OperationID", "Path", "Security")
	expect(t, runContract[goodOut](op, openapimcp.WithExempt("passkeyStart")))

	op.Extensions = map[string]any{openapimcp.ExemptExtension: "WebAuthn ceremony: shapes fixed by the spec"}
	expect(t, runContract[goodOut](op))

	op.Extensions = map[string]any{openapimcp.ExemptExtension: true}
	expect(t, runContract[goodOut](op), "needs a non-empty string reason")
}
