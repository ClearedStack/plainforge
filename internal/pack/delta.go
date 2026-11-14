package pack

import "github.com/ClearedStack/plainforge/internal/pferr"

var errDelta = pferr.Parsef("a delta in the pack is malformed")

// deltaSize reads one size from the start of a delta.
func deltaSize(d []byte) (size int, rest []byte, err error) {
	shift := uint(0)
	for i, b := range d {
		if shift > 28 {
			return 0, nil, errDelta
		}
		size |= int(b&0x7f) << shift
		shift += 7
		if b&0x80 == 0 {
			return size, d[i+1:], nil
		}
	}
	return 0, nil, errDelta
}

// ApplyDelta rebuilds an object from a base and a Git delta. limit is the
// largest result that is accepted.
func ApplyDelta(base, delta []byte, limit int) ([]byte, error) {
	srcSize, d, err := deltaSize(delta)
	if err != nil {
		return nil, err
	}
	dstSize, d, err := deltaSize(d)
	if err != nil {
		return nil, err
	}
	if srcSize != len(base) {
		return nil, errDelta
	}
	if dstSize > limit {
		return nil, pferr.Needsf("an object in the pack is larger than Plainforge handles")
	}
	out := make([]byte, 0, dstSize)
	for len(d) > 0 {
		op := d[0]
		d = d[1:]
		switch {
		case op&0x80 != 0: // copy from the base
			var off, n int
			for i := uint(0); i < 4; i++ {
				if op&(1<<i) != 0 {
					if len(d) == 0 {
						return nil, errDelta
					}
					off |= int(d[0]) << (8 * i)
					d = d[1:]
				}
			}
			for i := uint(0); i < 3; i++ {
				if op&(0x10<<i) != 0 {
					if len(d) == 0 {
						return nil, errDelta
					}
					n |= int(d[0]) << (8 * i)
					d = d[1:]
				}
			}
			if n == 0 {
				n = 0x10000
			}
			if off > len(base) || n > len(base)-off || n > dstSize-len(out) {
				return nil, errDelta
			}
			out = append(out, base[off:off+n]...)
		case op != 0: // insert literal bytes
			n := int(op)
			if n > len(d) || n > dstSize-len(out) {
				return nil, errDelta
			}
			out = append(out, d[:n]...)
			d = d[n:]
		default:
			return nil, errDelta
		}
	}
	if len(out) != dstSize {
		return nil, errDelta
	}
	return out, nil
}
