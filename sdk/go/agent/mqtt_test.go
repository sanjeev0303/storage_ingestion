package agent

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func publish(qos byte, id uint16, dup bool, topic string, payload []byte) (byte, []byte) {
	kind := byte(0x30) | qos<<1
	if dup {
		kind |= 0x08
	}
	body := mqttString(nil, topic)
	if qos > 0 {
		body = binary.BigEndian.AppendUint16(body, id)
	}
	return kind, append(body, payload...)
}

// TestMQTTSession runs the agent against a broker played by the test.
func TestMQTTSession(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	dir := t.TempDir()
	m := &MQTT{Broker: "tcp://" + ln.Addr().String(), ClientID: "dev-1", Topics: []string{"factory/+/telemetry"}, Dir: dir, RollSeconds: 3600}
	m.withDefaults()
	a := &Agent{cfg: Config{MQTT: m}, log: slog.New(slog.NewTextHandler(io.Discard, nil)), now: time.Now}
	w := &rollingWriter{dir: dir, name: m.Name, roll: time.Hour, maxBytes: m.RollBytes, now: time.Now}

	ctx, cancel := context.WithCancel(context.Background())
	ended := make(chan error, 1)
	go func() { ended <- a.mqttSession(ctx, w) }()

	conn, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)
	next := func(wantType byte) mqttPacket {
		t.Helper()
		for {
			p, err := readPacket(br)
			if err != nil {
				t.Fatalf("reading packet type %d: %v", wantType, err)
			}
			if p.kind>>4 == 12 { // PINGREQ
				continue
			}
			if p.kind>>4 != wantType {
				t.Fatalf("got packet type %d, want %d", p.kind>>4, wantType)
			}
			return p
		}
	}

	connect := next(1)
	if !strings.Contains(string(connect.body), "dev-1") || connect.body[7]&0x02 != 0 {
		t.Fatalf("CONNECT should name the client and keep the session: % x", connect.body)
	}
	_ = writePacket(conn, 0x20, []byte{0, 0})
	sub := next(8)
	if !strings.Contains(string(sub.body), "factory/+/telemetry") || sub.body[len(sub.body)-1] != 1 {
		t.Fatalf("SUBSCRIBE: % x", sub.body)
	}
	_ = writePacket(conn, 0x90, []byte{0, 1, 1})

	k, b := publish(1, 7, false, "factory/1/telemetry", []byte(`{"temp": 21.5}`))
	_ = writePacket(conn, k, b)
	k, b = publish(0, 0, false, "factory/2/telemetry", []byte{0x00, 0x01, 0xFF})
	_ = writePacket(conn, k, b)
	k, b = publish(2, 9, false, "factory/3/telemetry", []byte("x"))
	_ = writePacket(conn, k, b)
	k, b = publish(2, 9, true, "factory/3/telemetry", []byte("x")) // a resend before PUBREL
	_ = writePacket(conn, k, b)

	if ack := next(4); binary.BigEndian.Uint16(ack.body) != 7 {
		t.Fatalf("PUBACK for %v", ack.body)
	}
	next(5) // PUBREC for 9
	next(5) // and again for the resend
	_ = writePacket(conn, 0x62, []byte{0, 9})
	if comp := next(7); binary.BigEndian.Uint16(comp.body) != 9 {
		t.Fatalf("PUBCOMP for %v", comp.body)
	}

	cancel()
	next(14) // DISCONNECT
	<-ended
	if err := w.finish(); err != nil {
		t.Fatal(err)
	}

	files, _ := filepath.Glob(filepath.Join(dir, "mqtt_*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("files %v", files)
	}
	if !regexp.MustCompile(m.watch().RecordingPattern).MatchString(filepath.Base(files[0])) {
		t.Fatalf("the recording watch does not match %s", files[0])
	}
	data, _ := os.ReadFile(files[0])
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 lines (the QoS 2 resend kept once), got %q", lines)
	}
	var first, second map[string]any
	_ = json.Unmarshal([]byte(lines[0]), &first)
	_ = json.Unmarshal([]byte(lines[1]), &second)
	if first["topic"] != "factory/1/telemetry" || first["payload"].(map[string]any)["temp"] != 21.5 || first["qos"] != 1.0 {
		t.Fatalf("JSON payload line %q", lines[0])
	}
	if second["payload_base64"] != "AAH/" {
		t.Fatalf("binary payload line %q", lines[1])
	}
}

func TestRollingWriterRecovers(t *testing.T) {
	dir := t.TempDir()
	torn := filepath.Join(dir, "mqtt_1.jsonl.part")
	_ = os.WriteFile(torn, []byte("{\"a\":1}\n{\"b\":"), 0o644)
	empty := filepath.Join(dir, "mqtt_2.jsonl.part")
	_ = os.WriteFile(empty, []byte("{\"half"), 0o644)
	w := &rollingWriter{dir: dir, name: "mqtt", roll: time.Hour, maxBytes: 1 << 20, now: time.Now}
	if err := w.recover(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "mqtt_1.jsonl"))
	if err != nil || string(data) != "{\"a\":1}\n" {
		t.Fatalf("recovered %q %v", data, err)
	}
	if _, err := os.Stat(empty); !os.IsNotExist(err) {
		t.Fatal("a file with no whole line should be removed")
	}

	// A file is finished once it reaches its size.
	w.maxBytes = 10
	if err := w.write([]byte("0123456789\n")); err != nil {
		t.Fatal(err)
	}
	if parts, _ := filepath.Glob(filepath.Join(dir, "*.part")); len(parts) != 0 {
		t.Fatalf("still open: %v", parts)
	}
}

func TestMQTTConfig(t *testing.T) {
	c := Config{API: "https://x", Token: "t", StateDir: t.TempDir(), MQTT: &MQTT{Broker: "tls://broker.example:8883", Topics: []string{"a/#"}, Dir: t.TempDir()}}
	if err := c.Validate(); err != nil {
		t.Fatalf("mqtt alone should be enough: %v", err)
	}
	c.MQTT.Broker = "http://broker.example"
	if err := c.Validate(); err == nil {
		t.Fatal("http is not an MQTT broker")
	}
}
