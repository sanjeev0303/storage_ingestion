package processing

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Loading a pickle runs whatever its GLOBAL and STACK_GLOBAL opcodes
// import, so a model file can carry code. The checks walk the opcodes
// without executing anything, the way picklescan and the Hugging Face Hub
// do, and judge each import:
//   - a known-dangerous one (os.system, builtins.eval, subprocess...)
//     is model.unsafe_pickle, which rejects by default;
//   - one outside the safe list (a model's own classes, sklearn) is
//     model.pickle_imports, which flags: loading it needs that code.

// safeGlobals are imports plain tensors, arrays and containers need.
var safeGlobals = map[string]bool{
	"collections.OrderedDict": true, "collections.defaultdict": true,
	"torch._utils._rebuild_tensor": true, "torch._utils._rebuild_tensor_v2": true, "torch._utils._rebuild_tensor_v3": true,
	"torch._utils._rebuild_parameter": true, "torch._utils._rebuild_parameter_with_state": true,
	"torch._utils._rebuild_sparse_tensor": true, "torch._utils._rebuild_qtensor": true,
	"torch._tensor._rebuild_from_type_v2": true, "torch.Size": true, "torch.device": true,
	"torch.nn.parameter.Parameter":       true,
	"numpy.core.multiarray._reconstruct": true, "numpy._core.multiarray._reconstruct": true,
	"numpy.core.multiarray.scalar": true, "numpy._core.multiarray.scalar": true,
	"numpy.ndarray": true, "numpy.dtype": true, "_codecs.encode": true,
	"builtins.set": true, "builtins.frozenset": true, "builtins.slice": true, "builtins.range": true,
	"builtins.complex": true, "builtins.bytearray": true, "builtins.dict": true, "builtins.list": true,
	"builtins.tuple": true, "builtins.int": true, "builtins.float": true, "builtins.str": true, "builtins.bool": true,
	"__builtin__.set": true, "__builtin__.frozenset": true, "__builtin__.slice": true, "__builtin__.complex": true,
}

// torchDTypes are the dtypes a checkpoint names as torch.<dtype>.
var torchDTypes = map[string]bool{
	"float16": true, "float32": true, "float64": true, "bfloat16": true, "half": true, "float": true, "double": true,
	"int8": true, "int16": true, "int32": true, "int64": true, "uint8": true, "bool": true, "long": true, "int": true,
	"complex64": true, "complex128": true, "float8_e4m3fn": true, "float8_e5m2": true,
}

var dangerousModules = map[string]bool{
	"os": true, "posix": true, "nt": true, "subprocess": true, "sys": true, "socket": true, "shutil": true,
	"runpy": true, "pty": true, "commands": true, "webbrowser": true, "importlib": true, "pickle": true,
	"_pickle": true, "marshal": true, "code": true, "ctypes": true, "multiprocessing": true, "asyncio": true,
	"requests": true, "urllib": true, "httplib": true, "http": true, "ftplib": true, "smtplib": true,
	"telnetlib": true, "platform": true, "pip": true, "setuptools": true, "dill": true, "torch.hub": true,
}

var dangerousBuiltins = map[string]bool{
	"eval": true, "exec": true, "execfile": true, "compile": true, "open": true, "file": true, "input": true,
	"getattr": true, "setattr": true, "delattr": true, "__import__": true, "globals": true, "locals": true,
	"vars": true, "breakpoint": true, "apply": true,
}

func judgeImport(global string) string {
	if safeGlobals[global] {
		return "safe"
	}
	module, name := global, ""
	if i := strings.LastIndex(global, "."); i > 0 {
		module, name = global[:i], global[i+1:]
	}
	// Storage classes and dtypes: torch.FloatStorage, torch.float16.
	if module == "torch" && (strings.HasSuffix(name, "Storage") || torchDTypes[name]) {
		return "safe"
	}
	if module == "builtins" || module == "__builtin__" || module == "__builtins__" {
		if dangerousBuiltins[name] {
			return "dangerous"
		}
		return "unknown"
	}
	top, _, _ := strings.Cut(module, ".")
	if dangerousModules[module] || dangerousModules[top] || (module == "operator" && (name == "attrgetter" || name == "methodcaller")) {
		return "dangerous"
	}
	return "unknown"
}

// pickleArg is how an opcode's argument is laid out after it.
const (
	argNone = iota
	argLine
	argTwoLines
	argFixed // n bytes
	argLen1  // 1-byte length, then data
	argLen4  // 4-byte length, then data
	argLen8  // 8-byte length, then data
)

type pickleOp struct {
	kind int
	n    int
	str  bool // pushes a string a later STACK_GLOBAL may use
}

var pickleOps = map[byte]pickleOp{
	'(': {}, '.': {}, '0': {}, '1': {}, '2': {}, 'N': {}, 'Q': {}, 'R': {}, 'a': {}, 'b': {}, 'd': {}, '}': {},
	'e': {}, 'l': {}, ']': {}, 'o': {}, 's': {}, 't': {}, ')': {}, 'u': {},
	0x81: {}, 0x85: {}, 0x86: {}, 0x87: {}, 0x88: {}, 0x89: {}, 0x8f: {}, 0x90: {}, 0x91: {}, 0x92: {},
	0x93: {}, 0x94: {}, 0x97: {}, 0x98: {},
	'F': {kind: argLine}, 'I': {kind: argLine}, 'L': {kind: argLine}, 'P': {kind: argLine},
	'S': {kind: argLine, str: true}, 'V': {kind: argLine, str: true}, 'g': {kind: argLine}, 'p': {kind: argLine},
	'c': {kind: argTwoLines}, 'i': {kind: argTwoLines},
	'J': {kind: argFixed, n: 4}, 'K': {kind: argFixed, n: 1}, 'M': {kind: argFixed, n: 2}, 'G': {kind: argFixed, n: 8},
	'h': {kind: argFixed, n: 1}, 'j': {kind: argFixed, n: 4}, 'q': {kind: argFixed, n: 1}, 'r': {kind: argFixed, n: 4},
	0x80: {kind: argFixed, n: 1}, 0x82: {kind: argFixed, n: 1}, 0x83: {kind: argFixed, n: 2}, 0x84: {kind: argFixed, n: 4},
	0x95: {kind: argFixed, n: 8},
	'T':  {kind: argLen4, str: true}, 'U': {kind: argLen1, str: true}, 'X': {kind: argLen4, str: true},
	'B': {kind: argLen4}, 'C': {kind: argLen1}, 0x8a: {kind: argLen1}, 0x8b: {kind: argLen4},
	0x8c: {kind: argLen1, str: true}, 0x8d: {kind: argLen8, str: true}, 0x8e: {kind: argLen8}, 0x96: {kind: argLen8},
}

var errNotPickle = errors.New("not a pickle")

// maxPickleString bounds the strings kept for STACK_GLOBAL; longer ones
// are skipped, never buffered.
const maxPickleString = 512

// inspectPickles walks every pickle in a row, as a legacy torch.save file
// holds several, and stops at the first byte that does not start one.
func inspectPickles(r io.Reader, source string, in *inspection) error {
	br := bufio.NewReaderSize(r, 64<<10)
	imports := map[string]string{}
	walked := 0
	for {
		first, err := br.Peek(1)
		if err != nil || (walked > 0 && first[0] != 0x80) {
			break
		}
		if err := walkPickle(br, imports); err != nil {
			if passthrough(err) {
				return err
			}
			if walked == 0 {
				in.find(source, "model.invalid_pickle", "the pickle cannot be parsed: "+err.Error())
				return nil
			}
			break // what follows the pickles is tensor data
		}
		walked++
	}
	reportImports(source, imports, in)
	return nil
}

func reportImports(source string, imports map[string]string, in *inspection) {
	var dangerous, unknown []string
	for global, verdict := range imports {
		switch verdict {
		case "dangerous":
			dangerous = append(dangerous, global)
		case "unknown":
			unknown = append(unknown, global)
		}
	}
	sort.Strings(dangerous)
	sort.Strings(unknown)
	if len(dangerous) > 0 {
		in.find(source, "model.unsafe_pickle", "loading it would run "+listUpTo(dangerous, 5))
	}
	if len(unknown) > 0 {
		in.find(source, "model.pickle_imports", "loading it imports "+listUpTo(unknown, 5))
	}
}

func listUpTo(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:n], ", ") + fmt.Sprintf(" and %d more", len(items)-n)
}

// walkPickle reads one pickle to its STOP, recording each import.
func walkPickle(br *bufio.Reader, imports map[string]string) error {
	var strs []string // strings pushed, newest last
	memo := map[uint64]string{}
	lastStr := ""
	lastWasStr := false
	var nextMemo uint64
	for ops := 0; ; ops++ {
		code, err := br.ReadByte()
		if err != nil {
			if passthrough(err) {
				return err
			}
			return errors.New("ends before its STOP opcode")
		}
		op, ok := pickleOps[code]
		if !ok {
			if ops == 0 {
				return errNotPickle
			}
			return fmt.Errorf("unknown opcode 0x%02x", code)
		}
		arg, err := readPickleArg(br, op)
		if err != nil {
			return err
		}
		pushedStr := false
		switch code {
		case '.':
			return nil
		case 'c', 'i':
			module, name, _ := strings.Cut(arg, "\n")
			global := module + "." + name
			imports[global] = judgeImport(global)
		case 0x93: // STACK_GLOBAL: module and name are the two newest strings
			if len(strs) < 2 {
				imports["(unresolved STACK_GLOBAL)"] = "dangerous"
				break
			}
			global := strs[len(strs)-2] + "." + strs[len(strs)-1]
			imports[global] = judgeImport(global)
		case 0x94: // MEMOIZE
			if lastWasStr {
				memo[nextMemo] = lastStr
			}
			nextMemo++
		case 'p', 'q', 'r': // PUT, BINPUT, LONG_BINPUT
			if lastWasStr {
				memo[memoIndex(code, arg)] = lastStr
			}
		case 'g', 'h', 'j': // GET, BINGET, LONG_BINGET
			if s, ok := memo[memoIndex(code, arg)]; ok {
				strs = append(strs, s)
				lastStr, pushedStr = s, true
			}
		}
		if op.str {
			s := arg
			if code == 'S' || code == 'V' {
				s = strings.Trim(s, `'"`)
			}
			strs = append(strs, s)
			lastStr, pushedStr = s, true
		}
		if len(strs) > 64 {
			strs = strs[len(strs)-2:]
		}
		// MEMOIZE and PUT keep the string just pushed; anything else ends it.
		if code != 0x94 && code != 'p' && code != 'q' && code != 'r' {
			lastWasStr = pushedStr
		}
	}
}

func memoIndex(code byte, arg string) uint64 {
	switch code {
	case 'h', 'q':
		return uint64(arg[0])
	case 'j', 'r':
		return uint64(binary.LittleEndian.Uint32([]byte(arg)))
	}
	var n uint64
	fmt.Sscan(arg, &n)
	return n
}

// readPickleArg returns the argument as a string when it is short enough
// to matter (names, memo keys), skipping long data without buffering it.
func readPickleArg(br *bufio.Reader, op pickleOp) (string, error) {
	switch op.kind {
	case argNone:
		return "", nil
	case argLine:
		return readLine(br)
	case argTwoLines:
		a, err := readLine(br)
		if err != nil {
			return "", err
		}
		b, err := readLine(br)
		return a + "\n" + b, err
	case argFixed:
		buf := make([]byte, op.n)
		_, err := io.ReadFull(br, buf)
		return string(buf), err
	}
	var size uint64
	switch op.kind {
	case argLen1:
		b, err := br.ReadByte()
		if err != nil {
			return "", err
		}
		size = uint64(b)
	case argLen4:
		var buf [4]byte
		if _, err := io.ReadFull(br, buf[:]); err != nil {
			return "", err
		}
		size = uint64(binary.LittleEndian.Uint32(buf[:]))
	case argLen8:
		var buf [8]byte
		if _, err := io.ReadFull(br, buf[:]); err != nil {
			return "", err
		}
		size = binary.LittleEndian.Uint64(buf[:])
	}
	if size > maxPickleString {
		if _, err := io.CopyN(io.Discard, br, int64(size)); err != nil {
			return "", argError(err)
		}
		return "", nil
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(br, buf); err != nil {
		return "", argError(err)
	}
	return string(buf), nil
}

func argError(err error) error {
	if passthrough(err) {
		return err
	}
	return errors.New("ends inside an argument")
}

func readLine(br *bufio.Reader) (string, error) {
	line, err := br.ReadSlice('\n')
	if err != nil {
		if passthrough(err) {
			return "", err
		}
		return "", errors.New("a text argument has no end")
	}
	return string(bytes.TrimRight(line, "\r\n")), nil
}

// ─── NumPy and safetensors ───────────────────────────────────────────

var npyMagic = []byte("\x93NUMPY")

// inspectNPY reads a .npy header. An object array ('|O') is a pickle, and
// numpy.load(allow_pickle=True) runs it, so its imports are judged too.
func inspectNPY(r io.Reader, source string, in *inspection) error {
	br := bufio.NewReader(r)
	var pre [8]byte
	if _, err := io.ReadFull(br, pre[:]); passthrough(err) {
		return err
	} else if err != nil || !bytes.HasPrefix(pre[:], npyMagic) {
		in.find(source, "format.invalid_npy", "the array header is missing")
		return nil
	}
	var headerLen int
	switch pre[6] {
	case 1:
		var b [2]byte
		if _, err := io.ReadFull(br, b[:]); err != nil {
			in.find(source, "format.invalid_npy", "the array header is cut short")
			return nil
		}
		headerLen = int(binary.LittleEndian.Uint16(b[:]))
	case 2, 3:
		var b [4]byte
		if _, err := io.ReadFull(br, b[:]); err != nil {
			in.find(source, "format.invalid_npy", "the array header is cut short")
			return nil
		}
		headerLen = int(binary.LittleEndian.Uint32(b[:]))
	default:
		in.find(source, "format.invalid_npy", fmt.Sprintf("unknown .npy version %d", pre[6]))
		return nil
	}
	if headerLen > 1<<20 {
		in.find(source, "format.invalid_npy", "the array header is implausibly large")
		return nil
	}
	header := make([]byte, headerLen)
	if _, err := io.ReadFull(br, header); err != nil {
		in.find(source, "format.invalid_npy", "the array header is cut short")
		return nil
	}
	if bytes.Contains(header, []byte("'|O'")) || bytes.Contains(header, []byte(`"|O"`)) {
		in.find(source, "model.npy_object_array", "an object array: loading it needs allow_pickle=True, which runs a pickle")
		return inspectPickles(br, source, in)
	}
	return nil
}

var safetensorsDTypes = map[string]int64{
	"BOOL": 1, "U8": 1, "I8": 1, "F8_E5M2": 1, "F8_E4M3": 1, "I16": 2, "U16": 2, "F16": 2, "BF16": 2,
	"I32": 4, "U32": 4, "F32": 4, "I64": 8, "U64": 8, "F64": 8,
}

// inspectSafetensors checks the header: its length, its JSON, and that
// every tensor's offsets lie inside the file and match its shape.
// Safetensors cannot carry code; a broken header still crashes or
// misleads loaders, and a name hides other formats.
func inspectSafetensors(r io.Reader, size int64, in *inspection) error {
	bad := func(detail string) error {
		in.find("safetensors", "model.invalid_safetensors", detail)
		return nil
	}
	var pre [8]byte
	if _, err := io.ReadFull(r, pre[:]); passthrough(err) {
		return err
	} else if err != nil {
		return bad("shorter than its header length")
	}
	n := binary.LittleEndian.Uint64(pre[:])
	if n == 0 || n > 100<<20 || int64(n) > size-8 {
		return bad("the header length is out of range")
	}
	raw := make([]byte, n)
	if _, err := io.ReadFull(r, raw); passthrough(err) {
		return err
	} else if err != nil {
		return bad("the header is cut short")
	}
	var header map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimRight(raw, " "), &header); err != nil {
		return bad("the header is not a JSON object")
	}
	dataSize := size - 8 - int64(n)
	for name, value := range header {
		if name == "__metadata__" {
			continue
		}
		var t struct {
			DType   string  `json:"dtype"`
			Shape   []int64 `json:"shape"`
			Offsets []int64 `json:"data_offsets"`
		}
		if err := json.Unmarshal(value, &t); err != nil || len(t.Offsets) != 2 {
			return bad("tensor " + name + " has no valid entry")
		}
		width, ok := safetensorsDTypes[t.DType]
		if !ok {
			return bad("tensor " + name + " has unknown dtype " + t.DType)
		}
		begin, end := t.Offsets[0], t.Offsets[1]
		if begin < 0 || end < begin || end > dataSize {
			return bad("tensor " + name + " points outside the file")
		}
		elements := int64(1)
		for _, d := range t.Shape {
			if d < 0 || (d > 0 && elements > (1<<62)/d) {
				return bad("tensor " + name + " has an invalid shape")
			}
			elements *= d
		}
		if elements*width != end-begin {
			return bad("tensor " + name + "'s shape does not match its byte range")
		}
	}
	return nil
}
