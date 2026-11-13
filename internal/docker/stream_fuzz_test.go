package docker

import (
	"bytes"
	"testing"
)

// FuzzDemux_framingOracle splits the payload into frames of the fuzzed size
// on alternating streams; however it is framed, demux must give back the
// payload's last LogKeep bytes, partial when the byte before them is no
// newline.
func FuzzDemux_framingOracle(f *testing.F) {
	f.Add([]byte("line 1\nline 2\n"), uint16(3))
	f.Add(bytes.Repeat([]byte("x"), LogKeep+100), uint16(4096))
	f.Add(append(bytes.Repeat([]byte("x\n"), 50), bytes.Repeat([]byte("y"), LogKeep)...), uint16(4096))
	f.Add([]byte{}, uint16(1))
	f.Fuzz(func(t *testing.T, payload []byte, size uint16) {
		n := int(size)%8192 + 1
		var in []byte
		for i, stream := 0, byte(1); i < len(payload); i, stream = i+n, 3-stream {
			in = append(in, frame(stream, string(payload[i:min(i+n, len(payload))]))...)
		}
		out, partial, err := demux(bytes.NewReader(in))
		if err != nil {
			t.Fatalf("demux: %v", err)
		}
		cut := len(payload) - LogKeep
		want := payload[max(0, cut):]
		if !bytes.Equal(out, want) {
			t.Fatalf("demux = %d bytes, want the last %d of the payload", len(out), len(want))
		}
		if wantPartial := cut > 0 && payload[cut-1] != '\n'; partial != wantPartial {
			t.Fatalf("demux partial = %v, want %v", partial, wantPartial)
		}
	})
}

// FuzzDemux_bounded feeds raw daemon bytes: demux never keeps more than
// LogKeep bytes, whatever the frames claim.
func FuzzDemux_bounded(f *testing.F) {
	f.Add(frame(1, "ok\n"))
	f.Add([]byte{1, 0, 0, 0, 0xff, 0xff, 0xff, 0xff, 'a'})
	f.Add([]byte{9, 9, 9})
	f.Fuzz(func(t *testing.T, raw []byte) {
		if out, _, _ := demux(bytes.NewReader(raw)); len(out) > LogKeep || len(out) > len(raw) {
			t.Fatalf("demux kept %d bytes from %d", len(out), len(raw))
		}
	})
}
