package health

import "reflect"

// isNil catches a typed nil pointer stored in the interface, such as a nil
// *pgxpool.Pool.
func isNil(p Pinger) bool {
	rv := reflect.ValueOf(p)
	switch rv.Kind() {
	case reflect.Ptr, reflect.Map, reflect.Slice, reflect.Func, reflect.Interface, reflect.Chan:
		return rv.IsNil()
	}
	return false
}
