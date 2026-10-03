package radio

import (
	"bytes"
	"io"
	"testing"
)

// oddReader hands out 3 bytes per Read, splitting int16 samples mid-way.
type oddReader struct{ data []byte }

func (r *oddReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p[:min(len(p), 3)], r.data)
	r.data = r.data[n:]
	return n, nil
}

type captureOut struct{ bytes.Buffer }

func (*captureOut) Close() error { return nil }

func TestRunPCMPipelineKeepsFrameAlignment(t *testing.T) {
	input := make([]byte, 4*1000) // 1000 stereo s16le frames
	for i := range input {
		input[i] = byte(i*7 + 1)
	}

	p := NewPlayer(100) // full volume and no fade: output must equal input byte for byte
	out := &captureOut{}
	if err := p.runPCMPipeline(&oddReader{data: input}, out, false); err != io.EOF {
		t.Fatalf("runPCMPipeline() error = %v, want io.EOF", err)
	}
	if !bytes.Equal(out.Bytes(), input) {
		t.Fatalf("PCM output differs from input (got %d bytes, want %d)", out.Len(), len(input))
	}
}
