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
	expect(t, runList[pagedIn, page.Body[goodOut]](goodOp()))
	// an items array without the parameters
	expect(t, runList[noParamsIn, page.Body[goodOut]](goodOp()), "lacks the limit or cursor query parameter (API-jvg)")
	// a limit without bounds
	expect(t, runList[badLimitIn, page.Body[goodOut]](goodOp()), "limit parameter of a list operation must be an integer with minimum, maximum and default")
	// parameters but no next_cursor
	expect(t, runList[pagedIn, itemsNoCursor](goodOp()), "must have a next_cursor property")
	// a bounded list is exempted with a reason
	expect(t, runList[noParamsIn, page.Body[goodOut]](goodOp(), openapimcp.WithExempt("list-widgets")))
	// only GET operations with an items array are lists
	post := goodOp()
	post.Method = http.MethodPost
	post.OperationID = "create-widget"
	expect(t, runList[noParamsIn, page.Body[goodOut]](post))
	if msgs := runContract[goodOut](goodOp()); len(msgs) != 0 && strings.Contains(strings.Join(msgs, " "), "API-jvg") {
		t.Errorf("a GET without an items array is not a list: %v", msgs)
	}
}
