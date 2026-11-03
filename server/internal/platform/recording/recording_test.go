package recording

import (
	"encoding/binary"
	"testing"
)

func record(op byte, body []byte) []byte {
	out := []byte{op}
	out = binary.LittleEndian.AppendUint64(out, uint64(len(body)))
	return append(out, body...)
}

func mstr(b []byte, s string) []byte {
	b = binary.LittleEndian.AppendUint32(b, uint32(len(s)))
	return append(b, s...)
}

// BuildMCAP makes a small real MCAP file: magic, no data, summary, footer.
func buildMCAP(t *testing.T) []byte {
	t.Helper()
	file := []byte(mcapMagic)
	file = append(file, record(0x01, mstr(mstr(nil, ""), ""))...) // header

	var schema []byte
	schema = binary.LittleEndian.AppendUint16(schema, 1)
	schema = mstr(schema, "sensor_msgs/Imu")
	schema = mstr(schema, "ros2msg")
	schema = mstr(schema, "")

	var channel []byte
	channel = binary.LittleEndian.AppendUint16(channel, 7)
	channel = binary.LittleEndian.AppendUint16(channel, 1)
	channel = mstr(channel, "/imu")
	channel = mstr(channel, "cdr")
	channel = binary.LittleEndian.AppendUint32(channel, 0)

	var stats []byte
	stats = binary.LittleEndian.AppendUint64(stats, 42)
	stats = binary.LittleEndian.AppendUint16(stats, 1)
	stats = binary.LittleEndian.AppendUint32(stats, 1)
	stats = binary.LittleEndian.AppendUint32(stats, 0)
	stats = binary.LittleEndian.AppendUint32(stats, 0)
	stats = binary.LittleEndian.AppendUint32(stats, 1)
	stats = binary.LittleEndian.AppendUint64(stats, 1_000)
	stats = binary.LittleEndian.AppendUint64(stats, 9_000)
	stats = binary.LittleEndian.AppendUint32(stats, 10)
	stats = binary.LittleEndian.AppendUint16(stats, 7)
	stats = binary.LittleEndian.AppendUint64(stats, 42)

	start := uint64(len(file))
	file = append(file, record(opSchema, schema)...)
	file = append(file, record(opChannel, channel)...)
	file = append(file, record(opStats, stats)...)

	var footer []byte
	footer = binary.LittleEndian.AppendUint64(footer, start)
	footer = binary.LittleEndian.AppendUint64(footer, 0)
	footer = binary.LittleEndian.AppendUint32(footer, 0)
	file = append(file, record(opFooter, footer)...)
	return append(file, mcapMagic...)
}

func TestMCAP(t *testing.T) {
	file := buildMCAP(t)
	size := int64(len(file))
	off, length, err := MCAPSummaryRange(file[size-int64(MCAPTailBytes):], size)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ParseMCAPSummary(file[off : off+length])
	if err != nil {
		t.Fatal(err)
	}
	if s.Kind != KindMCAP || s.StartNS != 1_000 || s.EndNS != 9_000 || s.MessageCount != 42 || len(s.Topics) != 1 {
		t.Fatalf("%+v", s)
	}
	if got := s.Topics[0]; got.Topic != "/imu" || got.Schema != "sensor_msgs/Imu" || got.Encoding != "cdr" || got.MessageCount != 42 {
		t.Fatalf("%+v", got)
	}
}

func TestMCAPRefusals(t *testing.T) {
	file := buildMCAP(t)
	size := int64(len(file))
	tail := file[size-int64(MCAPTailBytes):]

	if _, _, err := MCAPSummaryRange(tail[:len(tail)-1], size); err == nil {
		t.Fatal("a short tail was read")
	}
	cut := append([]byte{}, tail...)
	cut[len(cut)-1] = 'x'
	if _, _, err := MCAPSummaryRange(cut, size); err == nil {
		t.Fatal("a file cut short was read")
	}
	none := append([]byte{}, tail...)
	binary.LittleEndian.PutUint64(none[9:], 0)
	if _, _, err := MCAPSummaryRange(none, size); err == nil {
		t.Fatal("a file with no summary was read")
	}
	past := append([]byte{}, tail...)
	binary.LittleEndian.PutUint64(past[9:], uint64(size))
	if _, _, err := MCAPSummaryRange(past, size); err == nil {
		t.Fatal("a footer pointing past the end was read")
	}

	off, length, _ := MCAPSummaryRange(tail, size)
	section := file[off : off+length]
	if _, err := ParseMCAPSummary(section[:len(section)-3]); err == nil {
		t.Fatal("a cut summary was read")
	}
	if _, err := ParseMCAPSummary(record(opSchema, mstr([]byte{1, 0}, "x"))); err == nil {
		t.Fatal("a summary with no statistics was read")
	}
}

const bagYAML = `rosbag2_bagfile_information:
  version: 5
  storage_identifier: sqlite3
  duration:
    nanoseconds: 5000
  starting_time:
    nanoseconds_since_epoch: 1000
  message_count: 30
  topics_with_message_count:
    - topic_metadata:
        name: /camera
        type: sensor_msgs/msg/Image
        serialization_format: cdr
        offered_qos_profiles: ""
      message_count: 10
    - topic_metadata:
        name: /imu
        type: sensor_msgs/msg/Imu
        serialization_format: cdr
        offered_qos_profiles: ""
      message_count: 20
  compression_format: ""
  relative_file_paths:
    - bag_0.db3
`

func TestRosbag2(t *testing.T) {
	s, err := ParseRosbag2(bagYAML)
	if err != nil {
		t.Fatal(err)
	}
	if s.Kind != KindRosbag2 || s.StartNS != 1000 || s.EndNS != 6000 || s.MessageCount != 30 || len(s.Topics) != 2 {
		t.Fatalf("%+v", s)
	}
	if got := s.Topics[1]; got.Topic != "/imu" || got.Schema != "sensor_msgs/msg/Imu" || got.MessageCount != 20 {
		t.Fatalf("%+v", got)
	}
	if _, err := ParseRosbag2("name: other"); err == nil {
		t.Fatal("another YAML file was read")
	}
}
