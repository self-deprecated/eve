package redact

import (
	"bytes"
	"errors"
	"io"
	"sync"
)

// maxLine is how much of a line without a newline the Writer holds back. Lines up to this
// length are redacted exactly as Redact would; longer ones are flushed in pieces cut where no
// known secret form or token crosses the cut (see safeCut).
const maxLine = 1 << 20

// ErrClosed is returned by Write after Close.
var ErrClosed = errors.New("redact: write after close")

// Writer returns a WriteCloser that redacts everything written to it, line by line, and passes
// the result on to w. A secret split across two writes is still caught because nothing is
// passed on until its line is complete. Close flushes a final partial line; it does not close w.
// Writes may come from several goroutines (a command's stdout and stderr).
func (r *Redactor) Writer(w io.Writer) io.WriteCloser {
	return &writer{r: r, w: w}
}

type writer struct {
	mu     sync.Mutex
	r      *Redactor
	w      io.Writer
	buf    []byte // the pending, incomplete line
	out    []byte // reused output buffer
	st     state
	err    error // sticky error from w
	closed bool
}

func (w *writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, ErrClosed
	}
	if w.err != nil {
		return 0, w.err
	}
	w.buf = append(w.buf, p...)
	if i := bytes.LastIndexByte(w.buf, '\n'); i >= 0 {
		w.emit(w.buf[:i+1])
		w.buf = append(w.buf[:0], w.buf[i+1:]...)
	} else if len(w.buf) > maxLine {
		cut := w.r.safeCut(w.buf, len(w.buf)-w.r.overlap())
		w.emit(w.buf[:cut])
		w.buf = append(w.buf[:0], w.buf[cut:]...)
	}
	if w.err != nil {
		return 0, w.err
	}
	return len(p), nil
}

// Close redacts and writes the final partial line.
func (w *writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return w.err
	}
	w.closed = true
	if len(w.buf) > 0 && w.err == nil {
		w.emit(w.buf)
		w.buf = nil
	}
	return w.err
}

func (w *writer) emit(block []byte) {
	w.out = w.r.appendLines(w.out[:0], block, &w.st)
	if len(w.out) == 0 {
		return
	}
	if _, err := w.w.Write(w.out); err != nil {
		w.err = err
	}
}

// overlap is how much of an over-long line is held back when part of it is flushed.
func (r *Redactor) overlap() int {
	return max(r.maxForm, 4096)
}

// safeCut picks where to split an over-long line near cut: at the last space or tab in the
// preceding 64 KiB (tokens, emails and identifiers never contain one), then moved back to the
// start of any secret form that would straddle it. Only a line longer than maxLine with no
// whitespace for 64 KiB can split a token.
func (r *Redactor) safeCut(b []byte, cut int) int {
	lo := max(0, cut-64<<10)
	if i := bytes.LastIndexAny(b[lo:cut], " \t"); i >= 0 {
		cut = lo + i + 1
	}
	for moved := true; moved; {
		moved = false
		for _, f := range r.forms {
			from := max(0, cut-len(f)+1)
			to := min(len(b), cut+len(f)-1)
			if i := bytes.Index(b[from:to], f); i >= 0 && from+i < cut {
				cut = from + i
				moved = true
			}
		}
	}
	return cut
}
