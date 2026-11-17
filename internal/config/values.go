package config

import (
	"cmp"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/go/pathinside"
	"github.com/cplieger/go/textsafe"
)

// byteUnits are the IEC units a size accepts.
var byteUnits = map[string]int64{
	"B":   1,
	"KiB": 1 << 10,
	"MiB": 1 << 20,
	"GiB": 1 << 30,
	"TiB": 1 << 40,
}

var (
	sizeRe = regexp.MustCompile(`^(\d{1,15})(B|KiB|MiB|GiB|TiB)$`)
	cpusRe = regexp.MustCompile(`^(\d{1,6})(?:\.(\d{1,3}))?$`)
	daysRe = regexp.MustCompile(`^(\d{1,3})d$`)
)

// problemError is a problem without its field path, which problems.add records.
type problemError string

func (e problemError) Error() string { return string(e) }

// quote renders an operator value inside an error: sanitized and capped, so a
// pasted control character cannot forge a log line.
func quote(s string) string {
	return strconv.Quote(textsafe.SanitizeSingleLineBounded(s, 80))
}

func invalid(what, raw string) error {
	return problemError(fmt.Sprintf("%s, got %s", what, quote(raw)))
}

// parseSize parses an IEC size such as "4GiB". "0" alone is zero, for swap.
func parseSize(raw string) (int64, error) {
	if raw == "0" {
		return 0, nil
	}
	m := sizeRe.FindStringSubmatch(raw)
	if m == nil {
		return 0, invalid("want a whole number with a unit (B, KiB, MiB, GiB or TiB)", raw)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	unit := byteUnits[m[2]]
	if err != nil || n > math.MaxInt64/unit {
		return 0, invalid("size is too large", raw)
	}
	return n * unit, nil
}

// parseCPUs parses a CPU count with up to three decimals into milli-CPUs.
func parseCPUs(raw string) (int64, error) {
	m := cpusRe.FindStringSubmatch(raw)
	if m == nil {
		return 0, invalid("want a CPU count such as 2 or 0.5", raw)
	}
	whole, _ := strconv.ParseInt(m[1], 10, 64)
	frac := m[2] + strings.Repeat("0", 3-len(m[2]))
	milli, _ := strconv.ParseInt(frac, 10, 64)
	if v := whole*1000 + milli; v > 0 {
		return v, nil
	}
	return 0, invalid("want more than 0 CPUs", raw)
}

func parseDuration(raw string) (time.Duration, error) {
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, invalid("want a positive duration such as 10m or 2h", raw)
	}
	return d, nil
}

// day is the unit of a retention setting, written as whole days ("90d").
const day = 24 * time.Hour

// days answers a parser of whole days from lo to hi, written "<n>d".
func days(lo, hi int64) func(string) (time.Duration, error) {
	return func(raw string) (time.Duration, error) {
		m := daysRe.FindStringSubmatch(raw)
		var n int64
		if m != nil {
			n, _ = strconv.ParseInt(m[1], 10, 64)
		}
		if m == nil || n < lo || n > hi {
			return 0, invalid(fmt.Sprintf("want whole days from %dd to %dd", lo, hi), raw)
		}
		return time.Duration(n) * day, nil
	}
}

// parseZone loads an IANA time zone name. time.LoadLocation reads "" and
// "Local" as zones of its own, which no setting names.
func parseZone(raw string) (*time.Location, error) {
	loc, err := time.LoadLocation(raw)
	if err != nil || raw == "Local" {
		return nil, invalid("want an IANA time zone name such as Europe/Paris", raw)
	}
	return loc, nil
}

func parseBool(raw string) (bool, error) {
	switch raw {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, invalid("want true or false", raw)
}

// positive answers a parser of whole numbers from 1 to hi.
func positive(hi int64) func(string) (int64, error) {
	return func(raw string) (int64, error) {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 1 || n > hi {
			return 0, invalid(fmt.Sprintf("want a whole number from 1 to %d", hi), raw)
		}
		return n, nil
	}
}

func parsePositiveSize(raw string) (int64, error) {
	n, err := parseSize(raw)
	if err == nil && n == 0 {
		return 0, invalid("want more than 0", raw)
	}
	return n, err
}

// scalar is a document value a section parser reads as text: a string, a
// number or a boolean as written, or "" for null or an absent key.
type scalar string

// UnmarshalJSONFrom takes a string, a number or a boolean; json.Unmarshal
// reports its refusal at the value's path.
func (s *scalar) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	v, err := dec.ReadValue()
	if err != nil {
		return err
	}
	switch v.Kind() {
	case '"':
		var str string
		if err := json.Unmarshal(v, &str); err != nil {
			return err
		}
		*s = scalar(str)
	case '0', 't', 'f':
		*s = scalar(v)
	case 'n':
	default:
		return errors.New("want a string, a number, true or false")
	}
	return nil
}

// field parses raw, or def when raw is empty, recording a failure under path.
// With both empty it answers T's zero value: the field is unset.
func field[T any](p *problems, path string, raw scalar, def string, parse func(string) (T, error)) T {
	var v T
	if r := cmp.Or(string(raw), def); r != "" {
		var err error
		v, err = parse(r)
		p.add(path, err)
	}
	return v
}

// parseHostPath accepts an absolute path written in its canonical form, the
// only form whose meaning cannot change between this check and Docker's.
func parseHostPath(raw string) (string, error) {
	if !strings.HasPrefix(raw, "/") || !pathinside.IsCanonical(raw) || pathinside.HasDotDot(raw) {
		return "", invalid("want an absolute, clean path", raw)
	}
	return raw, nil
}

// PrivateRanges are the IPv4 destinations a job reaches only through an
// allow entry: RFC 1918, shared address space and link-local, which hold
// every machine's Docker bridges and the cloud metadata address.
var PrivateRanges = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
}

// within reports whether p lies wholly inside one of blocks.
func within(p netip.Prefix, blocks []netip.Prefix) bool {
	return slices.ContainsFunc(blocks, func(b netip.Prefix) bool { return b.Bits() <= p.Bits() && b.Contains(p.Addr()) })
}

// parseSubnet accepts a masked IPv4 prefix of /28 or wider that lies wholly
// inside one private range. Any public address in it would route real
// destinations into the job bridge.
func parseSubnet(raw string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(raw)
	if err != nil || !p.Addr().Is4() || p != p.Masked() || p.Bits() < 8 || p.Bits() > 28 || !within(p, PrivateRanges) {
		return netip.Prefix{}, invalid("want a private IPv4 subnet from /8 to /28, such as 172.30.0.0/24", raw)
	}
	return p, nil
}

// parsePrefix accepts an address, read as its own /32 or /128, or a masked
// prefix.
func parsePrefix(raw string) (netip.Prefix, error) {
	if a, err := netip.ParseAddr(raw); err == nil && a.Zone() == "" {
		return netip.PrefixFrom(a, a.BitLen()), nil
	}
	p, err := netip.ParsePrefix(raw)
	if err != nil || p != p.Masked() {
		return netip.Prefix{}, invalid("want an IP address or a prefix such as 10.0.0.0/24", raw)
	}
	return p, nil
}

// problems collects every validation failure at its field path.
type problems struct{ errs FieldErrors }

func (p *problems) add(path string, err error) {
	if err != nil {
		p.errs = append(p.errs, FieldError{Path: path, Message: err.Error()})
	}
}

func (p *problems) addf(path, format string, args ...any) {
	p.add(path, problemError(fmt.Sprintf(format, args...)))
}

func (p *problems) err() error {
	if len(p.errs) == 0 {
		return nil
	}
	return p.errs
}
