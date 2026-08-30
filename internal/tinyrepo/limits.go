package tinyrepo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Everything in this file exists because the mirror, and since -ws, the client
// too, are outside our control. They decide how many bytes to send, what a
// package is called and how often to ask for one.

// errPathEscapes is returned for a path that would be written outside the
// destination repository.
var errPathEscapes = errors.New("path escapes the repository")

// safeJoin joins a repository-relative path onto root and refuses anything that
// leaves it.
//
// filepath.Join cleans "..", it does not reject it, so joining an upstream
// Filename of "../../../../etc/cron.d/x" onto "/tmp/repo" quietly produces
// "/etc/cron.d/x". That path comes straight out of the mirror's index, so
// without this a hostile mirror can write anywhere the process can.
func safeJoin(root, relative string) (string, error) {
	if relative == "" {
		return "", fmt.Errorf("%w: %q", errPathEscapes, relative)
	}

	// An absolute path would replace root entirely rather than extend it.
	if filepath.IsAbs(relative) || strings.HasPrefix(relative, "/") {
		return "", fmt.Errorf("%w: %q", errPathEscapes, relative)
	}

	joined := filepath.Join(root, filepath.FromSlash(relative))

	// Compare against the cleaned root, because Join cleaned its result.
	cleanRoot := filepath.Clean(root)
	if joined != cleanRoot && !strings.HasPrefix(joined, cleanRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q", errPathEscapes, relative)
	}
	return joined, nil
}

// safeRelPath reports whether a repository-relative path is one we are willing
// to write. It is safeJoin's test without needing a root, for the places that
// reject an entry as it is read rather than when it is used.
func safeRelPath(relative string) bool {
	_, err := safeJoin("/tinyrepo", relative)
	return err == nil
}

// errTooLarge is returned when the far end sent more than it was allowed to.
var errTooLarge = errors.New("larger than expected")

// errBusy is what waiters are given when a request gave up its slot, so they
// retry instead of assuming the file could not be fetched at all.
var errBusy = errors.New("no download slot available")

// copyCapped copies at most limit bytes and fails if the source still had more
// to give.
//
// The point is to fail on the way in. Checking a size after io.Copy has already
// run means a mirror that promises four kilobytes and streams five hundred
// gigabytes fills the disk first and is rejected afterwards.
func copyCapped(dst io.Writer, src io.Reader, limit int64) (int64, error) {
	if limit < 0 {
		limit = 0
	}

	// One byte past the limit: if it arrives, the source was too big.
	written, err := io.Copy(dst, io.LimitReader(src, limit+1))
	if err != nil {
		return written, err
	}
	if written > limit {
		return written, fmt.Errorf("%w (%s %d)", errTooLarge, _t("limit"), limit)
	}
	return written, nil
}

// fetchLimit bounds how many upstream downloads may be in flight at once,
// across every client.
//
// The acquirer next door deduplicates identical paths, which is a different
// job: without this, one client asking for a hundred *different* packages opens
// a hundred connections, a hundred temporary files and a hundred copy buffers,
// and tinyrepo becomes an amplifier pointed at its own mirror.
type fetchLimit struct {
	slots chan struct{}
}

func newFetchLimit(n int) *fetchLimit {
	if n < 1 {
		n = 1
	}
	return &fetchLimit{slots: make(chan struct{}, n)}
}

// acquire takes a slot, waiting up to wait for one. It reports false if the
// wait ran out, which is the caller's cue to answer 503 rather than hold the
// connection open. A wait of zero means "only if a slot is free right now",
// which is what background work uses so it always yields to a real request.
func (f *fetchLimit) acquire(ctx context.Context, wait time.Duration) bool {
	if f == nil {
		return true
	}

	if wait <= 0 {
		select {
		case f.slots <- struct{}{}:
			return true
		default:
			return false
		}
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case f.slots <- struct{}{}:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
}

// limitListener caps how many connections are open at once.
//
// net/http has no such limit of its own: every accepted connection is a
// goroutine and at least one file descriptor, and a request that misses on
// demand can sit waiting for a download slot for half a minute without writing
// anything. A client opening thousands of slow connections would exhaust the
// process's descriptors and it would stop answering anybody at all.
//
// Past the cap the excess waits in the kernel's accept queue, which is exactly
// the backpressure wanted: nothing is refused, it is only made to queue.
type limitListener struct {
	net.Listener
	slots chan struct{}
}

func newLimitListener(inner net.Listener, n int) *limitListener {
	if n < 1 {
		n = 1
	}
	return &limitListener{Listener: inner, slots: make(chan struct{}, n)}
}

// counts reports the connections currently held and the cap. A held slot is by
// construction an open connection, so this is the enforced number rather than a
// second count of it.
func (l *limitListener) counts() (open, max int) {
	return len(l.slots), cap(l.slots)
}

func (l *limitListener) Accept() (net.Conn, error) {
	l.slots <- struct{}{}

	conn, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}

	return &limitConn{Conn: conn, release: func() { <-l.slots }}, nil
}

// limitConn gives its slot back exactly once however many times it is closed:
// net/http closes a connection from more than one place, and releasing twice
// would raise the cap by one every time it happened.
type limitConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

func (f *fetchLimit) release() {
	if f == nil {
		return
	}
	select {
	case <-f.slots:
	default:
	}
}
