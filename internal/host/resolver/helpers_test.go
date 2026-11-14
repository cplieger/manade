package resolver

import (
	"encoding/binary"
	"net/netip"
	"strings"
)

// wireName encodes name as RFC 1035 §3.1 labels.
func wireName(name string) []byte {
	var b []byte
	for l := range strings.SplitSeq(strings.TrimSuffix(name, "."), ".") {
		b = append(b, byte(len(l)))
		b = append(b, l...)
	}
	return append(b, 0)
}

// query is a recursive query for name of type qtype, class IN.
func query(id uint16, name string, qtype uint16) []byte {
	b := binary.BigEndian.AppendUint16(nil, id)
	b = append(b, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0)
	b = append(b, wireName(name)...)
	b = binary.BigEndian.AppendUint16(b, qtype)
	return binary.BigEndian.AppendUint16(b, 1)
}

// rr is one answer record; owner "" is a pointer to the question's name.
type rr struct {
	owner string
	data  []byte
	ttl   uint32
	typ   uint16
}

func aRR(addr string, ttl uint32) rr {
	a := netip.MustParseAddr(addr)
	if a.Is4() {
		b := a.As4()
		return rr{typ: 1, ttl: ttl, data: b[:]}
	}
	b := a.As16()
	return rr{typ: 28, ttl: ttl, data: b[:]}
}

func cnameRR(target string, ttl uint32) rr {
	return rr{typ: 5, ttl: ttl, data: wireName(target)}
}

// response answers q with rcode and the records, as an upstream would.
func response(q []byte, rcode byte, recs ...rr) []byte {
	b := append([]byte(nil), q...)
	b[2] = 0x81
	b[3] = 0x80 | rcode
	binary.BigEndian.PutUint16(b[6:], uint16(len(recs)))
	for _, r := range recs {
		if r.owner == "" {
			b = append(b, 0xc0, 12)
		} else {
			b = append(b, wireName(r.owner)...)
		}
		b = binary.BigEndian.AppendUint16(b, r.typ)
		b = binary.BigEndian.AppendUint16(b, 1)
		b = binary.BigEndian.AppendUint32(b, r.ttl)
		b = binary.BigEndian.AppendUint16(b, uint16(len(r.data)))
		b = append(b, r.data...)
	}
	return b
}
