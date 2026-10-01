package visual

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/zylar06/video-agent/internal/domain"
	"github.com/zylar06/video-agent/internal/media"
)

type Sampler struct {
	MaxFrames  int
	IntervalUS int64
}

func (s Sampler) normalized() Sampler {
	if s.IntervalUS <= 0 {
		s.IntervalUS = 1_000_000
	}
	if s.IntervalUS < 1_000_000 {
		s.IntervalUS = 1_000_000
	}
	return s
}

// Sample extracts deterministic representative frames. A later scene-change
// sampler can replace the filter without changing the Evidence contract.
func (s Sampler) Sample(ctx context.Context, tools media.Tools, asset domain.MediaAsset, dir string) ([]domain.Evidence, error) {
	s = s.normalized()
	if err := asset.Validate(); err != nil {
		return nil, err
	}
	if s.MaxFrames <= 0 {
		s.MaxFrames = int((asset.DurationUS + s.IntervalUS - 1) / s.IntervalUS)
	}
	if s.MaxFrames > 1800 {
		return nil, fmt.Errorf("full visual analysis currently supports at most 30 minutes at 1fps")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	pattern := filepath.Join(dir, "frame-%04d.jpg")
	args := []string{"-hide_banner", "-v", "error", "-nostdin", "-y", "-ss", "0", "-i", asset.Path, "-vf", "fps=1/" + strconv.FormatInt(s.IntervalUS/1_000_000, 10) + ",scale=1024:1024:force_original_aspect_ratio=decrease", "-frames:v", strconv.Itoa(s.MaxFrames), "-q:v", "3", pattern}
	if _, err := media.Run(ctx, tools.FFmpeg, args...); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := []domain.Evidence{}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".jpg" {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(entry.Name(), "frame-%d.jpg", &n); err != nil || n < 1 {
			continue
		}
		start := int64(n-1) * s.IntervalUS
		if start >= asset.DurationUS {
			continue
		}
		end := start + s.IntervalUS
		if end > asset.DurationUS {
			end = asset.DurationUS
		}
		out = append(out, domain.Evidence{ID: fmt.Sprintf("%s-frame-%04d", asset.ID, n), ProjectID: asset.ProjectID, AssetID: asset.ID, StartUS: start, EndUS: end, AssetContentHash: asset.ContentHash, FrameRefs: []string{filepath.Join(dir, entry.Name())}, Provider: "ffmpeg-sampler", AnalyzerVersion: "sample-v1"})
	}
	return out, nil
}
