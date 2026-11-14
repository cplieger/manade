package resolver

import (
	"net/netip"
	"slices"
	"testing"
	"time"
)

func TestAddresses(t *testing.T) {
	q := query(7, "codeload.github.com", typeA)
	resp := response(q, 0,
		cnameRR("lb-140-82-121-10-fra.github.com", 300),
		rr{owner: "lb-140-82-121-10-fra.github.com", typ: 1, ttl: 60, data: []byte{140, 82, 121, 10}},
		aRR("2606:50c0:8000::154", 120),
	)
	rq, err := parseQuestion(resp)
	if err != nil {
		t.Fatalf("parseQuestion: %v", err)
	}
	if rq.name != "codeload.github.com" || rq.qtype != typeA || rq.id != 7 {
		t.Errorf("question = %+v", rq)
	}
	got, err := addresses(resp, rq)
	if err != nil {
		t.Fatalf("addresses: %v", err)
	}
	want := []Record{
		{Addr: netip.MustParseAddr("140.82.121.10"), TTL: time.Minute},
		{Addr: netip.MustParseAddr("2606:50c0:8000::154"), TTL: 2 * time.Minute},
	}
	if !slices.Equal(got, want) {
		t.Errorf("addresses = %v, want %v (the CNAME skipped)", got, want)
	}
}

func TestParseQuestionCase(t *testing.T) {
	q, err := parseQuestion(query(1, "API.GitHub.COM", typeAAAA))
	if err != nil || q.name != "api.github.com" || q.qtype != typeAAAA || q.cls != classIN {
		t.Errorf("parseQuestion = %+v, %v", q, err)
	}
}

func TestMalformed(t *testing.T) {
	q := query(9, "github.com", typeA)
	loop := append(append([]byte(nil), q[:12]...), 0xc0, 12, 0, 1, 0, 1)
	truncated := response(q, 0, aRR("140.82.121.4", 60))
	truncated = truncated[:len(truncated)-2]
	badLabel := append(append([]byte(nil), q[:12]...), 0x40)
	badLabel = append(append(badLabel, make([]byte, 0x40)...), 0, 0, 1, 0, 1)
	twoQuestions := append([]byte(nil), q...)
	twoQuestions[5] = 2
	for _, tc := range []struct {
		name string
		msg  []byte
	}{
		{"shorter than a header", q[:11]},
		{"a pointer loop", loop},
		{"a label type the format reserves", badLabel},
		{"two questions", twoQuestions},
		{"a question cut short", q[:len(q)-1]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseQuestion(tc.msg); err == nil {
				t.Error("parseQuestion accepted it")
			}
		})
	}
	t.Run("an answer cut short", func(t *testing.T) {
		rq, err := parseQuestion(truncated)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := addresses(truncated, rq); err == nil {
			t.Error("addresses accepted it")
		}
	})
}

func TestFailure(t *testing.T) {
	q := query(0xbeef, "github.com", typeA)
	pq, err := parseQuestion(q)
	if err != nil {
		t.Fatal(err)
	}
	f := failure(q, pq, rcodeFail)
	fq, err := parseQuestion(f)
	if err != nil {
		t.Fatalf("parseQuestion(failure) = %v", err)
	}
	isResp, rc := isResponse(f), rcodeOf(f)
	if !isResp || rc != 2 || !sameQuestion(pq, fq) || fq.answers != 0 {
		t.Errorf("failure = %x: response %v, rcode %d, question %+v", f, isResp, rc, fq)
	}
	if f[2]&0x01 == 0 {
		t.Error("failure dropped the query's RD bit")
	}
}
