// Package pkt reads and writes Git pkt-lines: a four digit hexadecimal
// length, which counts itself, followed by the payload. "0000" is a flush.
package pkt

import (
	"errors"
	"io"

	"github.com/ClearedStack/plainforge/internal/pferr"
)

// MaxPayload is the longest payload one pkt-line may carry.
const MaxPayload = 65516

const hexDigits = "0123456789abcdef"

// Write writes one pkt-line holding payload.
func Write(w io.Writer, payload []byte) error {
	if len(payload) == 0 || len(payload) > MaxPayload {
		return pferr.Parsef("a line to send was empty or too long")
	}
	n := len(payload) + 4
	buf := make([]byte, 0, n)
	buf = append(buf, hexDigits[n>>12&15], hexDigits[n>>8&15], hexDigits[n>>4&15], hexDigits[n&15])
	buf = append(buf, payload...)
	_, err := w.Write(buf)
	return err
}

// WriteString writes one pkt-line holding s.
func WriteString(w io.Writer, s string) error { return Write(w, []byte(s)) }

// Flush writes a flush packet.
func Flush(w io.Writer) error {
	_, err := io.WriteString(w, "0000")
	return err
}

// Reader reads pkt-lines from an underlying reader.
type Reader struct {
	r   io.Reader
	hdr [4]byte
}

// NewReader returns a Reader that reads from r.
func NewReader(r io.Reader) *Reader { return &Reader{r: r} }

// Next reads one packet. For a flush packet it returns flush = true and no
// payload. At a clean end of input it returns io.EOF.
func (p *Reader) Next() (payload []byte, flush bool, err error) {
	if _, err := io.ReadFull(p.r, p.hdr[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, false, io.EOF
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, false, pferr.Parsef("a line ended inside its length")
		}
		return nil, false, err
	}
	n := 0
	for _, c := range p.hdr {
		var v int
		switch {
		case c >= '0' && c <= '9':
			v = int(c - '0')
		case c >= 'a' && c <= 'f':
			v = int(c-'a') + 10
		default:
			return nil, false, pferr.Parsef("a line length was not four lower-case hexadecimal digits")
		}
		n = n<<4 | v
	}
	switch {
	case n == 0:
		return nil, true, nil
	case n == 1 || n == 2:
		return nil, false, pferr.Needsf("the request uses a protocol version 2 packet")
	case n < 5:
		return nil, false, pferr.Parsef("a line length was too short to be a line")
	case n-4 > MaxPayload:
		return nil, false, pferr.Parsef("a line was longer than the protocol allows")
	}
	payload = make([]byte, n-4)
	if _, err := io.ReadFull(p.r, payload); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, false, pferr.Parsef("a line ended before its stated length")
		}
		return nil, false, err
	}
	return payload, false, nil
}
