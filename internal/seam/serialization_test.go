package seam

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestRuntimeSurfaceCarriesOnlySerializableValues(t *testing.T) {
	runtimeType := reflect.TypeOf((*Runtime)(nil)).Elem()
	for i := 0; i < runtimeType.NumMethod(); i++ {
		method := runtimeType.Method(i)
		for j := 0; j < method.Type.NumIn(); j++ {
			assertSerializableSurfaceType(t, method.Name+" argument", method.Type.In(j))
		}
		for j := 0; j < method.Type.NumOut(); j++ {
			assertSerializableSurfaceType(t, method.Name+" result", method.Type.Out(j))
		}
	}
}

func assertSerializableSurfaceType(t *testing.T, label string, typ reflect.Type) {
	t.Helper()
	assertNoLiveSeamType(t, label, typ, make(map[reflect.Type]bool))
	if typ.PkgPath() == "context" && typ.Name() == "Context" {
		t.Fatalf("%s exposes context.Context, whose cancellation channel is not seam data", label)
	}
	value := reflect.Zero(typ).Interface()
	if _, err := json.Marshal(value); err != nil {
		t.Fatalf("%s type %v does not marshal to JSON: %v", label, typ, err)
	}
}

func assertNoLiveSeamType(t *testing.T, label string, typ reflect.Type, seen map[reflect.Type]bool) {
	t.Helper()
	if seen[typ] {
		return
	}
	seen[typ] = true
	switch typ.Kind() {
	case reflect.Chan, reflect.Func, reflect.Ptr, reflect.UnsafePointer:
		t.Fatalf("%s exposes non-serializable %v", label, typ)
	case reflect.Array, reflect.Slice:
		assertNoLiveSeamType(t, label, typ.Elem(), seen)
	case reflect.Map:
		assertNoLiveSeamType(t, label, typ.Key(), seen)
		assertNoLiveSeamType(t, label, typ.Elem(), seen)
	case reflect.Struct:
		// External value documents such as config.Config own their internal
		// representation. The seam test guards seam-owned payload fields and
		// direct method types, which is where a live runtime pointer could leak.
		if typ.PkgPath() != reflect.TypeOf(Event{}).PkgPath() {
			return
		}
		for i := 0; i < typ.NumField(); i++ {
			assertNoLiveSeamType(t, label+" field "+typ.Field(i).Name, typ.Field(i).Type, seen)
		}
	}
}

func TestEventPollingPayloadsJSONRoundTrip(t *testing.T) {
	want := EventBatch{
		Events: []Event{{
			Cursor:     7,
			Time:       time.Date(2026, 9, 18, 1, 2, 3, 0, time.UTC),
			RuntimeID:  "run-1",
			AgentID:    "agent-1",
			AgentTitle: "seat",
			Kind:       "assistant",
			Text:       "done",
			Metadata:   map[string]any{"round": float64(1), "labels": []any{"one", "two"}},
		}},
		Cursor: 7,
		End:    true,
	}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got EventBatch
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("event batch round trip = %#v, want %#v", got, want)
	}

	query := EventQuery{After: 6, Limit: 128, WaitMilliseconds: 250}
	encoded, err = json.Marshal(query)
	if err != nil {
		t.Fatal(err)
	}
	var decodedQuery EventQuery
	if err := json.Unmarshal(encoded, &decodedQuery); err != nil {
		t.Fatal(err)
	}
	if decodedQuery != query {
		t.Fatalf("event query round trip = %#v, want %#v", decodedQuery, query)
	}
}
