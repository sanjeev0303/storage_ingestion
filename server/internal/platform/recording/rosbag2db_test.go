package recording

import (
	"bytes"
	"os"
	"testing"
)

// testdata/rosbag2.db3 was written by Python's sqlite3 with the rosbag2
// schema and 512-byte pages, so the messages table has interior pages and
// the camera messages and QoS text spill onto overflow pages. It holds 160
// messages 10 ms apart: every eighth on /camera/image, the rest on /imu.
func TestParseRosbag2DB(t *testing.T) {
	data, err := os.ReadFile("testdata/rosbag2.db3")
	if err != nil {
		t.Fatal(err)
	}
	sum, err := ParseRosbag2DB(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	const start = 1730000000000000000
	if sum.Kind != KindRosbag2 || sum.MessageCount != 160 || sum.StartNS != start || sum.EndNS != start+159*10_000_000 {
		t.Fatalf("summary %+v", sum)
	}
	want := map[string]int64{"/camera/image": 20, "/imu": 140, "/unused": 0}
	if len(sum.Topics) != 3 {
		t.Fatalf("topics %+v", sum.Topics)
	}
	for _, tp := range sum.Topics {
		if tp.MessageCount != want[tp.Topic] || tp.Encoding != "cdr" || tp.Schema == "" {
			t.Fatalf("topic %+v", tp)
		}
	}

	// A copy cut short in the middle of the messages table is refused, not misread.
	if _, err := ParseRosbag2DB(bytes.NewReader(data[:len(data)/2]), int64(len(data)/2)); err == nil {
		t.Fatal("a cut-short database was read without error")
	}
	if _, err := ParseRosbag2DB(bytes.NewReader([]byte("not sqlite")), 10); err == nil {
		t.Fatal("not a database")
	}
}

func TestColumnIndexes(t *testing.T) {
	got := columnIndexes(`CREATE TABLE "messages" (id INTEGER PRIMARY KEY, topic_id INTEGER NOT NULL, "timestamp" INTEGER, data BLOB, CHECK (topic_id > 0))`, "topic_id", "timestamp", "missing")
	if got["topic_id"] != 1 || got["timestamp"] != 2 || got["missing"] != -1 {
		t.Fatal(got)
	}
}
