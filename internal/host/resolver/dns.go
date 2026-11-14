package resolver

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"strings"
	"time"
)

// The DNS wire format's parts the forwarder reads (RFC 1035 §4.1).
const (
	headerLen  = 12
	typeA      = 1
	typeAAAA   = 28
	classIN    = 1
	flagQR     = 0x8000
	maxPointer = 64
)

// rcode is a response code (RFC 1035 §4.1.1).
type rcode uint16

const (
	rcodeOK      rcode = 0
	rcodeFail    rcode = 2
	rcodeRefused rcode = 5
)

var errMalformed = errors.New("malformed DNS message")

// question is a message's one question.
type question struct {
	name        string
	end         int
	qtype, cls  uint16
	id, answers uint16
}

// Record is an address an answer gave, with its TTL.
type Record struct {
	Addr netip.Addr
	TTL  time.Duration
}

// parseQuestion reads msg's header and its one question.
func parseQuestion(msg []byte) (question, error) {
	if len(msg) < headerLen || binary.BigEndian.Uint16(msg[4:]) != 1 {
		return question{}, errMalformed
	}
	name, end, err := readName(msg, headerLen)
	if err != nil || end+4 > len(msg) {
		return question{}, errMalformed
	}
	return question{
		id: binary.BigEndian.Uint16(msg), answers: binary.BigEndian.Uint16(msg[6:]),
		name:  name,
		qtype: binary.BigEndian.Uint16(msg[end:]), cls: binary.BigEndian.Uint16(msg[end+2:]),
		end: end + 4,
	}, nil
}

// isResponse reports whether msg, at least a header long, is a response.
func isResponse(msg []byte) bool { return binary.BigEndian.Uint16(msg[2:])&flagQR != 0 }

// rcodeOf answers the response code of msg, at least a header long.
func rcodeOf(msg []byte) rcode { return rcode(binary.BigEndian.Uint16(msg[2:]) & 0xf) }

// sameQuestion reports whether a and b ask the same, the name in any case.
func sameQuestion(a, b question) bool {
	return a.id == b.id && a.qtype == b.qtype && a.cls == b.cls && strings.EqualFold(a.name, b.name)
}

// addresses answers each A and AAAA record of resp's answer section, whose
// question q is; RFC 1035 gives no answer section an order to trust, so a
// CNAME chain's addresses are read wherever they sit.
func addresses(resp []byte, q question) ([]Record, error) {
	var out []Record
	off := q.end
	for range q.answers {
		var err error
		if _, off, err = readName(resp, off); err != nil || off+10 > len(resp) {
			return nil, errMalformed
		}
		typ, cls := binary.BigEndian.Uint16(resp[off:]), binary.BigEndian.Uint16(resp[off+2:])
		ttl := binary.BigEndian.Uint32(resp[off+4:])
		n := int(binary.BigEndian.Uint16(resp[off+8:]))
		off += 10
		if off+n > len(resp) {
			return nil, errMalformed
		}
		if a, ok := netip.AddrFromSlice(resp[off : off+n]); ok && cls == classIN && (typ == typeA && n == 4 || typ == typeAAAA && n == 16) {
			out = append(out, Record{Addr: a, TTL: time.Duration(ttl) * time.Second})
		}
		off += n
	}
	return out, nil
}

// readName reads the name at off and answers it, lower-cased and without
// its trailing dot, and the offset past it in msg.
func readName(msg []byte, off int) (name string, next int, err error) {
	var labels []string
	next = -1
	for range maxPointer + 1 {
		ls, past, ptr, err := readLabels(msg, off)
		if err != nil {
			return "", 0, err
		}
		labels = append(labels, ls...)
		if next < 0 {
			next = past
		}
		if ptr < 0 {
			return strings.ToLower(strings.Join(labels, ".")), next, nil
		}
		off = ptr
	}
	return "", 0, errMalformed
}

// readLabels reads labels from off up to the name's end or a compression
// pointer, answering them, the offset past what it read and the pointer's
// target, or -1 at the name's end.
func readLabels(msg []byte, off int) (labels []string, next, ptr int, err error) {
	for {
		if off >= len(msg) {
			return nil, 0, 0, errMalformed
		}
		l := int(msg[off])
		switch {
		case l == 0:
			return labels, off + 1, -1, nil
		case l&0xc0 == 0xc0 && off+1 < len(msg):
			return labels, off + 2, int(binary.BigEndian.Uint16(msg[off:]) & 0x3fff), nil
		case l&0xc0 != 0 || off+1+l > len(msg):
			return nil, 0, 0, errMalformed
		}
		labels = append(labels, string(msg[off+1:off+1+l]))
		off += 1 + l
	}
}

// failure answers a response to query q with rcode and no records: the
// query's header and question, its RD bit kept.
func failure(query []byte, q question, rc rcode) []byte {
	resp := make([]byte, q.end)
	copy(resp, query[:q.end])
	flags := binary.BigEndian.Uint16(query[2:])&0x7900 | flagQR | 0x0080 | uint16(rc)
	binary.BigEndian.PutUint16(resp[2:], flags)
	binary.BigEndian.PutUint16(resp[4:], 1)
	clear(resp[6:headerLen])
	return resp
}
