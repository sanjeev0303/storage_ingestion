package processing

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

func sampleOf(name string, data []byte) interfaces.ContentSample {
	head := data
	if len(head) > sniffBytes {
		head = head[:sniffBytes]
	}
	return interfaces.ContentSample{
		Head: head, FileName: name, SizeBytes: int64(len(data)), DetectedType: DetectType(head), AssetID: "a1",
		Open: func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil },
	}
}

var testInspector = ContentInspector{
	Limits:   ArchiveLimits{MaxArchiveBytes: 1 << 30, MaxEntries: 1000, MaxUncompressedBytes: 1 << 30, MaxRatio: 100},
	MaxBytes: 1 << 30, SecretScanBytes: 1 << 20,
}

// codes runs the inspector and returns the finding codes it reported.
func codes(t *testing.T, name string, data []byte) []string {
	t.Helper()
	result, err := testInspector.Validate(context.Background(), sampleOf(name, data), model.ContentPolicy{})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var out []string
	for _, f := range result.Findings {
		out = append(out, f.Code)
	}
	return out
}

func expectCodes(t *testing.T, name string, data []byte, want ...string) {
	t.Helper()
	got := codes(t, name, data)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s: findings %v, want %v", name, got, want)
	}
}

func TestPickleImports(t *testing.T) {
	// os.system("ls"), protocol 2.
	evil := []byte("\x80\x02cos\nsystem\nq\x00X\x02\x00\x00\x00lsq\x01\x85q\x02Rq\x03.")
	expectCodes(t, "model.pkl", evil, "model.unsafe_pickle")

	// builtins.eval through STACK_GLOBAL, protocol 4, then again from the memo.
	stack := []byte("\x80\x04\x8c\x08builtins\x94\x8c\x04eval\x94\x93\x94\x8c\x031+1\x94\x85\x94R\x94.")
	expectCodes(t, "model.pt", stack, "model.unsafe_pickle")
	memo := []byte("\x80\x04\x8c\x03abc\x94\x8c\x03def\x94\x30\x30h\x00h\x01\x93\x94).")
	if got := codes(t, "m.pkl", memo); len(got) != 1 || got[0] != "model.pickle_imports" {
		t.Fatalf("memo GETs: %v", got)
	}
	memoEvil := []byte("\x80\x04\x8c\x02os\x94\x8c\x06system\x94\x30\x30h\x00h\x01\x93\x94).")
	expectCodes(t, "m.pkl", memoEvil, "model.unsafe_pickle")

	// Plain containers and tensors import nothing unsafe.
	safe := []byte("\x80\x02ccollections\nOrderedDict\nq\x00)Rq\x01ctorch\nFloatStorage\nq\x02.")
	expectCodes(t, "weights.pth", safe)

	// A model's own class needs its code to load: flagged, not rejected.
	own := []byte("\x80\x02cmymodel.net\nNet\nq\x00)\x81q\x01.")
	expectCodes(t, "net.pkl", own, "model.pickle_imports")

	expectCodes(t, "broken.pkl", []byte("\x80\x02cos\nsys"), "model.invalid_pickle")
}

func TestPyTorchZipAndNPY(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("archive/data.pkl")
	w.Write([]byte("\x80\x02cposix\nsystem\nq\x00X\x02\x00\x00\x00idq\x01\x85Rq\x02."))
	w, _ = zw.Create("archive/data/0")
	w.Write(make([]byte, 64))
	zw.Close()
	expectCodes(t, "model.pt", buf.Bytes(), "model.unsafe_pickle")

	header := "{'descr': '|O', 'fortran_order': False, 'shape': (1,), }"
	header += strings.Repeat(" ", 64-(10+len(header)+1)%64) + "\n"
	npy := append([]byte("\x93NUMPY\x01\x00"), byte(len(header)), byte(len(header)>>8))
	npy = append(npy, header...)
	npy = append(npy, "\x80\x02cos\nsystem\nq\x00X\x01\x00\x00\x00xq\x01\x85R."...)
	expectCodes(t, "x.npy", npy, "model.npy_object_array", "model.unsafe_pickle")
}

func safetensors(header string, data int) []byte {
	out := binary.LittleEndian.AppendUint64(nil, uint64(len(header)))
	out = append(out, header...)
	return append(out, make([]byte, data)...)
}

func TestSafetensors(t *testing.T) {
	good := safetensors(`{"w":{"dtype":"F32","shape":[2,2],"data_offsets":[0,16]},"__metadata__":{"a":"b"}}`, 16)
	expectCodes(t, "m.safetensors", good)
	outside := safetensors(`{"w":{"dtype":"F32","shape":[2,2],"data_offsets":[0,16]}}`, 8)
	expectCodes(t, "m.safetensors", outside, "model.invalid_safetensors")
	shape := safetensors(`{"w":{"dtype":"F16","shape":[3],"data_offsets":[0,16]}}`, 16)
	expectCodes(t, "m.safetensors", shape, "model.invalid_safetensors")
	expectCodes(t, "m.safetensors", []byte("not a safetensors file at all"), "model.invalid_safetensors")
}

func deflate(s string) []byte {
	var b bytes.Buffer
	zw := zlib.NewWriter(&b)
	zw.Write([]byte(s))
	zw.Close()
	return b.Bytes()
}

func TestPDFActiveContent(t *testing.T) {
	plain := []byte("%PDF-1.7\n1 0 obj << /Type /Catalog >> endobj\ntrailer << /Root 1 0 R >>\n%%EOF")
	expectCodes(t, "a.pdf", plain)

	escaped := []byte("%PDF-1.7\n1 0 obj << /OpenAction << /S /J#61vaScript /JS (app.alert(1)) >> >> endobj\n%%EOF")
	expectCodes(t, "a.pdf", escaped, "active_content.pdf_javascript")

	// Hidden inside a compressed object stream.
	hidden := []byte("%PDF-1.7\n2 0 obj << /Type /ObjStm /Filter /FlateDecode >>\nstream\n")
	hidden = append(hidden, deflate("<< /S /Launch /F (cmd.exe) >>")...)
	hidden = append(hidden, "\nendstream\nendobj\n%%EOF"...)
	expectCodes(t, "a.pdf", hidden, "active_content.pdf_launch")
}

func TestOfficeMacros(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range []string{"[Content_Types].xml", "word/document.xml", "word/vbaProject.bin"} {
		w, _ := zw.Create(name)
		w.Write([]byte("x"))
	}
	zw.Close()
	expectCodes(t, "report.docm", buf.Bytes(), "active_content.ooxml_macro")

	ole := append(append([]byte{}, oleMagic...), make([]byte, 600)...)
	ole = append(ole, utf16le("_VBA_PROJECT")...)
	expectCodes(t, "old.doc", ole, "active_content.ole_macro")
	expectCodes(t, "clean.doc", append(append([]byte{}, oleMagic...), make([]byte, 1000)...))
}

func gz(data []byte, extra []byte) []byte {
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	zw.Header.Extra = extra
	zw.Write(data)
	zw.Close()
	return b.Bytes()
}

func TestGzipIntegrity(t *testing.T) {
	good := gz([]byte("hello world\n"), nil)
	expectCodes(t, "a.txt.gz", good)
	bad := append([]byte{}, good...)
	bad[len(bad)-6] ^= 0xff // CRC-32
	expectCodes(t, "a.txt.gz", bad, "format.corrupt_gzip")
	expectCodes(t, "a.txt.gz", good[:len(good)-4], "format.corrupt_gzip")
}

func bamRecord(ref int32) []byte {
	body := make([]byte, 32)
	binary.LittleEndian.PutUint32(body[0:], uint32(ref))
	body[8] = 2 // read name length, with its NUL
	binary.LittleEndian.PutUint32(body[20:], uint32(0xffffffff))
	body = append(body, 'r', 0)
	return append(binary.LittleEndian.AppendUint32(nil, uint32(len(body))), body...)
}

func bam(records ...[]byte) []byte {
	out := []byte("BAM\x01")
	out = binary.LittleEndian.AppendUint32(out, 0) // no header text
	out = binary.LittleEndian.AppendUint32(out, 1) // one reference
	out = binary.LittleEndian.AppendUint32(out, 5)
	out = append(out, "chr1\x00"...)
	out = binary.LittleEndian.AppendUint32(out, 1000)
	for _, r := range records {
		out = append(out, r...)
	}
	return out
}

func TestBAM(t *testing.T) {
	bc := []byte{'B', 'C', 2, 0, 0, 0}
	good := append(gz(bam(bamRecord(0), bamRecord(-1)), bc), bgzfEOF...)
	expectCodes(t, "reads.bam", good)
	expectCodes(t, "reads.bam", gz(bam(bamRecord(0)), bc), "format.truncated_bgzf")
	badRef := append(gz(bam(bamRecord(3)), bc), bgzfEOF...)
	expectCodes(t, "reads.bam", badRef, "format.invalid_bam")
	cut := bam(bamRecord(0))
	expectCodes(t, "reads.bam", append(gz(cut[:len(cut)-5], bc), bgzfEOF...), "format.invalid_bam")
}

func TestFASTQ(t *testing.T) {
	expectCodes(t, "r.fastq", []byte("@r1\nACGT\n+\nIIII\n@r2\nAC\n+\nII\n"))
	expectCodes(t, "r.fastq", []byte("@r1\nACGT\n+\nIII\n"), "format.invalid_fastq")
	expectCodes(t, "r.fq", []byte("@r1\nACGT\n+\nIIII\n@r2\nAC\n"), "format.invalid_fastq")
	expectCodes(t, "r.fastq.gz", gz([]byte("@r1\nACGT\n+\nIIII\n"), nil))
	expectCodes(t, "r.fastq.gz", gz([]byte("r1\nACGT\n+\nIIII\n"), nil), "format.invalid_fastq")
}

func mcapRecord(op byte, body string) []byte {
	return append(binary.LittleEndian.AppendUint64([]byte{op}, uint64(len(body))), body...)
}

func TestMCAP(t *testing.T) {
	good := append([]byte{}, mcapMagic...)
	good = append(good, mcapRecord(0x01, "header")...)
	good = append(good, mcapRecord(0x05, "message")...)
	good = append(good, mcapRecord(0x02, "footer..")...)
	good = append(good, mcapMagic...)
	expectCodes(t, "run.mcap", good)
	cut := good[:len(good)-len(mcapMagic)-len(mcapRecord(0x02, "footer.."))]
	expectCodes(t, "run.mcap", cut, "format.invalid_mcap")
	expectCodes(t, "run.mcap", []byte("not an mcap file"), "format.invalid_mcap")
}

func TestTarEntries(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "ok/file.txt", Size: 2, Mode: 0o644})
	tw.Write([]byte("hi"))
	tw.WriteHeader(&tar.Header{Name: "../../etc/cron.d/x", Size: 0, Mode: 0o644})
	tw.Close()
	expectCodes(t, "a.tar", buf.Bytes(), "format.unsafe_archive_entry")
	expectCodes(t, "a.tar.gz", gz(buf.Bytes(), nil), "format.unsafe_archive_entry")

	buf.Reset()
	tw = tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../../../root/.ssh"})
	tw.Close()
	expectCodes(t, "b.tar", buf.Bytes(), "format.unsafe_archive_entry")
}

func TestSecrets(t *testing.T) {
	text := "region=us-east-1\naws_access_key_id = AKIAIOSFODNN7EXAMPLE\n-----BEGIN RSA PRIVATE KEY-----\nMIIE\n"
	expectCodes(t, "config.env", []byte(text), "secret.aws_access_key", "secret.private_key")
	expectCodes(t, "notes.txt", []byte("nothing to see here\n"))

	// A key split across two reads is still found, and counted once.
	padding := strings.Repeat("x", (1<<20)-10)
	big := []byte(padding + " ghp_" + strings.Repeat("a", 36) + " end")
	wide := testInspector
	wide.SecretScanBytes = 4 << 20
	got, _ := wide.Validate(context.Background(), sampleOf("big.txt", big), model.ContentPolicy{})
	if len(got.Findings) != 1 || got.Findings[0].Code != "secret.github_token" || !strings.Contains(got.Findings[0].Detail, "(1 found)") {
		t.Fatalf("split secret: %+v", got.Findings)
	}
	if strings.Contains(got.Findings[0].Detail, "ghp_") {
		t.Fatal("a finding must never carry the secret")
	}
	// Past the scan limit nothing is claimed, and a note says so.
	cut, _ := testInspector.Validate(context.Background(), sampleOf("big.txt", big), model.ContentPolicy{})
	if len(cut.Findings) != 0 || len(cut.Notes) != 1 || !strings.Contains(cut.Notes[0], "not scanned") {
		t.Fatalf("limit: %+v", cut)
	}
}

func TestFindingActionsFromPolicy(t *testing.T) {
	data := []byte("key AKIAIOSFODNN7EXAMPLE\n")
	run := func(actions map[string]string) interfaces.ValidationResult {
		r, err := RunValidators(context.Background(), []interfaces.FileValidator{testInspector}, sampleOf("a.txt", data),
			model.ContentPolicy{FindingActions: actions})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := run(nil); r.RejectReason != "" || len(r.Findings) != 1 || r.Findings[0].Action != model.FindingFlag {
		t.Fatalf("default: %+v", r)
	}
	if r := run(map[string]string{"secret": "reject"}); r.RejectReason != model.RejectContentCheck || r.Findings[0].Source != "secrets" {
		t.Fatalf("family reject: %+v", r)
	}
	if r := run(map[string]string{"secret.aws_access_key": "allow"}); len(r.Findings) != 0 || r.RejectReason != "" {
		t.Fatalf("allow: %+v", r)
	}
	if model.FindingAction("malware.detected_on_rescan", "", map[string]string{"malware": "allow"}) != model.FindingReject {
		t.Fatal("malware findings must always reject")
	}
	if err := model.ValidateFindingActions(map[string]string{"malware": "flag"}); err == nil {
		t.Fatal("loosening malware must be refused")
	}
	if err := model.ValidateFindingActions(map[string]string{"nonsense.x": "flag"}); err == nil {
		t.Fatal("unknown families must be refused")
	}
}

func TestHookChecker(t *testing.T) {
	var gotBody, gotAuth, gotAsset string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotAuth, gotAsset = string(b), r.Header.Get("Authorization"), r.Header.Get("X-OSP-Asset-Id")
		json.NewEncoder(w).Encode(map[string]any{"findings": []map[string]string{{"code": "pii.ssn", "action": "reject", "detail": "2 SSNs"}}})
	}))
	defer srv.Close()
	hook := HookChecker{URL: srv.URL, Token: "t0k"}
	r, err := RunValidators(context.Background(), []interfaces.FileValidator{hook}, sampleOf("p.csv", []byte("name,ssn\n")), model.ContentPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody != "name,ssn\n" || gotAuth != "Bearer t0k" || gotAsset != "a1" {
		t.Fatalf("request: %q %q %q", gotBody, gotAuth, gotAsset)
	}
	if r.RejectReason != model.RejectContentCheck || r.Findings[0].Code != "external.pii.ssn" || r.Findings[0].Source != "external" {
		t.Fatalf("result: %+v", r)
	}
	// The tenant can make the engine's reject a flag.
	r, _ = RunValidators(context.Background(), []interfaces.FileValidator{hook}, sampleOf("p.csv", []byte("x")),
		model.ContentPolicy{FindingActions: map[string]string{"external": "flag"}})
	if r.RejectReason != "" || r.Findings[0].Action != model.FindingFlag {
		t.Fatalf("override: %+v", r)
	}

	down := HookChecker{URL: "http://127.0.0.1:1"}
	if _, err := down.Validate(context.Background(), sampleOf("p.csv", []byte("x")), model.ContentPolicy{}); err == nil {
		t.Fatal("an unreachable engine must be an error, never a pass")
	}
}

// A failed read of the object is retried, never taken for a broken file.
func TestReadFailureIsNotAFinding(t *testing.T) {
	data := gz([]byte("@r1\nACGT\n+\nIIII\n"), nil)
	s := sampleOf("r.fastq.gz", data)
	s.Open = func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(io.MultiReader(bytes.NewReader(data[:10]), errReader{})), nil
	}
	if _, err := testInspector.Validate(context.Background(), s, model.ContentPolicy{}); err == nil {
		t.Fatal("expected the read failure to be returned")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestDefaultFindingActions(t *testing.T) {
	for code, want := range map[string]string{
		"model.unsafe_pickle": "reject", "model.pickle_imports": "flag", "model.npy_object_array": "flag",
		"model.invalid_safetensors": "reject", "active_content.ooxml_macro": "flag", "active_content.pdf_launch": "reject",
		"secret.aws_access_key": "flag", "format.invalid_bam": "reject", "external.pii.ssn": "flag",
	} {
		if got := model.FindingAction(code, "", nil); got != want {
			t.Errorf("%s: %s, want %s", code, got, want)
		}
	}
}
