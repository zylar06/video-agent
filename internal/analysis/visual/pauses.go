package visual

import (
	"context"
	"github.com/zylar06/video-agent/internal/domain"
	"github.com/zylar06/video-agent/internal/media"
	"regexp"
	"strconv"
)

func Pauses(ctx context.Context, tools media.Tools, asset domain.MediaAsset) ([]domain.Evidence, error) {
	if !asset.HasAudio {
		return nil, nil
	}
	raw, err := media.Run(ctx, tools.FFmpeg, "-v", "error", "-nostdin", "-i", asset.Path, "-vn", "-af", "silencedetect=noise=-35dB:d=0.25,ametadata=print:file=-", "-f", "null", "-")
	if err != nil {
		return nil, err
	}
	events := regexp.MustCompile("lavfi.silence_(start|end)=([0-9.]+)").FindAllStringSubmatch(string(raw), -1)
	out := []domain.Evidence{}
	var start int64
	for _, m := range events {
		seconds, _ := strconv.ParseFloat(m[2], 64)
		t := int64(seconds * 1e6)
		if m[1] == "start" {
			start = t
			continue
		}
		if t > start && t <= asset.DurationUS {
			out = append(out, domain.Evidence{ID: asset.ID + "-pause-" + strconv.FormatInt(start, 10), ProjectID: asset.ProjectID, AssetID: asset.ID, StartUS: start, EndUS: t, AssetContentHash: asset.ContentHash, VisualSummary: "音频静音区间（本地检测）", Provider: "ffmpeg-silence"})
		}
	}
	return out, nil
}
