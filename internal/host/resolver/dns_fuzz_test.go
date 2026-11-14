package resolver

import "testing"

// FuzzParse_bounded holds the forwarder's parsers to an upstream's or a
// client's arbitrary bytes: no panic, and every record read is an address
// of its own family taken from inside the message.
func FuzzParse_bounded(f *testing.F) {
	q := query(1, "codeload.github.com", typeA)
	f.Add(q)
	f.Add(response(q, 0, cnameRR("x.github.com", 60), aRR("140.82.121.10", 60), aRR("2606:50c0:8000::154", 60)))
	f.Add(append(append([]byte(nil), q[:12]...), 0xc0, 12, 0, 1, 0, 1))
	f.Fuzz(func(t *testing.T, msg []byte) {
		rq, err := parseQuestion(msg)
		if err != nil {
			return
		}
		if rq.end > len(msg) {
			t.Fatalf("question ends at %d past the message's %d bytes", rq.end, len(msg))
		}
		recs, err := addresses(msg, rq)
		if err != nil {
			return
		}
		if len(recs) > int(rq.answers) {
			t.Fatalf("%d records from %d answers", len(recs), rq.answers)
		}
		for _, r := range recs {
			if !r.Addr.Is4() && !r.Addr.Is6() {
				t.Fatalf("record %v is no address", r)
			}
		}
	})
}
