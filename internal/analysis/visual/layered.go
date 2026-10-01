package visual

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"

	"github.com/zylar06/video-agent/internal/domain"
	"github.com/zylar06/video-agent/internal/media"
)

// Overview combines scene changes with sparse coverage of long static shots.
// Every observation describes only the sampled instant, never the whole gap.
func Overview(ctx context.Context, tools media.Tools, asset domain.MediaAsset, dir string) ([]domain.Evidence, error) {
	raw, err := media.Run(ctx, tools.FFmpeg, "-v", "error", "-nostdin", "-i", asset.Path,
		"-an", "-vf", "scale=320:-2,select='gt(scene,0.35)',metadata=print:file=-", "-f", "null", "-")
	if err != nil {
		return nil, err
	}
	times := []int64{0}
	for _, m := range regexp.MustCompile("pts_time:([0-9.]+)").FindAllStringSubmatch(string(raw), -1) {
		seconds, _ := strconv.ParseFloat(m[1], 64)
		times = append(times, int64(seconds*1e6))
	}
	for t := int64(20_000_000); t < asset.DurationUS; t += 20_000_000 {
		times = append(times, t)
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	unique := []int64{}
	for _, t := range times {
		if t < asset.DurationUS && (len(unique) == 0 || t-unique[len(unique)-1] >= 1_000_000) {
			unique = append(unique, t)
		}
	}
	return SampleTimes(ctx, tools, asset, dir, unique)
}

func SampleTimes(ctx context.Context, tools media.Tools, asset domain.MediaAsset, dir string, times []int64) ([]domain.Evidence, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	out := []domain.Evidence{}
	seen := map[int64]bool{}
	for _, t := range times {
		if t < 0 || t >= asset.DurationUS || seen[t] {
			continue
		}
		seen[t] = true
		path := filepath.Join(dir, fmt.Sprintf("at-%012d.jpg", t))
		if _, err := os.Stat(path); err != nil {
			if _, err = media.Run(ctx, tools.FFmpeg, "-v", "error", "-nostdin", "-ss", fmt.Sprintf("%.6f", float64(t)/1e6), "-i", asset.Path, "-frames:v", "1", "-vf", "scale=768:-2", "-y", path); err != nil {
				return nil, err
			}
		}
		out = append(out, domain.Evidence{ID: fmt.Sprintf("%s-at-%012d", asset.ID, t), ProjectID: asset.ProjectID, AssetID: asset.ID, StartUS: t, EndUS: min(asset.DurationUS, t+33_333), AssetContentHash: asset.ContentHash, FrameRefs: []string{path}, Provider: "visual", AnalyzerVersion: "layered-v1"})
	}
	return out, nil
}
