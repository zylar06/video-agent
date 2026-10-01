package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zylar06/video-agent/internal/domain"
	"github.com/zylar06/video-agent/internal/store"
)

type Tools struct{ FFmpeg, FFprobe string }

func Default() Tools {
	f := os.Getenv("VIDEO_AGENT_FFMPEG")
	if f == "" {
		f = "ffmpeg"
	}
	p := os.Getenv("VIDEO_AGENT_FFPROBE")
	if p == "" {
		p = "ffprobe"
	}
	return Tools{FFmpeg: f, FFprobe: p}
}

type limitedBuffer struct {
	data  []byte
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - len(b.data)
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		b.data = append(b.data, p...)
	}
	return n, nil
}

// Commands use argv, never a shell; stdout and diagnostics are bounded.
func Run(ctx context.Context, exe string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, exe, args...)
	out := &limitedBuffer{limit: 4 << 20}
	stderr := &limitedBuffer{limit: 32 << 10}
	cmd.Stdout = out
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%s: %w: %s", filepath.Base(exe), err, stderr.data)
	}
	return out.data, nil
}

type Info struct {
	DurationUS    int64  `json:"duration_us"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	HasAudio      bool   `json:"has_audio"`
	AudioChannels int    `json:"audio_channels"`
	Rotation      int    `json:"rotation"`
	FPS           string `json:"fps"`
	Frames        int    `json:"frames"`
	VideoCodec    string `json:"video_codec"`
	AudioCodec    string `json:"audio_codec"`
}

func LocalFile(path string) (string, error) {
	p, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	p, err = filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", errors.New("input must be a regular local file")
	}
	return p, nil
}
func (t Tools) Probe(ctx context.Context, path string) (Info, error) {
	p, err := LocalFile(path)
	if err != nil {
		return Info{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	b, err := Run(ctx, t.FFprobe, "-v", "error", "-protocol_whitelist", "file,pipe", "-show_streams", "-show_format", "-of", "json", p)
	if err != nil {
		return Info{}, err
	}
	var raw struct {
		Streams []struct {
			CodecType               string `json:"codec_type"`
			CodecName               string `json:"codec_name"`
			Width, Height, Channels int
			Duration                string
			AvgFrameRate            string `json:"avg_frame_rate"`
			NBFrames                string `json:"nb_frames"`
			Disposition             struct {
				AttachedPic int `json:"attached_pic"`
			}
			Tags     struct{ Rotate string }
			SideData []struct{ Rotation float64 } `json:"side_data_list"`
		}
		Format struct{ Duration string }
	}
	if err = json.Unmarshal(b, &raw); err != nil {
		return Info{}, err
	}
	info := Info{}
	duration := 0.0
	for _, s := range raw.Streams {
		if s.CodecType == "audio" && !info.HasAudio {
			info.HasAudio = true
			info.AudioChannels = s.Channels
			info.AudioCodec = s.CodecName
		}
		if s.CodecType != "video" || info.Width != 0 {
			continue
		}
		if s.Disposition.AttachedPic != 0 {
			return info, errors.New("attached-picture primary video is unsupported")
		}
		info.Width, info.Height, info.FPS, info.VideoCodec = s.Width, s.Height, s.AvgFrameRate, s.CodecName
		info.Frames, _ = strconv.Atoi(s.NBFrames)
		duration, _ = strconv.ParseFloat(s.Duration, 64)
		rotation, _ := strconv.ParseFloat(s.Tags.Rotate, 64)
		for _, d := range s.SideData {
			rotation = d.Rotation
		}
		info.Rotation = int(math.Round(rotation))
		if ((info.Rotation%360)+360)%180 == 90 {
			info.Width, info.Height = info.Height, info.Width
		}
	}
	if duration <= 0 {
		duration, _ = strconv.ParseFloat(raw.Format.Duration, 64)
	}
	if math.IsNaN(duration) || math.IsInf(duration, 0) || duration <= 0 || duration > 86400 || info.Width < 1 || info.Height < 1 {
		return info, errors.New("input requires a finite video duration and dimensions")
	}
	info.DurationUS = int64(math.Round(duration * 1e6))
	return info, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
func Hash(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, contextReader{ctx, f}); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Import snapshots the source into a content-addressed directory. User originals
// can subsequently move or change without silently changing an existing revision.
func (t Tools) Import(ctx context.Context, s *store.Store, project, path string) (domain.MediaAsset, error) {
	if _, err := s.Project(project); err != nil {
		return domain.MediaAsset{}, err
	}
	p, err := LocalFile(path)
	if err != nil {
		return domain.MediaAsset{}, err
	}
	dir := filepath.Join(s.Dir, "assets")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return domain.MediaAsset{}, err
	}
	src, err := os.Open(p)
	if err != nil {
		return domain.MediaAsset{}, err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(dir, ".import-*")
	if err != nil {
		return domain.MediaAsset{}, err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), contextReader{ctx, src})
	closeErr := tmp.Close()
	if err != nil {
		return domain.MediaAsset{}, err
	}
	if closeErr != nil {
		return domain.MediaAsset{}, closeErr
	}
	hash := hex.EncodeToString(h.Sum(nil))
	target := filepath.Join(dir, hash+".media")
	// Import is all-or-nothing: the snapshot is content-addressed and shared by
	// every asset with the same bytes, so a failure after publication would
	// leave a file no record points at. Only a link this call created is
	// removed, never one that already existed.
	published := false
	if linkErr := os.Link(tmp.Name(), target); linkErr == nil {
		published = true
	} else if !errors.Is(linkErr, os.ErrExist) {
		return domain.MediaAsset{}, linkErr
	}
	defer func() {
		if published {
			_ = os.Remove(target)
		}
	}()
	actual, err := Hash(ctx, target)
	if err != nil {
		return domain.MediaAsset{}, err
	}
	if actual != hash {
		return domain.MediaAsset{}, errors.New("asset cache hash mismatch")
	}
	info, err := t.Probe(ctx, target)
	if err != nil {
		return domain.MediaAsset{}, err
	}
	a := domain.MediaAsset{ID: "asset-" + hash, ProjectID: project, Path: target, ContentHash: hash, DurationUS: info.DurationUS, Width: info.Width, Height: info.Height, HasAudio: info.HasAudio, Status: "ready", FPS: info.FPS, AudioChannels: info.AudioChannels, Rotation: info.Rotation}
	asset, err := s.PutAsset(a)
	if err != nil {
		return domain.MediaAsset{}, err
	}
	published = false
	return asset, nil
}

func Seconds(us int64) string { return strconv.FormatFloat(float64(us)/1e6, 'f', 6, 64) }
func Rate(s string) float64 {
	p := strings.Split(s, "/")
	if len(p) != 2 {
		return 0
	}
	a, _ := strconv.ParseFloat(p[0], 64)
	b, _ := strconv.ParseFloat(p[1], 64)
	if b <= 0 {
		return 0
	}
	return a / b
}
