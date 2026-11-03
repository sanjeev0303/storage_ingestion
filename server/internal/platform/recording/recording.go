// Package recording reads what robotics recordings say about themselves: an
// MCAP file's summary section, and a rosbag2 folder's metadata.yaml. Only the
// metadata is read; a terabyte recording costs a few ranged reads.
package recording

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Kinds of recording.
const (
	KindMCAP    = "MCAP"
	KindRosbag2 = "ROSBAG2"
)

// Topic is one channel of a recording.
type Topic struct {
	Topic        string `json:"topic"`
	Schema       string `json:"schema,omitempty"`
	Encoding     string `json:"encoding,omitempty"`
	MessageCount int64  `json:"message_count"`
}

// Summary is what a recording holds.
type Summary struct {
	Kind         string
	StartNS      int64
	EndNS        int64
	MessageCount int64
	Topics       []Topic
}

const (
	mcapMagic   = "\x89MCAP0\r\n"
	opFooter    = 0x02
	opSchema    = 0x03
	opChannel   = 0x04
	opStats     = 0x0B
	footerBytes = 1 + 8 + 20
	// MCAPTailBytes is the footer record and the closing magic.
	MCAPTailBytes = footerBytes + len(mcapMagic)
	// A summary section past this is not believed.
	maxSummaryBytes = 64 << 20
)

// MCAPSummaryRange reads a file's last MCAPTailBytes and answers where its
// summary section is, as an offset and length.
func MCAPSummaryRange(tail []byte, size int64) (int64, int64, error) {
	if len(tail) != MCAPTailBytes || string(tail[footerBytes:]) != mcapMagic {
		return 0, 0, errors.New("not an MCAP file: the closing magic is missing, it may be cut short")
	}
	if tail[0] != opFooter || binary.LittleEndian.Uint64(tail[1:9]) != 20 {
		return 0, 0, errors.New("the MCAP footer is malformed")
	}
	start := binary.LittleEndian.Uint64(tail[9:17])
	if start == 0 {
		return 0, 0, errors.New("the MCAP file has no summary section")
	}
	footerAt := size - int64(MCAPTailBytes)
	if start > uint64(footerAt) {
		return 0, 0, errors.New("the MCAP footer points past the end of the file")
	}
	length := footerAt - int64(start)
	if length > maxSummaryBytes {
		return 0, 0, fmt.Errorf("the MCAP summary is larger than %d MiB", maxSummaryBytes>>20)
	}
	return int64(start), length, nil
}

type mcapReader struct {
	b   []byte
	err error
}

func (r *mcapReader) take(n uint64) []byte {
	if r.err != nil {
		return nil
	}
	if n > uint64(len(r.b)) {
		r.err = errors.New("the MCAP summary is cut short")
		return nil
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}

func (r *mcapReader) u16() uint16 {
	if b := r.take(2); b != nil {
		return binary.LittleEndian.Uint16(b)
	}
	return 0
}

func (r *mcapReader) u32() uint32 {
	if b := r.take(4); b != nil {
		return binary.LittleEndian.Uint32(b)
	}
	return 0
}

func (r *mcapReader) u64() uint64 {
	if b := r.take(8); b != nil {
		return binary.LittleEndian.Uint64(b)
	}
	return 0
}

func (r *mcapReader) str() string {
	return string(r.take(uint64(r.u32())))
}

// ParseMCAPSummary reads the summary section: its schemas, channels and
// statistics. Without statistics a file does not say what it holds, and that
// is an error rather than a recording of nothing.
func ParseMCAPSummary(section []byte) (Summary, error) {
	out := Summary{Kind: KindMCAP}
	schemas := map[uint16]string{}
	type channel struct {
		schema   uint16
		topic    string
		encoding string
	}
	channels := map[uint16]channel{}
	counts := map[uint16]uint64{}
	found := false
	r := &mcapReader{b: section}
	for len(r.b) > 0 && r.err == nil {
		op := r.take(1)
		length := r.u64()
		body := r.take(length)
		if r.err != nil {
			break
		}
		c := &mcapReader{b: body}
		switch op[0] {
		case opSchema:
			id := c.u16()
			schemas[id] = c.str()
		case opChannel:
			id := c.u16()
			schema := c.u16()
			topic := c.str()
			channels[id] = channel{schema: schema, topic: topic, encoding: c.str()}
		case opStats:
			found = true
			out.MessageCount = int64(c.u64())
			c.u16() // schemas
			c.u32() // channels
			c.u32() // attachments
			c.u32() // metadata
			c.u32() // chunks
			out.StartNS = int64(c.u64())
			out.EndNS = int64(c.u64())
			m := &mcapReader{b: c.take(uint64(c.u32()))}
			for len(m.b) > 0 && m.err == nil {
				id := m.u16()
				counts[id] = m.u64()
			}
			if m.err != nil {
				c.err = m.err
			}
		}
		if c.err != nil {
			r.err = c.err
		}
	}
	if r.err != nil {
		return out, r.err
	}
	if !found {
		return out, errors.New("the MCAP summary has no statistics record")
	}
	for id, ch := range channels {
		out.Topics = append(out.Topics, Topic{Topic: ch.topic, Schema: schemas[ch.schema], Encoding: ch.encoding, MessageCount: int64(counts[id])})
	}
	sort.Slice(out.Topics, func(a, b int) bool { return out.Topics[a].Topic < out.Topics[b].Topic })
	return out, nil
}

// ParseRosbag2 reads a rosbag2 metadata.yaml.
//
// ponytail: a line reader for the one layout ros2 bag writes, not a YAML
// parser; a hand-edited file with other indentation or flow style is refused
// or read short. Move to a YAML library if such files turn up.
func ParseRosbag2(text string) (Summary, error) {
	out := Summary{Kind: KindRosbag2}
	if !strings.Contains(text, "rosbag2_bagfile_information") {
		return out, errors.New("not a rosbag2 metadata.yaml")
	}
	var durationNS int64
	section := ""
	inTopics := false
	var current *Topic
	finish := func() {
		if current != nil && current.Topic != "" {
			out.Topics = append(out.Topics, *current)
		}
		current = nil
	}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimRight(raw, " \r")
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if inTopics && indent <= 2 && !strings.HasPrefix(trimmed, "- ") {
			finish()
			inTopics = false
		}
		key, value, _ := strings.Cut(strings.TrimPrefix(trimmed, "- "), ":")
		key, value = strings.TrimSpace(key), strings.Trim(strings.TrimSpace(value), `"'`)
		switch {
		case key == "topics_with_message_count":
			inTopics = true
		case inTopics:
			switch key {
			case "topic_metadata":
				finish()
				current = &Topic{}
			case "name":
				if current != nil {
					current.Topic = value
				}
			case "type":
				if current != nil {
					current.Schema = value
				}
			case "serialization_format":
				if current != nil {
					current.Encoding = value
				}
			case "message_count":
				if current != nil {
					current.MessageCount, _ = strconv.ParseInt(value, 10, 64)
				}
			}
		case key == "duration" || key == "starting_time":
			section = key
		case key == "nanoseconds" && section == "duration":
			durationNS, _ = strconv.ParseInt(value, 10, 64)
		case key == "nanoseconds_since_epoch" && section == "starting_time":
			out.StartNS, _ = strconv.ParseInt(value, 10, 64)
		case key == "message_count" && indent <= 2:
			out.MessageCount, _ = strconv.ParseInt(value, 10, 64)
		}
	}
	finish()
	if out.StartNS == 0 && out.MessageCount == 0 && len(out.Topics) == 0 {
		return out, errors.New("the rosbag2 metadata names no start time, messages or topics")
	}
	out.EndNS = out.StartNS + durationNS
	sort.Slice(out.Topics, func(a, b int) bool { return out.Topics[a].Topic < out.Topics[b].Topic })
	return out, nil
}
