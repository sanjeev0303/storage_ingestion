package event

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func encoded(t *testing.T, env Envelope) []byte {
	t.Helper()
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func validEnvelope() Envelope {
	e := NewUploadCompleted("tenant-1", "upload-1", time.Now().UTC(), "corr-1")
	e.AggregateSequence = 1
	return FromOutbox(e)
}

func TestEnvelopeRoundTrip(t *testing.T) {
	sent := validEnvelope()
	got, err := Decode(encoded(t, sent))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != sent.ID || got.Type != TypeUploadCompleted || got.SchemaVersion != 1 || got.TenantID != "tenant-1" ||
		got.AggregateType != AggregateUpload || got.AggregateID != "upload-1" || got.CorrelationID != "corr-1" {
		t.Fatalf("envelope changed in transit: %+v", got)
	}
	var p UploadCompleted
	if err := DecodePayload(got, &p); err != nil || p.UploadID != "upload-1" {
		t.Fatalf("payload: %+v %v", p, err)
	}
	if q, _ := QueueOf(got.Type); q != QueueProcessing {
		t.Fatalf("upload.completed belongs on %s, got %s", QueueProcessing, q)
	}
}

func TestUnknownTypesAndVersionsAreRefused(t *testing.T) {
	unknown := validEnvelope()
	unknown.Type = "upload.teleported"
	if _, err := Decode(encoded(t, unknown)); !errors.Is(err, ErrUnknownType) {
		t.Fatalf("an unknown type must be refused, got %v", err)
	}

	future := validEnvelope()
	future.SchemaVersion = 9
	env, err := Decode(encoded(t, future))
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("an unsupported version must be refused, got %v", err)
	}
	if env.ID != future.ID {
		t.Fatal("a refused envelope must still carry its ID, so the failure can be recorded against it")
	}
}

func TestMalformedMessagesAreRefused(t *testing.T) {
	cases := map[string][]byte{
		"not json":     []byte(`{"id": "x", "type":`),
		"no id":        encoded(t, func() Envelope { e := validEnvelope(); e.ID = ""; return e }()),
		"no tenant":    encoded(t, func() Envelope { e := validEnvelope(); e.TenantID = ""; return e }()),
		"no sequence":  encoded(t, func() Envelope { e := validEnvelope(); e.AggregateSequence = 0; return e }()),
		"no payload":   encoded(t, func() Envelope { e := validEnvelope(); e.Payload = nil; return e }()),
		"empty field":  encoded(t, func() Envelope { e := validEnvelope(); e.Payload = json.RawMessage(`{"upload_id": ""}`); return e }()),
		"wrong type":   encoded(t, func() Envelope { e := validEnvelope(); e.Payload = json.RawMessage(`{"upload_id": 42}`); return e }()),
		"array body":   []byte(`[1, 2, 3]`),
		"empty object": []byte(`{}`),
	}
	for name, body := range cases {
		if _, err := Decode(body); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: want ErrMalformed, got %v", name, err)
		}
	}
}

func TestNewOptionalPayloadFieldsStayCompatible(t *testing.T) {
	env := validEnvelope()
	env.Payload = json.RawMessage(`{"upload_id": "upload-1", "added_later": true}`)
	if _, err := Decode(encoded(t, env)); err != nil {
		t.Fatalf("an unknown optional field must not break version 1 consumers: %v", err)
	}
}

func TestEventsCarryIdentifiersOnly(t *testing.T) {
	for _, e := range []struct {
		name    string
		payload []byte
	}{
		{"upload", NewUploadCompleted("t", "u", time.Now(), "").Payload},
		{"deletion", NewDeletionScheduled("t", "a", "op", time.Now(), "").Payload},
		{"hold", NewHoldReleased("t", "h", time.Now(), "").Payload},
	} {
		if strings.Contains(string(e.payload), "http") || len(e.payload) > 100 {
			t.Errorf("%s payload must hold identifiers only: %s", e.name, e.payload)
		}
	}
}

func TestReplayIsANewOperationCausedByTheOriginal(t *testing.T) {
	original := NewDeletionScheduled("t", "asset-1", "op-1", time.Now(), "corr")
	replay := Replay(original, time.Now())
	if replay.ID == original.ID || replay.CausationID != original.ID || replay.CorrelationID != "corr" ||
		replay.AggregateID != "asset-1" || string(replay.Payload) != string(original.Payload) {
		t.Fatalf("unexpected replay %+v", replay)
	}
}
