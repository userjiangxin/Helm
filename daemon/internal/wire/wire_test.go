package wire

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const fixtures = "../../../protocol/fixtures"

// roundTrip decodes a fixture into a typed value, re-encodes it, and checks that
// no field was lost or changed (compared as generic JSON).
func roundTrip(t *testing.T, name string, v any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtures, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("%s: decode: %v", name, err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("%s: encode: %v", name, err)
	}
	var a, b any
	_ = json.Unmarshal(raw, &a)
	_ = json.Unmarshal(out, &b)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("%s: round trip mismatch\nwant %s\n got %s", name, raw, out)
	}
}

func TestFixtures(t *testing.T) {
	roundTrip(t, "run_assignment.json", &RunAssignment{})
	roundTrip(t, "directive.json", &Directive{})
	roundTrip(t, "directive_ack.json", &DirectiveAck{})
	roundTrip(t, "capabilities.json", &Capabilities{})
	roundTrip(t, "ws_wake.json", &Envelope{})
	roundTrip(t, "ws_heartbeat.json", &Envelope{})
	roundTrip(t, "ws_run_events.json", &Envelope{})
	roundTrip(t, "ws_events_ack.json", &Envelope{})
}

func TestEnvelopePayloads(t *testing.T) {
	cases := map[string]any{
		"ws_wake.json":       &Wake{},
		"ws_heartbeat.json":  &Heartbeat{},
		"ws_run_events.json": &RunEvents{},
		"ws_events_ack.json": &EventsAck{},
	}
	for name, payload := range cases {
		raw, _ := os.ReadFile(filepath.Join(fixtures, name))
		var env Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatal(err)
		}
		if env.V != Version {
			t.Fatalf("%s: version %d", name, env.V)
		}
		dec := json.NewDecoder(bytes.NewReader(env.Payload))
		dec.DisallowUnknownFields()
		if err := dec.Decode(payload); err != nil {
			t.Fatalf("%s payload: %v", name, err)
		}
	}
}
