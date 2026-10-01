package visual

import "testing"

func TestSamplerDefaults(t *testing.T) {
	s := (Sampler{}).normalized()
	if s.MaxFrames != 0 || s.IntervalUS != 1_000_000 {
		t.Fatalf("defaults: %+v", s)
	}
}
