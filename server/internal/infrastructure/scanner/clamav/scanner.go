// Package clamav scans streams with a ClamAV daemon (clamd) over TCP using
// its INSTREAM command. Files are streamed in chunks and never executed.
package clamav

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

const chunkSize = 256 * 1024

// scanBuffers is what one scan needs, reused from a pool instead of
// allocated per scan: the chunk it reads into, a writer that packs the
// command, each chunk's length and bytes and the closing marker into as few
// writes as it can (a small file leaves in one packet instead of four,
// which counts when clamd is a network hop away), and the reply reader.
type scanBuffers struct {
	chunk []byte
	size  [4]byte
	out   *bufio.Writer
	in    *bufio.Reader
}

var buffers = sync.Pool{New: func() any {
	return &scanBuffers{
		chunk: make([]byte, chunkSize),
		out:   bufio.NewWriterSize(nil, 16+chunkSize),
		in:    bufio.NewReaderSize(nil, 256),
	}
}}

// Scanner talks to one clamd address such as "127.0.0.1:3310".
type Scanner struct {
	address string
	timeout time.Duration
}

var (
	_ interfaces.MalwareScanner   = (*Scanner)(nil)
	_ interfaces.VersionedScanner = (*Scanner)(nil)
)

func New(address string, timeout time.Duration) *Scanner {
	return &Scanner{address: address, timeout: timeout}
}

func (s *Scanner) Name() string { return "clamav" }

// Ping sends PING and expects PONG.
func (s *Scanner) Ping(ctx context.Context) error {
	conn, err := s.dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("zPING\x00")); err != nil {
		return fmt.Errorf("clamd ping: %w", err)
	}
	reply, err := bufio.NewReader(conn).ReadString(0)
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("clamd ping: %w", err)
	}
	if strings.TrimRight(reply, "\x00\n") != "PONG" {
		return errors.New("clamd did not answer PONG")
	}
	return nil
}

// Version answers clamd's VERSION, such as "ClamAV 1.4.3/27800/Mon Oct 27
// 08:23:00 2026": the engine, the signature database number and its date.
// It changes whenever clamd loads new signatures.
func (s *Scanner) Version(ctx context.Context) (string, error) {
	conn, err := s.dial(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("zVERSION\x00")); err != nil {
		return "", fmt.Errorf("clamd version: %w", err)
	}
	reply, err := bufio.NewReader(conn).ReadString(0)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("clamd version: %w", err)
	}
	version := strings.TrimRight(reply, "\x00\n")
	if !strings.HasPrefix(version, "ClamAV ") {
		return "", errors.New("clamd sent no version")
	}
	return version, nil
}

// Scan streams content to clamd and maps its single-line answer:
//
//	stream: OK                  -> CLEAN
//	stream: <signature> FOUND   -> INFECTED
//	... size limit exceeded ... -> UNSUPPORTED (clamd's StreamMaxLength)
//	deadline or socket timeout  -> TIMEOUT
//	anything else               -> ERROR
//
// None but CLEAN is ever a clean result.
func (s *Scanner) Scan(ctx context.Context, content io.Reader) interfaces.ScanResult {
	conn, err := s.dial(ctx)
	if err != nil {
		return s.failure(ctx, "connect", err)
	}
	defer conn.Close()

	b := buffers.Get().(*scanBuffers)
	defer func() {
		// Drop the connection before the buffers go back for the next scan.
		b.out.Reset(nil)
		b.in.Reset(nil)
		buffers.Put(b)
	}()
	b.out.Reset(conn)
	b.in.Reset(conn)

	if _, err := b.out.WriteString("zINSTREAM\x00"); err != nil {
		return s.failure(ctx, "start scan", err)
	}
	for {
		n, readErr := content.Read(b.chunk)
		if n > 0 {
			binary.BigEndian.PutUint32(b.size[:], uint32(n))
			if _, err := b.out.Write(b.size[:]); err != nil {
				return s.failure(ctx, "send chunk", err)
			}
			if _, err := b.out.Write(b.chunk[:n]); err != nil {
				return s.failure(ctx, "send chunk", err)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return interfaces.ScanResult{Outcome: model.ScanError, Detail: "could not read the content to scan"}
		}
	}

	// A zero-length chunk ends the stream; the flush sends what is left.
	if _, err := b.out.Write([]byte{0, 0, 0, 0}); err != nil {
		return s.failure(ctx, "end stream", err)
	}
	if err := b.out.Flush(); err != nil {
		return s.failure(ctx, "end stream", err)
	}

	reply, err := b.in.ReadString(0)
	if err != nil && !errors.Is(err, io.EOF) {
		return s.failure(ctx, "read verdict", err)
	}
	return parseReply(strings.TrimRight(reply, "\x00\n"))
}

// dial connects with one deadline for the whole exchange; cancelling ctx
// closes the socket.
func (s *Scanner) dial(ctx context.Context) (net.Conn, error) {
	dialer := net.Dialer{Timeout: s.timeout}
	conn, err := dialer.DialContext(ctx, "tcp", s.address)
	if err != nil {
		return nil, fmt.Errorf("connect to clamd: %w", err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(s.timeout))
	}
	context.AfterFunc(ctx, func() { _ = conn.Close() })
	return conn, nil
}

func parseReply(reply string) interfaces.ScanResult {
	reply = strings.TrimSpace(strings.TrimPrefix(reply, "stream:"))

	switch {
	case reply == "OK":
		return interfaces.ScanResult{Outcome: model.ScanClean}
	case strings.HasSuffix(reply, " FOUND"):
		return interfaces.ScanResult{Outcome: model.ScanInfected, Signature: strings.TrimSuffix(reply, " FOUND")}
	case strings.Contains(reply, "size limit exceeded"):
		return interfaces.ScanResult{Outcome: model.ScanUnsupported, Detail: "object exceeds the scanner's stream size limit"}
	case reply == "":
		return interfaces.ScanResult{Outcome: model.ScanError, Detail: "scanner sent no verdict"}
	default:
		return interfaces.ScanResult{Outcome: model.ScanError, Detail: "scanner reported an error"}
	}
}

// failure classifies a transport error as TIMEOUT or ERROR. The socket
// deadline is the context's deadline and can fire a moment before the
// context notices, so a passed deadline counts as a timeout too.
func (s *Scanner) failure(ctx context.Context, step string, err error) interfaces.ScanResult {
	var netErr net.Error
	deadline, hasDeadline := ctx.Deadline()
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded) ||
		(errors.As(err, &netErr) && netErr.Timeout()) ||
		(hasDeadline && !time.Now().Before(deadline))
	if timedOut {
		return interfaces.ScanResult{Outcome: model.ScanTimeout, Detail: "scan timed out at " + step}
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return interfaces.ScanResult{Outcome: model.ScanError, Detail: "scan cancelled at " + step}
	}
	return interfaces.ScanResult{Outcome: model.ScanError, Detail: "scanner unreachable or failed at " + step}
}
