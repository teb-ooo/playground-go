package embed

import (
	"database/sql/driver"
	"math"
	"reflect"
	"testing"
)

func TestLiteralAndParseRoundTrip(t *testing.T) {
	in := []float32{0.1, -2, 3.5e-8, 0, 1e20}
	lit := Literal(in)
	if lit != "[0.1,-2,3.5e-08,0,1e+20]" {
		t.Fatal(lit)
	}
	out, err := ParseLiteral(lit)
	if err != nil || !reflect.DeepEqual(in, out) {
		t.Fatalf("%v %v", out, err)
	}
	if Literal(nil) != "[]" {
		t.Fatal("empty literal")
	}
	if v, err := ParseLiteral(" [ 1 , 2 ] "); err != nil || len(v) != 2 {
		t.Fatalf("%v %v", v, err)
	}
	for _, bad := range []string{"", "1,2", "[1,x]", "(1,2)"} {
		if _, err := ParseLiteral(bad); err == nil {
			t.Errorf("%q must fail", bad)
		}
	}
}

func TestVectorValueAndScan(t *testing.T) {
	var _ driver.Valuer = Vector(nil)
	val, err := Vector{1, 2.5}.Value()
	if err != nil || val != "[1,2.5]" {
		t.Fatalf("%v %v", val, err)
	}
	if v, err := Vector(nil).Value(); err != nil || v != nil {
		t.Fatalf("nil vector is NULL: %v %v", v, err)
	}
	for _, bad := range []float32{float32(math.NaN()), float32(math.Inf(1))} {
		if _, err := (Vector{1, bad}).Value(); err == nil {
			t.Errorf("%v must be refused", bad)
		}
	}
	var v Vector
	if err := v.Scan("[1,2,3]"); err != nil || !reflect.DeepEqual(v, Vector{1, 2, 3}) {
		t.Fatalf("%v %v", v, err)
	}
	if err := v.Scan([]byte("[4]")); err != nil || !reflect.DeepEqual(v, Vector{4}) {
		t.Fatalf("%v %v", v, err)
	}
	if err := v.Scan(nil); err != nil || v != nil {
		t.Fatalf("%v %v", v, err)
	}
	if err := v.Scan(42); err == nil {
		t.Fatal("int must fail")
	}
}
