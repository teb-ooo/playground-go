package page_test

import (
	"errors"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/teb-ooo/playground-go/page"
)

type key struct {
	ID string `json:"id"`
}

func TestCursorRoundTripAndErrors(t *testing.T) {
	c, err := page.Encode(key{ID: "x1"})
	if err != nil {
		t.Fatal(err)
	}
	var k key
	ok, err := page.Decode(c, &k)
	if err != nil || !ok || k.ID != "x1" {
		t.Fatalf("round trip: %v %v %+v", ok, err, k)
	}
	if ok, err := page.Decode("", &k); ok || err != nil {
		t.Errorf("empty cursor is the first page: %v %v", ok, err)
	}
	for _, bad := range []string{"%%%", "bm90LWpzb24"} { // not base64; base64 of "not-json"
		_, err := page.Decode(bad, &k)
		var se huma.StatusError
		if !errors.As(err, &se) || se.GetStatus() != 400 {
			t.Errorf("Decode(%q) = %v, want a 400", bad, err)
		}
	}
}

func TestTrim(t *testing.T) {
	rows := []key{{"a"}, {"b"}, {"c"}}
	items, next := page.Trim(rows, 2, func(r key) any { return r })
	if len(items) != 2 || next == nil {
		t.Fatalf("%v %v", items, next)
	}
	var k key
	if _, err := page.Decode(*next, &k); err != nil || k.ID != "b" {
		t.Errorf("the cursor holds the last returned row: %+v %v", k, err)
	}
	items, next = page.Trim(rows[:2], 2, func(r key) any { return r })
	if len(items) != 2 || next != nil {
		t.Errorf("exactly limit rows is the last page: %v %v", items, next)
	}
	items, next = page.Trim[key](nil, 2, nil)
	if items == nil || len(items) != 0 || next != nil {
		t.Errorf("no rows: a non-nil empty slice (JSON []), no cursor: %v %v", items, next)
	}
}
