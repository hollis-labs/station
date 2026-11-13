// Package cfgnum repairs numbers in a configuration tree before it is re-encoded as YAML.
//
// Substrate hands a unit's configuration over as generic values in which every number is a float64.
// Both Station apps write their block back out as YAML and decode it into structs, and YAML writes a
// float64 of 1,000,000 or more in exponent form: an unquoted `1048576` that belongs in a string field (an
// account id, an argument, an environment value) would arrive as the string "1.048576e+06". Integral
// floats are therefore turned back into integers first, so the text a person wrote is the text the struct
// gets, as it was when the old build read the same YAML file directly.
//
// Not repaired: a number the file wrote beyond 2^53 has already lost digits when Substrate read it, so
// quote such a value.
package cfgnum

import "math"

// exact is the largest magnitude at which every integer is representable in a float64.
const exact = 1 << 53

// Integers returns a copy of v in which every float64 holding a whole number below 2^53 in magnitude is
// an int64. Maps, lists and other values are copied or passed through unchanged.
func Integers(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = Integers(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = Integers(e)
		}
		return out
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < exact {
			return int64(x)
		}
		return x
	default:
		return v
	}
}
