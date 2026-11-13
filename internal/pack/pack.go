// Package pack reads and writes Git packfiles (version 2).
package pack

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"hash"
	"io"

	"github.com/clearedstack/plainforge/internal/pferr"
	"github.com/clearedstack/plainforge/internal/store"
)

const (
	typeOfsDelta = 6
	typeRefDelta = 7
)

// MaxPendingBytes bounds the deltas that have to wait in memory because
// their base comes later in the pack.
const MaxPendingBytes = 8 << 20

// Sink receives the objects of a pack and answers questions about objects
// that are already known.
type Sink interface {
	Write(t store.Type, data []byte) (store.OID, error)
	Read(o store.OID) (t store.Type, data []byte, ok bool, err error)
}

// hashReader hashes every byte that is read through it. It reads byte by
// byte when asked to, so the decompressor never takes more than its share.
type hashReader struct {
	br *bufio.Reader
	h  hash.Hash
	n  int64
}

func (r *hashReader) Read(p []byte) (int, error) {
	n, err := r.br.Read(p)
	r.h.Write(p[:n])
	r.n += int64(n)
	return n, err
}

func (r *hashReader) ReadByte() (byte, error) {
	b, err := r.br.ReadByte()
	if err == nil {
		r.h.Write([]byte{b})
		r.n++
	}
	return b, err
}

// readErr keeps public errors and deadlines as they are, and calls every
// other failure to read a broken pack.
func readErr(err error) error {
	var pe *pferr.Error
	if errors.As(err, &pe) {
		return err
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return pferr.Parsef("the pack ended early")
	}
	return pferr.Public(err, "the pack could not be read")
}

type pending struct {
	offset  int64
	baseOff int64     // base by position in this pack, when ofs is true
	baseOID store.OID // base by name otherwise
	ofs     bool
	delta   []byte
}

// Stats counts the entries of a pack by how they were stored.
type Stats struct {
	Whole, OfsDeltas, RefDeltas int
}

// Read reads a whole pack from r, rebuilds every object and hands it to the
// sink. It returns a count of the entries in the pack. Anything after the
// pack's checksum is an error.
func Read(r io.Reader, sink Sink) (Stats, error) {
	st, err := read(r, sink)
	if err != nil {
		return Stats{}, err
	}
	return st, nil
}

func read(r io.Reader, sink Sink) (st Stats, err0 error) {
	hr := &hashReader{br: bufio.NewReaderSize(r, 64<<10), h: sha1.New()}
	var head [12]byte
	if _, err := io.ReadFull(hr, head[:]); err != nil {
		return st, readErr(err)
	}
	if string(head[:4]) != "PACK" {
		return st, pferr.Parsef("the data sent is not a pack")
	}
	if v := binary.BigEndian.Uint32(head[4:8]); v != 2 {
		return st, pferr.Needsf("the pack is not version 2")
	}
	count := int(binary.BigEndian.Uint32(head[8:12]))

	byOffset := make(map[int64]store.OID, count)
	var waiting []pending
	waitingBytes := 0
	var zr io.ReadCloser

	for i := 0; i < count; i++ {
		start := hr.n
		b, err := hr.ReadByte()
		if err != nil {
			return st, readErr(err)
		}
		typ := int(b >> 4 & 7)
		size := int64(b & 15)
		for shift := uint(4); b&0x80 != 0; shift += 7 {
			if shift > 60 {
				return st, pferr.Parsef("an object size in the pack is malformed")
			}
			if b, err = hr.ReadByte(); err != nil {
				return st, readErr(err)
			}
			size |= int64(b&0x7f) << shift
		}
		if size > store.MaxObjectSize {
			return st, pferr.Needsf("an object in the pack is larger than Plainforge handles")
		}
		p := pending{offset: start}
		switch typ {
		case typeOfsDelta:
			if b, err = hr.ReadByte(); err != nil {
				return st, readErr(err)
			}
			back := int64(b & 0x7f)
			for b&0x80 != 0 {
				if back > 1<<40 {
					return st, pferr.Parsef("a delta in the pack is malformed")
				}
				if b, err = hr.ReadByte(); err != nil {
					return st, readErr(err)
				}
				back = (back+1)<<7 | int64(b&0x7f)
			}
			if back <= 0 || back > start {
				return st, pferr.Parsef("a delta in the pack is malformed")
			}
			p.ofs, p.baseOff = true, start-back
			st.OfsDeltas++
		case typeRefDelta:
			st.RefDeltas++
			if _, err := io.ReadFull(hr, p.baseOID[:]); err != nil {
				return st, readErr(err)
			}
		case int(store.Commit), int(store.Tree), int(store.Blob), int(store.Tag):
			st.Whole++
		default:
			return st, pferr.Parsef("the pack holds an entry of an unknown kind")
		}

		if zr == nil {
			zr, err = zlib.NewReader(hr)
		} else {
			err = zr.(zlib.Resetter).Reset(hr, nil)
		}
		if err != nil {
			return st, pferr.Parsef("the pack holds data that cannot be decompressed")
		}
		data := make([]byte, size)
		if _, err := io.ReadFull(zr, data); err != nil {
			return st, inflateErr(err)
		}
		// Read to the end of the stream so its checksum is verified.
		if n, err := zr.Read(make([]byte, 1)); n != 0 || err != io.EOF {
			return st, inflateErr(err)
		}

		if typ != typeOfsDelta && typ != typeRefDelta {
			oid, err := sink.Write(store.Type(typ), data)
			if err != nil {
				return st, err
			}
			byOffset[start] = oid
			continue
		}
		p.delta = data
		oid, done, err := resolve(p, byOffset, sink)
		if err != nil {
			return st, err
		}
		if done {
			byOffset[start] = oid
			continue
		}
		waitingBytes += len(data)
		if waitingBytes > MaxPendingBytes {
			return st, pferr.Needsf("the pack holds too many deltas whose base comes later")
		}
		waiting = append(waiting, p)
	}

	var want [20]byte
	hr.h.Sum(want[:0])
	var got [20]byte
	if _, err := io.ReadFull(hr.br, got[:]); err != nil {
		return st, readErr(err)
	}
	if got != want {
		return st, pferr.Parsef("the pack checksum does not match its content")
	}
	if _, err := hr.br.ReadByte(); err != io.EOF {
		if err == nil {
			return st, pferr.Parsef("there is data after the end of the pack")
		}
		return st, readErr(err)
	}

	for len(waiting) > 0 {
		var still []pending
		for _, p := range waiting {
			oid, done, err := resolve(p, byOffset, sink)
			if err != nil {
				return st, err
			}
			if done {
				byOffset[p.offset] = oid
			} else {
				still = append(still, p)
			}
		}
		if len(still) == len(waiting) {
			return st, pferr.Parsef("the pack holds a delta whose base is not available")
		}
		waiting = still
	}
	return st, nil
}

func inflateErr(err error) error {
	var pe *pferr.Error
	if errors.As(err, &pe) {
		return err
	}
	if p := pferr.Public(err, ""); p.Code == pferr.Timeout {
		return p
	}
	return pferr.Parsef("the pack holds data that cannot be decompressed")
}

// resolve rebuilds a delta when its base is available.
func resolve(p pending, byOffset map[int64]store.OID, sink Sink) (store.OID, bool, error) {
	base := p.baseOID
	if p.ofs {
		var ok bool
		if base, ok = byOffset[p.baseOff]; !ok {
			return store.OID{}, false, nil
		}
	}
	t, data, ok, err := sink.Read(base)
	if err != nil || !ok {
		return store.OID{}, false, err
	}
	out, err := ApplyDelta(data, p.delta, store.MaxObjectSize)
	if err != nil {
		return store.OID{}, false, err
	}
	oid, err := sink.Write(t, out)
	return oid, err == nil, err
}

// Write writes a pack holding the given objects, in the given order, with
// no deltas. read returns one object at a time.
func Write(w io.Writer, oids []store.OID, read func(store.OID) (store.Type, []byte, error)) error {
	h := sha1.New()
	bw := bufio.NewWriterSize(io.MultiWriter(w, h), 64<<10)
	var head [12]byte
	copy(head[:4], "PACK")
	binary.BigEndian.PutUint32(head[4:8], 2)
	binary.BigEndian.PutUint32(head[8:12], uint32(len(oids)))
	bw.Write(head[:])
	zw := zlib.NewWriter(bw)
	var buf bytes.Buffer
	for _, oid := range oids {
		t, data, err := read(oid)
		if err != nil {
			return err
		}
		buf.Reset()
		size := len(data)
		b := byte(t)<<4 | byte(size&15)
		size >>= 4
		for size > 0 {
			buf.WriteByte(b | 0x80)
			b = byte(size & 0x7f)
			size >>= 7
		}
		buf.WriteByte(b)
		bw.Write(buf.Bytes())
		zw.Reset(bw)
		zw.Write(data)
		if err := zw.Close(); err != nil {
			return err
		}
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	_, err := w.Write(h.Sum(nil))
	return err
}
