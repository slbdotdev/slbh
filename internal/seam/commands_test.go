package seam

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestEveryCommandJSONRoundTrips(t *testing.T) {
	seenNames := make(map[string]bool)
	seenTypes := make(map[reflect.Type]bool)
	for _, name := range CommandNames() {
		command, ok := NewCommand(name)
		if !ok {
			t.Fatalf("NewCommand(%q) rejected an enumerated command", name)
		}
		if command.CommandName() != name {
			t.Fatalf("NewCommand(%q).CommandName() = %q", name, command.CommandName())
		}
		if seenNames[name] {
			t.Fatalf("command name %q is duplicated", name)
		}
		seenNames[name] = true
		typ := reflect.TypeOf(command)
		if seenTypes[typ] {
			t.Fatalf("command type %v is registered more than once", typ)
		}
		seenTypes[typ] = true

		data, err := json.Marshal(command)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		decoded := reflect.New(typ)
		if err := json.Unmarshal(data, decoded.Interface()); err != nil {
			t.Fatalf("unmarshal %s: %v", name, err)
		}
		if got := decoded.Elem().Interface(); !reflect.DeepEqual(got, command) {
			t.Fatalf("%s round trip = %#v, want %#v", name, got, command)
		}
	}
}

func TestCommandNamesReturnsACopy(t *testing.T) {
	first := CommandNames()
	first[0] = "mutated"
	if second := CommandNames(); second[0] == "mutated" {
		t.Fatal("mutating CommandNames result changed the registry")
	}
}
