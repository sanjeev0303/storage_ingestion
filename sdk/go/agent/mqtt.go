package agent

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// MQTT subscribes to a broker and keeps every message, one JSON line each,
// in rolling files under Dir. Each finished file is a chunk of one logical
// recording, sent like any other (docs/edge.md). Messages at QoS 1 and 2 are
// acknowledged only once their line is synced to disk, and the session is
// persistent, so the broker holds what arrives while the device is offline.
type MQTT struct {
	// Broker is tcp://host:1883 or tls://host:8883 (mqtt://, mqtts:// and
	// ssl:// work too).
	Broker string `json:"broker"`
	// ClientID names the session the broker keeps; default osp-agent-<host>.
	ClientID     string   `json:"client_id,omitempty"`
	Username     string   `json:"username,omitempty"`
	PasswordFile string   `json:"password_file,omitempty"`
	CAFile       string   `json:"ca_file,omitempty"` // for a broker with a private CA
	Topics       []string `json:"topics"`            // filters, such as factory/+/telemetry
	// QoS asked for on every topic: 0, 1 (the default) or 2.
	QoS *int `json:"qos,omitempty"`
	// Dir holds the files; Name prefixes them and names the recording.
	Dir  string `json:"dir"`
	Name string `json:"name,omitempty"`
	// A file is finished after RollSeconds (default 60) or RollBytes
	// (default 64 MiB), whichever comes first.
	RollSeconds        int   `json:"roll_seconds,omitempty"`
	RollBytes          int64 `json:"roll_bytes,omitempty"`
	KeepAliveSeconds   int   `json:"keepalive_seconds,omitempty"`
	DeleteAfterConfirm bool  `json:"delete_after_confirm,omitempty"`
}

const (
	mqttMaxPacket = 64 << 20
	mqttAckEvery  = 200 * time.Millisecond
)

func (m *MQTT) withDefaults() {
	if m.Name == "" {
		m.Name = "mqtt"
	}
	if m.RollSeconds <= 0 {
		m.RollSeconds = 60
	}
	if m.RollBytes <= 0 {
		m.RollBytes = 64 << 20
	}
	if m.KeepAliveSeconds <= 0 {
		m.KeepAliveSeconds = 30
	}
	if m.QoS == nil {
		one := 1
		m.QoS = &one
	}
	if m.ClientID == "" {
		host, _ := os.Hostname()
		m.ClientID = "osp-agent-" + host
	}
}

func (m *MQTT) validate() error {
	u, err := url.Parse(m.Broker)
	if err != nil || u.Host == "" {
		return fmt.Errorf("mqtt broker %q is not an address such as tcp://host:1883", m.Broker)
	}
	switch u.Scheme {
	case "tcp", "mqtt", "tls", "ssl", "mqtts":
	default:
		return fmt.Errorf("mqtt broker scheme %q is not tcp or tls", u.Scheme)
	}
	if len(m.Topics) == 0 {
		return errors.New("mqtt needs at least one topic")
	}
	if m.Dir == "" {
		return errors.New("mqtt needs dir, where its files are kept")
	}
	if m.QoS != nil && (*m.QoS < 0 || *m.QoS > 2) {
		return errors.New("mqtt qos is 0, 1 or 2")
	}
	if strings.ContainsAny(m.Name, `/\`) {
		return errors.New("mqtt name cannot hold a path separator")
	}
	return nil
}

// watch is the recording watch that sends the finished files.
func (m *MQTT) watch() Watch {
	return Watch{Path: m.Dir, Mode: ModeRecording, RecordingPattern: `^(` + regexp.QuoteMeta(m.Name) + `)_\d+\.jsonl$`,
		ChunkSeconds: m.RollSeconds, DeleteAfterConfirm: m.DeleteAfterConfirm}
}

// runMQTT keeps a session open until ctx ends, reconnecting with backoff.
func (a *Agent) runMQTT(ctx context.Context) {
	m := a.cfg.MQTT
	w := &rollingWriter{dir: m.Dir, name: m.Name, roll: time.Duration(m.RollSeconds) * time.Second, maxBytes: m.RollBytes, now: a.now}
	if err := w.recover(); err != nil {
		a.log.Warn("mqtt: finishing files left by a stop", "error", err)
	}
	defer func() {
		if err := w.finish(); err != nil {
			a.log.Warn("mqtt: closing the open file", "error", err)
		}
	}()
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := a.mqttSession(ctx, w)
		if ctx.Err() != nil {
			return
		}
		// Only a session rolls the open file, so roll it here too, or what
		// it holds waits for the broker to come back.
		if err := w.rollIfDue(); err != nil {
			a.log.Warn("mqtt: closing the open file", "error", err)
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		a.log.Warn("mqtt: session ended; reconnecting", "error", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

type mqttPacket struct {
	kind byte // the fixed header's first byte: type and flags
	body []byte
}

// mqttSession runs one connection: connect, subscribe, then keep every
// message until the connection or ctx ends.
func (a *Agent) mqttSession(ctx context.Context, w *rollingWriter) error {
	m := a.cfg.MQTT
	conn, err := dialMQTT(ctx, m)
	if err != nil {
		return err
	}
	defer conn.Close()
	br := bufio.NewReader(conn)

	password := ""
	if m.PasswordFile != "" {
		raw, err := os.ReadFile(m.PasswordFile)
		if err != nil {
			return err
		}
		password = strings.TrimSpace(string(raw))
	}
	if err := writePacket(conn, 0x10, connectBody(m, password)); err != nil {
		return err
	}
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	p, err := readPacket(br)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	if p.kind>>4 != 2 || len(p.body) < 2 || p.body[1] != 0 {
		return fmt.Errorf("the broker refused the connection (code %v)", p.body)
	}
	if err := writePacket(conn, 0x82, subscribeBody(1, m.Topics, byte(*m.QoS))); err != nil {
		return err
	}
	_ = conn.SetReadDeadline(time.Time{})
	a.log.Info("mqtt: connected", "broker", m.Broker, "topics", len(m.Topics))

	packets := make(chan mqttPacket)
	failed := make(chan error, 1)
	done := make(chan struct{})
	defer close(done) // and conn.Close above ends the read
	go func() {
		for {
			p, err := readPacket(br)
			if err != nil {
				failed <- err
				return
			}
			select {
			case packets <- p:
			case <-done:
				return
			}
		}
	}()

	keepAlive := time.Duration(m.KeepAliveSeconds) * time.Second
	ticker := time.NewTicker(mqttAckEvery)
	defer ticker.Stop()
	lastSent, lastHeard := time.Now(), time.Now()
	var acks, recs []uint16       // QoS 1 PUBACKs and QoS 2 PUBRECs owed once synced
	inflight := map[uint16]bool{} // QoS 2 messages kept, awaiting PUBREL

	// flush syncs what was written, and only then acknowledges it.
	flush := func() error {
		if len(acks) == 0 && len(recs) == 0 {
			return nil
		}
		if err := w.sync(); err != nil {
			return err
		}
		for _, id := range acks {
			if err := writePacket(conn, 0x40, binary.BigEndian.AppendUint16(nil, id)); err != nil {
				return err
			}
		}
		for _, id := range recs {
			if err := writePacket(conn, 0x50, binary.BigEndian.AppendUint16(nil, id)); err != nil {
				return err
			}
		}
		acks, recs = acks[:0], recs[:0]
		lastSent = time.Now()
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			_ = flush()
			_ = writePacket(conn, 0xE0, nil) // DISCONNECT
			return ctx.Err()
		case err := <-failed:
			_ = flush()
			return err
		case <-ticker.C:
			if err := flush(); err != nil {
				return err
			}
			if err := w.rollIfDue(); err != nil {
				return err
			}
			if time.Since(lastHeard) > keepAlive*3/2 {
				return errors.New("the broker stopped answering")
			}
			if time.Since(lastSent) >= keepAlive/2 {
				if err := writePacket(conn, 0xC0, nil); err != nil { // PINGREQ
					return err
				}
				lastSent = time.Now()
			}
		case p := <-packets:
			lastHeard = time.Now()
			switch p.kind >> 4 {
			case 3: // PUBLISH
				msg, err := parsePublish(p)
				if err != nil {
					return err
				}
				if msg.qos == 2 && inflight[msg.id] {
					recs = append(recs, msg.id) // a resend of one already kept
					break
				}
				if err := w.write(mqttLine(msg, a.now())); err != nil {
					return err
				}
				switch msg.qos {
				case 1:
					acks = append(acks, msg.id)
				case 2:
					inflight[msg.id] = true
					recs = append(recs, msg.id)
				}
				if len(acks)+len(recs) >= 256 {
					if err := flush(); err != nil {
						return err
					}
				}
			case 6: // PUBREL: the broker has let go of a QoS 2 message
				if len(p.body) < 2 {
					return errors.New("a malformed PUBREL")
				}
				id := binary.BigEndian.Uint16(p.body)
				delete(inflight, id)
				if err := writePacket(conn, 0x70, p.body[:2]); err != nil { // PUBCOMP
					return err
				}
				lastSent = time.Now()
			case 9: // SUBACK
				if len(p.body) < 3 {
					return errors.New("a malformed SUBACK")
				}
				for _, code := range p.body[2:] {
					if code == 0x80 {
						return errors.New("the broker refused a topic subscription")
					}
				}
			}
		}
	}
}

type mqttMessage struct {
	topic   string
	qos     int
	retain  bool
	id      uint16
	payload []byte
}

func parsePublish(p mqttPacket) (mqttMessage, error) {
	msg := mqttMessage{qos: int(p.kind>>1) & 3, retain: p.kind&1 == 1}
	b := p.body
	if len(b) < 2 {
		return msg, errors.New("a malformed PUBLISH")
	}
	n := int(binary.BigEndian.Uint16(b))
	if len(b) < 2+n {
		return msg, errors.New("a malformed PUBLISH topic")
	}
	msg.topic = string(b[2 : 2+n])
	b = b[2+n:]
	if msg.qos > 0 {
		if len(b) < 2 {
			return msg, errors.New("a PUBLISH without its packet id")
		}
		msg.id = binary.BigEndian.Uint16(b)
		b = b[2:]
	}
	msg.payload = b
	return msg, nil
}

// mqttLine is one message as a JSON line: a JSON payload as itself,
// anything else as base64.
func mqttLine(msg mqttMessage, at time.Time) []byte {
	line := struct {
		Topic         string          `json:"topic"`
		ReceivedAt    string          `json:"received_at"`
		QoS           int             `json:"qos"`
		Retain        bool            `json:"retain,omitempty"`
		Payload       json.RawMessage `json:"payload,omitempty"`
		PayloadBase64 string          `json:"payload_base64,omitempty"`
	}{Topic: msg.topic, ReceivedAt: at.UTC().Format(time.RFC3339Nano), QoS: msg.qos, Retain: msg.retain}
	if len(msg.payload) > 0 && json.Valid(msg.payload) {
		line.Payload = msg.payload
	} else {
		line.PayloadBase64 = base64.StdEncoding.EncodeToString(msg.payload)
	}
	out, _ := json.Marshal(line)
	return append(out, '\n')
}

func dialMQTT(ctx context.Context, m *MQTT) (net.Conn, error) {
	u, _ := url.Parse(m.Broker)
	secure := u.Scheme == "tls" || u.Scheme == "ssl" || u.Scheme == "mqtts"
	host := u.Host
	if u.Port() == "" {
		port := "1883"
		if secure {
			port = "8883"
		}
		host = net.JoinHostPort(u.Hostname(), port)
	}
	d := &net.Dialer{Timeout: 30 * time.Second}
	if !secure {
		return d.DialContext(ctx, "tcp", host)
	}
	cfg := &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12}
	if m.CAFile != "" {
		pem, err := os.ReadFile(m.CAFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s holds no certificate", m.CAFile)
		}
		cfg.RootCAs = pool
	}
	return (&tls.Dialer{NetDialer: d, Config: cfg}).DialContext(ctx, "tcp", host)
}

// ─── MQTT 3.1.1 packets ──────────────────────────────────────────────

func mqttString(out []byte, s string) []byte {
	out = binary.BigEndian.AppendUint16(out, uint16(len(s)))
	return append(out, s...)
}

func connectBody(m *MQTT, password string) []byte {
	b := mqttString(nil, "MQTT")
	b = append(b, 4) // protocol level 3.1.1
	flags := byte(0) // clean session off: the broker keeps the session
	if m.Username != "" {
		flags |= 0x80
		if password != "" {
			flags |= 0x40
		}
	}
	b = append(b, flags)
	b = binary.BigEndian.AppendUint16(b, uint16(m.KeepAliveSeconds))
	b = mqttString(b, m.ClientID)
	if m.Username != "" {
		b = mqttString(b, m.Username)
		if password != "" {
			b = mqttString(b, password)
		}
	}
	return b
}

func subscribeBody(id uint16, topics []string, qos byte) []byte {
	b := binary.BigEndian.AppendUint16(nil, id)
	for _, t := range topics {
		b = mqttString(b, t)
		b = append(b, qos)
	}
	return b
}

func writePacket(w io.Writer, kind byte, body []byte) error {
	out := []byte{kind}
	n := len(body)
	for {
		digit := byte(n % 128)
		n /= 128
		if n > 0 {
			digit |= 0x80
		}
		out = append(out, digit)
		if n == 0 {
			break
		}
	}
	_, err := w.Write(append(out, body...))
	return err
}

func readPacket(r *bufio.Reader) (mqttPacket, error) {
	kind, err := r.ReadByte()
	if err != nil {
		return mqttPacket{}, err
	}
	length, mult := 0, 1
	for i := 0; ; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return mqttPacket{}, err
		}
		length += int(b&0x7F) * mult
		if b&0x80 == 0 {
			break
		}
		if i == 3 {
			return mqttPacket{}, errors.New("a malformed packet length")
		}
		mult *= 128
	}
	if length > mqttMaxPacket {
		return mqttPacket{}, fmt.Errorf("a %d-byte message is larger than %d bytes", length, mqttMaxPacket)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return mqttPacket{}, err
	}
	return mqttPacket{kind: kind, body: body}, nil
}

// ─── Rolling files ───────────────────────────────────────────────────

// rollingWriter writes lines to <name>_<unix nanoseconds>.jsonl.part and
// renames it to .jsonl once finished, so the recording watch only ever sees
// whole files.
type rollingWriter struct {
	dir      string
	name     string
	roll     time.Duration
	maxBytes int64
	now      func() time.Time

	f       *os.File
	opened  time.Time
	written int64
}

func (w *rollingWriter) write(line []byte) error {
	if w.f == nil {
		if err := os.MkdirAll(w.dir, 0o755); err != nil {
			return err
		}
		w.opened = w.now()
		file := filepath.Join(w.dir, fmt.Sprintf("%s_%d.jsonl.part", w.name, w.opened.UnixNano()))
		f, err := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		w.f, w.written = f, 0
	}
	n, err := w.f.Write(line)
	w.written += int64(n)
	if err != nil {
		return err
	}
	if w.written >= w.maxBytes {
		return w.finish()
	}
	return nil
}

func (w *rollingWriter) sync() error {
	if w.f == nil {
		return nil
	}
	return w.f.Sync()
}

func (w *rollingWriter) rollIfDue() error {
	if w.f != nil && w.now().Sub(w.opened) >= w.roll {
		return w.finish()
	}
	return nil
}

// finish syncs, closes and renames the open file.
func (w *rollingWriter) finish() error {
	if w.f == nil {
		return nil
	}
	f := w.f
	w.f = nil
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), strings.TrimSuffix(f.Name(), ".part"))
}

// recover finishes files a stop left open: each is cut back to its last
// whole line, since a line written after the last sync may be torn.
func (w *rollingWriter) recover() error {
	files, err := filepath.Glob(filepath.Join(w.dir, w.name+"_*.jsonl.part"))
	if err != nil {
		return err
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		keep := len(data)
		if i := strings.LastIndexByte(string(data), '\n'); i+1 < len(data) {
			keep = i + 1
		}
		if keep == 0 {
			if err := os.Remove(file); err != nil {
				return err
			}
			continue
		}
		if err := os.Truncate(file, int64(keep)); err != nil {
			return err
		}
		if err := os.Rename(file, strings.TrimSuffix(file, ".part")); err != nil {
			return err
		}
	}
	return nil
}
