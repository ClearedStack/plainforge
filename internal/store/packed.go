package store

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ClearedStack/plainforge/internal/pferr"
)

const (
	maxPackedRefsBytes = 4 << 20
	maxDeltaDepth      = 64
	idxMagic           = "\xfftOc"
	packMagic          = "PACK"
)

type packFile struct {
	packPath string
	packSize int64
	fanout   [256]uint32
	oids     []OID
	offsets  []int64
}

func loadPackFiles(objsDir string) ([]*packFile, error) {
	packDir := filepath.Join(objsDir, "pack")
	entries, err := os.ReadDir(packDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, storageErr("listing pack directory", err)
	}

	var packNames []string
	idxSet := make(map[string]bool)
	for _, e := range entries {
		if e.IsDir() {
			return nil, pferr.Parsef("the repository pack directory holds an unexpected subdirectory")
		}
		name := e.Name()
		switch {
		case strings.HasSuffix(name, ".pack"):
			packNames = append(packNames, strings.TrimSuffix(name, ".pack"))
		case strings.HasSuffix(name, ".idx"):
			idxSet[strings.TrimSuffix(name, ".idx")] = true
		case strings.HasSuffix(name, ".promisor"):
			return nil, pferr.Needsf("the repository uses a promisor pack, which Plainforge cannot read")
		case strings.HasSuffix(name, ".rev"),
			strings.HasSuffix(name, ".bitmap"),
			strings.HasSuffix(name, ".mtimes"),
			strings.HasSuffix(name, ".keep"),
			name == "multi-pack-index":
			// Optional pack acceleration or lock metadata written by git gc.
		default:
			return nil, pferr.Needsf("the repository pack directory holds an unsupported file")
		}
	}
	sort.Strings(packNames)
	if len(packNames) != len(idxSet) {
		return nil, pferr.Parsef("the repository has mismatched pack and index files")
	}

	out := make([]*packFile, 0, len(packNames))
	for _, base := range packNames {
		if !idxSet[base] {
			return nil, pferr.Parsef("a pack in the repository has no matching index file")
		}
		pf, err := openPackPair(
			filepath.Join(packDir, base+".idx"),
			filepath.Join(packDir, base+".pack"),
		)
		if err != nil {
			return nil, err
		}
		out = append(out, pf)
	}
	return out, nil
}

func openPackPair(idxPath, packPath string) (*packFile, error) {
	idxBytes, err := os.ReadFile(idxPath)
	if err != nil {
		return nil, storageErr("reading pack index", err)
	}
	// Minimum v2 idx: 8 (header) + 1024 (fanout) + 40 (pack+idx sha1) = 1072 bytes.
	if len(idxBytes) < 1072 || string(idxBytes[:4]) != idxMagic {
		return nil, pferr.Parsef("a pack index in the repository is malformed")
	}
	ver := binary.BigEndian.Uint32(idxBytes[4:8])
	if ver != 2 {
		return nil, pferr.Needsf("a pack index uses version %d, which Plainforge cannot read", ver)
	}

	idxSum := sha1.Sum(idxBytes[:len(idxBytes)-20])
	if !bytes.Equal(idxSum[:], idxBytes[len(idxBytes)-20:]) {
		return nil, pferr.Parsef("a pack index checksum does not match")
	}
	wantPackSum := idxBytes[len(idxBytes)-40 : len(idxBytes)-20]

	var fanout [256]uint32
	var prev uint32
	for i := 0; i < 256; i++ {
		v := binary.BigEndian.Uint32(idxBytes[8+i*4 : 12+i*4])
		if v < prev {
			return nil, pferr.Parsef("a pack index fanout table is not monotonic")
		}
		fanout[i] = v
		prev = v
	}
	count := int(fanout[255])
	oidStart := 8 + 256*4
	crcStart := oidStart + count*20
	off32Start := crcStart + count*4
	off64Start := off32Start + count*4
	trailerStart := len(idxBytes) - 40
	if off64Start > trailerStart || (trailerStart-off64Start)%8 != 0 {
		return nil, pferr.Parsef("a pack index has a truncated table")
	}
	largeCount := (trailerStart - off64Start) / 8

	oids := make([]OID, count)
	for i := 0; i < count; i++ {
		copy(oids[i][:], idxBytes[oidStart+i*20:oidStart+(i+1)*20])
		if i > 0 && bytes.Compare(oids[i-1][:], oids[i][:]) >= 0 {
			return nil, pferr.Parsef("a pack index object table is not strictly sorted")
		}
	}

	pfFile, err := os.Open(packPath)
	if err != nil {
		return nil, storageErr("opening pack file", err)
	}
	defer pfFile.Close()
	st, err := pfFile.Stat()
	if err != nil {
		return nil, storageErr("stating pack file", err)
	}
	packSize := st.Size()
	if packSize < 32 {
		return nil, pferr.Parsef("a pack file in the repository is too short")
	}
	var hdr [12]byte
	if _, err := io.ReadFull(pfFile, hdr[:]); err != nil {
		return nil, pferr.Parsef("a pack header could not be read")
	}
	if string(hdr[:4]) != packMagic {
		return nil, pferr.Parsef("a pack file did not start with PACK")
	}
	if pver := binary.BigEndian.Uint32(hdr[4:8]); pver != 2 {
		return nil, pferr.Needsf("a pack file uses version %d, which Plainforge cannot read", pver)
	}
	if int(binary.BigEndian.Uint32(hdr[8:12])) != count {
		return nil, pferr.Parsef("a pack file object count disagrees with its index")
	}
	var packTrailer [20]byte
	if _, err := pfFile.ReadAt(packTrailer[:], packSize-20); err != nil {
		return nil, pferr.Parsef("a pack trailer could not be read")
	}
	if !bytes.Equal(packTrailer[:], wantPackSum) {
		return nil, pferr.Parsef("a pack trailer checksum disagrees with its index")
	}

	dataEnd := packSize - 20
	offsets := make([]int64, count)
	for i := 0; i < count; i++ {
		raw := binary.BigEndian.Uint32(idxBytes[off32Start+i*4 : off32Start+(i+1)*4])
		var off int64
		if raw&0x80000000 == 0 {
			off = int64(raw)
		} else {
			idx64 := int(raw & 0x7fffffff)
			if idx64 >= largeCount {
				return nil, pferr.Parsef("a pack index 64-bit offset entry is out of range")
			}
			u64 := binary.BigEndian.Uint64(idxBytes[off64Start+idx64*8 : off64Start+(idx64+1)*8])
			if u64 > uint64(dataEnd) {
				return nil, pferr.Parsef("a pack index offset points past the pack")
			}
			off = int64(u64)
		}
		if off < 12 || off >= dataEnd {
			return nil, pferr.Parsef("a pack index offset is outside the pack payload")
		}
		offsets[i] = off
	}

	return &packFile{
		packPath: packPath,
		packSize: packSize,
		fanout:   fanout,
		oids:     oids,
		offsets:  offsets,
	}, nil
}

func (pf *packFile) lookup(o OID) (int64, bool) {
	first := o[0]
	var lo int
	if first > 0 {
		lo = int(pf.fanout[first-1])
	}
	hi := int(pf.fanout[first])
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		cmp := bytes.Compare(pf.oids[mid][:], o[:])
		switch {
		case cmp < 0:
			lo = mid + 1
		case cmp > 0:
			hi = mid
		default:
			return pf.offsets[mid], true
		}
	}
	return 0, false
}

func (r *Repo) readFromPacks(o OID, depth int) (Type, []byte, error) {
	if depth > maxDeltaDepth {
		return 0, nil, pferr.Needsf("a delta chain in a pack is deeper than Plainforge follows")
	}
	for _, pf := range r.packs {
		off, ok := pf.lookup(o)
		if !ok {
			continue
		}
		t, data, err := r.readPackEntry(pf, off, depth)
		if err != nil {
			return 0, nil, err
		}
		if Hash(t, data) != o {
			return 0, nil, pferr.Parsef("a packed object's content does not match its name")
		}
		return t, data, nil
	}
	return 0, nil, errNotFound
}

func (r *Repo) readPackEntry(pf *packFile, offset int64, depth int) (Type, []byte, error) {
	if depth > maxDeltaDepth {
		return 0, nil, pferr.Needsf("a delta chain in a pack is deeper than Plainforge follows")
	}
	f, err := os.Open(pf.packPath)
	if err != nil {
		return 0, nil, storageErr("opening pack file", err)
	}
	defer f.Close()

	dataEnd := pf.packSize - 20
	if offset < 12 || offset >= dataEnd {
		return 0, nil, pferr.Parsef("a pack entry offset is out of bounds")
	}
	sr := io.NewSectionReader(f, offset, dataEnd-offset)
	br := &byteReader{r: sr}

	b, err := br.ReadByte()
	if err != nil {
		return 0, nil, pferr.Parsef("a pack entry header ended unexpectedly")
	}
	rawKind := (b >> 4) & 7
	size := uint64(b & 0x0f)
	shift := uint(4)
	for b&0x80 != 0 {
		if shift > 56 {
			return 0, nil, pferr.Parsef("a pack entry size header is malformed")
		}
		b, err = br.ReadByte()
		if err != nil {
			return 0, nil, pferr.Parsef("a pack entry header ended unexpectedly")
		}
		size |= uint64(b&0x7f) << shift
		shift += 7
	}
	if size > MaxObjectSize {
		return 0, nil, pferr.Needsf("an object in the pack is larger than Plainforge handles")
	}

	switch rawKind {
	case 1, 2, 3, 4:
		payload, err := inflateExact(br, int(size))
		if err != nil {
			return 0, nil, err
		}
		return Type(rawKind), payload, nil

	case 6: // OFS_DELTA
		c, err := br.ReadByte()
		if err != nil {
			return 0, nil, pferr.Parsef("an offset delta header ended unexpectedly")
		}
		negOff := int64(c & 0x7f)
		for c&0x80 != 0 {
			if negOff > offset {
				return 0, nil, pferr.Parsef("an offset delta points before the pack start")
			}
			c, err = br.ReadByte()
			if err != nil {
				return 0, nil, pferr.Parsef("an offset delta header ended unexpectedly")
			}
			negOff = ((negOff + 1) << 7) | int64(c&0x7f)
		}
		if negOff <= 0 || negOff >= offset {
			return 0, nil, pferr.Parsef("an offset delta points outside the pack")
		}
		deltaBytes, err := inflateExact(br, int(size))
		if err != nil {
			return 0, nil, err
		}
		baseKind, baseData, err := r.readPackEntry(pf, offset-negOff, depth+1)
		if err != nil {
			return 0, nil, err
		}
		out, err := applyPackDelta(baseData, deltaBytes, MaxObjectSize)
		if err != nil {
			return 0, nil, err
		}
		return baseKind, out, nil

	case 7: // REF_DELTA
		var baseOID OID
		if _, err := io.ReadFull(br, baseOID[:]); err != nil {
			return 0, nil, pferr.Parsef("a reference delta base name ended unexpectedly")
		}
		deltaBytes, err := inflateExact(br, int(size))
		if err != nil {
			return 0, nil, err
		}
		baseKind, baseData, err := r.readObjectDepth(baseOID, depth+1)
		if err != nil {
			if errors.Is(err, errNotFound) {
				return 0, nil, pferr.Parsef("a pack holds a delta whose base is not available")
			}
			return 0, nil, err
		}
		out, err := applyPackDelta(baseData, deltaBytes, MaxObjectSize)
		if err != nil {
			return 0, nil, err
		}
		return baseKind, out, nil

	default:
		return 0, nil, pferr.Parsef("a pack entry had an unknown object kind")
	}
}

type byteReader struct {
	r   io.Reader
	buf [1]byte
}

func (b *byteReader) Read(p []byte) (int, error) { return b.r.Read(p) }

func (b *byteReader) ReadByte() (byte, error) {
	_, err := io.ReadFull(b.r, b.buf[:])
	return b.buf[0], err
}

func inflateExact(r io.Reader, want int) ([]byte, error) {
	zr, err := zlib.NewReader(r)
	if err != nil {
		return nil, pferr.Parsef("a packed object compressed stream could not be opened")
	}
	defer zr.Close()
	data, err := io.ReadAll(io.LimitReader(zr, int64(want)+1))
	if err != nil {
		return nil, pferr.Parsef("a packed object compressed stream was damaged")
	}
	if len(data) != want {
		return nil, pferr.Parsef("a packed object's inflated size did not match its header")
	}
	return data, nil
}

func deltaVarint(d []byte) (int, []byte, error) {
	shift := uint(0)
	var size int
	for i, b := range d {
		if shift > 28 {
			return 0, nil, pferr.Parsef("a delta in the pack is malformed")
		}
		size |= int(b&0x7f) << shift
		shift += 7
		if b&0x80 == 0 {
			return size, d[i+1:], nil
		}
	}
	return 0, nil, pferr.Parsef("a delta in the pack is malformed")
}

func applyPackDelta(base, delta []byte, limit int) ([]byte, error) {
	srcSize, d, err := deltaVarint(delta)
	if err != nil {
		return nil, err
	}
	dstSize, d, err := deltaVarint(d)
	if err != nil {
		return nil, err
	}
	if srcSize != len(base) {
		return nil, pferr.Parsef("a delta in the pack is malformed")
	}
	if dstSize > limit {
		return nil, pferr.Needsf("an object in the pack is larger than Plainforge handles")
	}
	out := make([]byte, 0, dstSize)
	for len(d) > 0 {
		op := d[0]
		d = d[1:]
		switch {
		case op&0x80 != 0:
			var off, n int
			for i := uint(0); i < 4; i++ {
				if op&(1<<i) != 0 {
					if len(d) == 0 {
						return nil, pferr.Parsef("a delta in the pack is malformed")
					}
					off |= int(d[0]) << (8 * i)
					d = d[1:]
				}
			}
			for i := uint(0); i < 3; i++ {
				if op&(0x10<<i) != 0 {
					if len(d) == 0 {
						return nil, pferr.Parsef("a delta in the pack is malformed")
					}
					n |= int(d[0]) << (8 * i)
					d = d[1:]
				}
			}
			if n == 0 {
				n = 0x10000
			}
			if off > len(base) || n > len(base)-off || n > dstSize-len(out) {
				return nil, pferr.Parsef("a delta in the pack is malformed")
			}
			out = append(out, base[off:off+n]...)
		case op != 0:
			n := int(op)
			if n > len(d) || n > dstSize-len(out) {
				return nil, pferr.Parsef("a delta in the pack is malformed")
			}
			out = append(out, d[:n]...)
			d = d[n:]
		default:
			return nil, pferr.Parsef("a delta in the pack is malformed")
		}
	}
	if len(out) != dstSize {
		return nil, pferr.Parsef("a delta in the pack is malformed")
	}
	return out, nil
}

// readPackedRefs parses a Git packed-refs file if present.
func readPackedRefs(repoDir string) (map[string]OID, error) {
	p := filepath.Join(repoDir, "packed-refs")
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, storageErr("opening packed-refs", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxPackedRefsBytes+1))
	if err != nil {
		return nil, storageErr("reading packed-refs", err)
	}
	if len(data) > maxPackedRefsBytes {
		return nil, pferr.Needsf("packed-refs is larger than Plainforge handles")
	}

	text := string(data)
	if text == "" {
		return nil, pferr.Parsef("packed-refs is empty")
	}
	refs := make(map[string]OID)
	sawRef := false
	for _, rawLine := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		line := strings.TrimSuffix(rawLine, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if peeled, ok := strings.CutPrefix(line, "^"); ok {
			if !sawRef {
				return nil, pferr.Parsef("packed-refs has a peeled line with no preceding ref")
			}
			if _, err := ParseOID(peeled); err != nil {
				return nil, pferr.Parsef("packed-refs has a malformed peeled object name")
			}
			continue
		}
		oidHex, name, ok := strings.Cut(line, " ")
		if !ok {
			return nil, pferr.Parsef("packed-refs has a malformed line")
		}
		oid, err := ParseOID(oidHex)
		if err != nil || oid.IsZero() {
			return nil, pferr.Parsef("packed-refs has a malformed object name")
		}
		if err := CheckRefName(name); err != nil {
			return nil, pferr.Needsf("the repository holds a packed ref Plainforge has no rule for")
		}
		refs[name] = oid
		sawRef = true
	}
	return refs, nil
}
