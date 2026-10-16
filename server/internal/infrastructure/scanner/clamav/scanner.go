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
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
)

const chunkSize = 64 * 1024

// Scanner talks to one clamd address such as "127.0.0.1:3310".
type Scanner struct {
	address string
	timeout time.Duration
}

var _ interfaces.MalwareScanner = (*Scanner)(nil)

func New(address string, timeout time.Duration) *Scanner {
	return &Scanner{address: address, timeout: timeout}
}

func (s *Scanner) Name() string { return "clamav" }

// Scan sends content to clamd and reads its single-line verdict:
//
//	stream: OK                     -> clean
//	stream: <signature> FOUND      -> infected
//	anything else (ERROR, limits)  -> error, never clean
//
// clamd rejects streams above its StreamMaxLength (25 MiB by default);
// that answer is an error here, so oversized files are never passed as clean.
func (s *Scanner) Scan(ctx context.Context, content io.Reader) (interfaces.ScanVerdict, error) {
	dialer := net.Dialer{Timeout: s.timeout}
	conn, err := dialer.DialContext(ctx, "tcp", s.address)
	if err != nil {
		return interfaces.ScanVerdict{}, fmt.Errorf("connect to clamd: %w", err)
	}
	defer conn.Close()

	// One deadline for the whole exchange; cancellation closes the socket.
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(s.timeout))
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	if _, err := conn.Write([]byte("zINSTREAM\x00")); err != nil {
		return interfaces.ScanVerdict{}, scanError(ctx, "start scan", err)
	}

	buf := make([]byte, chunkSize)
	size := make([]byte, 4)
	for {
		n, readErr := content.Read(buf)
		if n > 0 {
			binary.BigEndian.PutUint32(size, uint32(n))
			if _, err := conn.Write(size); err != nil {
				return interfaces.ScanVerdict{}, scanError(ctx, "send chunk", err)
			}
			if _, err := conn.Write(buf[:n]); err != nil {
				return interfaces.ScanVerdict{}, scanError(ctx, "send chunk", err)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return interfaces.ScanVerdict{}, fmt.Errorf("read content to scan: %w", readErr)
		}
	}

	// A zero-length chunk ends the stream.
	if _, err := conn.Write([]byte{0, 0, 0, 0}); err != nil {
		return interfaces.ScanVerdict{}, scanError(ctx, "end stream", err)
	}

	reply, err := bufio.NewReader(conn).ReadString(0)
	if err != nil && !errors.Is(err, io.EOF) {
		return interfaces.ScanVerdict{}, scanError(ctx, "read verdict", err)
	}
	return parseReply(strings.TrimRight(reply, "\x00\n"))
}

func parseReply(reply string) (interfaces.ScanVerdict, error) {
	reply = strings.TrimSpace(strings.TrimPrefix(reply, "stream:"))

	switch {
	case reply == "OK":
		return interfaces.ScanVerdict{}, nil
	case strings.HasSuffix(reply, " FOUND"):
		return interfaces.ScanVerdict{Infected: true, Signature: strings.TrimSuffix(reply, " FOUND")}, nil
	case reply == "":
		return interfaces.ScanVerdict{}, errors.New("clamd sent no verdict")
	default:
		return interfaces.ScanVerdict{}, fmt.Errorf("clamd could not scan the stream: %s", reply)
	}
}

func scanError(ctx context.Context, step string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// The socket deadline is the context's deadline, and it can fire a
	// moment before the context notices; report it as the same timeout.
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return fmt.Errorf("clamd %s: %w", step, err)
}
