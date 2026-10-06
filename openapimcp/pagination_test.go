package openapimcp_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/teb-ooo/playground-go/openapimcp"
	"github.com/teb-ooo/playground-go/page"
)

func listOp() huma.Operation {
	o := goodOp()
	o.OperationID = "list-widgets"
	return o
}

func runList[I, O any](op huma.Operation, opts ...openapimcp.ParityOption) []string {
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("t", "1"))
	huma.Register(api, op, func(context.Context, *I) (*struct{ Body O }, error) { return nil, nil })
	ft := &fakeT{}
	openapimcp.ParityCheck(ft, api, openapimcp.Handler(api, mux, openapimcp.Options{}), opts...)
	return ft.msgs
}

type pagedIn struct{ page.Params }
type noParamsIn struct{}
type badLimitIn struct {
	Limit  int    `query:"limit"`
	Cursor string `query:"cursor"`
}
type itemsNoCursor struct {
	Items []goodOut `json:"items"`
}

func TestPaginationRule(t *testing.T) {
	// the page package satisfies it
	expect(t, runList[pagedIn, page.Body[goodOut]](listOp()))
	// an items array without the parameters
	expect(t, runList[noParamsIn, page.Body[goodOut]](listOp()), "lacks the limit or cursor query parameter (API-jvg)")
	// a limit without bounds
	expect(t, runList[badLimitIn, page.Body[goodOut]](listOp()), "limit parameter of a list operation must be an integer with minimum, maximum and default")
	// parameters but no next_cursor
	expect(t, runList[pagedIn, itemsNoCursor](listOp()), "must have a next_cursor property")
	// a bounded list is exempted with a reason
	expect(t, runList[noParamsIn, page.Body[goodOut]](listOp(), openapimcp.WithExempt("list-widgets")))
	// only GET operations with an items array are lists
	post := goodOp()
	post.Method = http.MethodPost
	post.OperationID = "create-widget"
	expect(t, runList[noParamsIn, page.Body[goodOut]](post))
	if msgs := runContract[goodOut](goodOp()); len(msgs) != 0 && strings.Contains(strings.Join(msgs, " "), "API-jvg") {
		t.Errorf("a GET without an items array is not a list: %v", msgs)
	}
}

type bareList struct{}

// A list-* operation must be paginated even when it returns a bare array or an object without items.
func TestListNamedOperationsMustBePaginated(t *testing.T) {
	op := listOp() // list-widgets
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("t", "1"))
	huma.Register(api, op, func(context.Context, *pagedIn) (*struct{ Body []goodOut }, error) { return nil, nil })
	ft := &fakeT{}
	openapimcp.ParityCheck(ft, api, openapimcp.Handler(api, mux, openapimcp.Options{}))
	if !strings.Contains(strings.Join(ft.msgs, " "), "a bare array") {
		t.Fatalf("a bare array from list-widgets: %v", ft.msgs)
	}
	// an object without items
	expect(t, runList[pagedIn, struct {
		Rows []goodOut `json:"rows"`
	}](op), "an object without an items array")
	// not a list by name: untouched
	other := goodOp()
	other.OperationID = "get-widgets"
	expect(t, runList[pagedIn, struct {
		Rows []goodOut `json:"rows"`
	}](other))
	// the exemption still works
	expect(t, runList[pagedIn, struct {
		Rows []goodOut `json:"rows"`
	}](op, openapimcp.WithExempt("list-widgets")))
}
