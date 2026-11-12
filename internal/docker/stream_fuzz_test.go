package docker

import (
	"bytes"
	"testing"
)

// FuzzDemux_framingOracle splits the payload into frames of the fuzzed size
// on alternating streams; however it is framed, demux must give back the
// payload's last logKeep bytes.
func FuzzDemux_framingOracle(f *testing.F) {
	f.Add([]byte("line 1\nline 2\n"), uint16(3))
	f.Add(bytes.Repeat([]byte("x"), logKeep+100), uint16(4096))
	f.Add([]byte{}, uint16(1))
	f.Fuzz(func(t *testing.T, payload []byte, size uint16) {
		n := int(size)%8192 + 1
		var in []byte
		for i, stream := 0, byte(1); i < len(payload); i, stream = i+n, 3-stream {
			in = append(in, frame(stream, string(payload[i:min(i+n, len(payload))]))...)
		}
		out, err := demux(bytes.NewReader(in))
		if err != nil {
			t.Fatalf("demux: %v", err)
		}
		want := payload[max(0, len(payload)-logKeep):]
		if !bytes.Equal(out, want) {
			t.Fatalf("demux = %d bytes, want the last %d of the payload", len(out), len(want))
		}
	})
}

// FuzzDemux_bounded feeds raw daemon bytes: demux never keeps more than
// logKeep bytes, whatever the frames claim.
func FuzzDemux_bounded(f *testing.F) {
	f.Add(frame(1, "ok\n"))
	f.Add([]byte{1, 0, 0, 0, 0xff, 0xff, 0xff, 0xff, 'a'})
	f.Add([]byte{9, 9, 9})
	f.Fuzz(func(t *testing.T, raw []byte) {
		if out, _ := demux(bytes.NewReader(raw)); len(out) > logKeep || len(out) > len(raw) {
			t.Fatalf("demux kept %d bytes from %d", len(out), len(raw))
		}
	})
}
