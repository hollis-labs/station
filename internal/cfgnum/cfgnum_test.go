package cfgnum

import (
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestIntegers(t *testing.T) {
	in := map[string]any{
		"big": float64(1048576), "small": float64(8080), "neg": float64(-2000000), "frac": 1.5,
		"huge": float64(1 << 60), "str": "1048576", "flag": true,
		"list": []any{float64(5000000), "x", map[string]any{"n": float64(1e6)}},
	}
	got := Integers(in).(map[string]any)
	want := map[string]any{
		"big": int64(1048576), "small": int64(8080), "neg": int64(-2000000), "frac": 1.5,
		"huge": float64(1 << 60), "str": "1048576", "flag": true,
		"list": []any{int64(5000000), "x", map[string]any{"n": int64(1000000)}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
	if in["big"] != float64(1048576) {
		t.Fatal("the input was modified")
	}
}

// The failure this package exists for: through YAML an integral float of a million or more becomes
// exponent text in a string field.
func TestYAMLRoundTripKeepsTheDigits(t *testing.T) {
	type s struct {
		ID string `yaml:"id"`
	}
	enc := func(v any) s {
		b, err := yaml.Marshal(map[string]any{"id": v})
		if err != nil {
			t.Fatal(err)
		}
		var out s
		if err := yaml.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := enc(float64(1048576)).ID; got == "1048576" {
		t.Skip("this yaml library no longer writes exponent form; the repair is harmless but untestable here")
	}
	if got := enc(Integers(float64(1048576))).ID; got != "1048576" {
		t.Fatalf("after Integers the id is %q", got)
	}
}
