package age

import (
	"strings"

	"github.com/ClearedStack/plainforge/internal/pferr"
)

const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

var bech32Gen = [5]uint32{
	0x3b6a57b2,
	0x26508e6d,
	0x1ea119fa,
	0x3d4233dd,
	0x2a1462b3,
}

func bech32Polymod(values []byte) uint32 {
	chk := uint32(1)
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(v)
		for i := 0; i < 5; i++ {
			if (top>>i)&1 == 1 {
				chk ^= bech32Gen[i]
			}
		}
	}
	return chk
}

func bech32HRPExpand(hrp string) []byte {
	out := make([]byte, 0, len(hrp)*2+1)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]>>5)
	}
	out = append(out, 0)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]&31)
	}
	return out
}

func bech32Checksum(hrp string, data []byte) [6]byte {
	values := append(bech32HRPExpand(hrp), data...)
	values = append(values, 0, 0, 0, 0, 0, 0)
	mod := bech32Polymod(values) ^ 1
	var ret [6]byte
	for i := 0; i < 6; i++ {
		ret[i] = byte((mod >> uint(5*(5-i))) & 31)
	}
	return ret
}

func bech32Encode(hrp string, data5 []byte, upper bool) string {
	lowerHRP := strings.ToLower(hrp)
	sum := bech32Checksum(lowerHRP, data5)
	var b strings.Builder
	b.Grow(len(lowerHRP) + 1 + len(data5) + 6)
	b.WriteString(lowerHRP)
	b.WriteByte('1')
	for _, v := range data5 {
		b.WriteByte(bech32Charset[v])
	}
	for _, v := range sum {
		b.WriteByte(bech32Charset[v])
	}
	out := b.String()
	if upper {
		return strings.ToUpper(out)
	}
	return out
}

func bech32Decode(s string) (hrp string, data5 []byte, isUpper bool, err error) {
	if len(s) < 8 || len(s) > 90 {
		return "", nil, false, pferr.Parsef("malformed bech32 key length")
	}
	hasLower := false
	hasUpper := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 33 || c > 126 {
			return "", nil, false, pferr.Parsef("malformed bech32 character")
		}
		if c >= 'a' && c <= 'z' {
			hasLower = true
		}
		if c >= 'A' && c <= 'Z' {
			hasUpper = true
		}
	}
	if hasLower && hasUpper {
		return "", nil, false, pferr.Parsef("mixed-case bech32 key is not allowed")
	}
	lower := strings.ToLower(s)
	sep := strings.LastIndexByte(lower, '1')
	if sep < 1 || sep+7 > len(lower) {
		return "", nil, false, pferr.Parsef("malformed bech32 separator")
	}
	hrp = lower[:sep]
	raw := lower[sep+1:]
	vals := make([]byte, len(raw))
	for i := 0; i < len(raw); i++ {
		idx := strings.IndexByte(bech32Charset, raw[i])
		if idx < 0 {
			return "", nil, false, pferr.Parsef("malformed bech32 data character")
		}
		vals[i] = byte(idx)
	}
	if bech32Polymod(append(bech32HRPExpand(hrp), vals...)) != 1 {
		return "", nil, false, pferr.Parsef("bech32 checksum mismatch")
	}
	return hrp, vals[:len(vals)-6], hasUpper, nil
}

func convertBits(data []byte, fromBits, toBits uint, pad bool) ([]byte, error) {
	var acc uint32
	var bitsCount uint
	maxv := uint32((1 << toBits) - 1)
	out := make([]byte, 0, (len(data)*int(fromBits)+int(toBits)-1)/int(toBits))
	for _, value := range data {
		if uint32(value)>>fromBits != 0 {
			return nil, pferr.Parsef("bit group value out of range")
		}
		acc = (acc << fromBits) | uint32(value)
		bitsCount += fromBits
		for bitsCount >= toBits {
			bitsCount -= toBits
			out = append(out, byte((acc>>bitsCount)&maxv))
		}
	}
	if pad {
		if bitsCount > 0 {
			out = append(out, byte((acc<<(toBits-bitsCount))&maxv))
		}
	} else if bitsCount >= fromBits || ((acc<<(toBits-bitsCount))&maxv) != 0 {
		return nil, pferr.Parsef("non-canonical bech32 padding bits")
	}
	return out, nil
}
