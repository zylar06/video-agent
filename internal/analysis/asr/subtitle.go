package asr

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/zylar06/video-agent/internal/domain"
)

type Cue struct {
	Index   int
	StartUS int64
	EndUS   int64
	Text    string
}

var timestampLine = regexp.MustCompile(`^\s*([0-9:.,]+)\s*-->\s*([0-9:.,]+)(?:\s+.*)?$`)

func ParseFile(ctx context.Context, path string) ([]Cue, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Parse(ctx, f)
}

func Parse(ctx context.Context, r *os.File) ([]Cue, error) { return parse(ctx, bufio.NewScanner(r)) }

func parse(ctx context.Context, scanner *bufio.Scanner) ([]Cue, error) {
	lines := []string{}
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lines = append(lines, strings.TrimPrefix(scanner.Text(), "\ufeff"))
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return parseLines(lines)
}

func parseLines(lines []string) ([]Cue, error) {
	var cues []Cue
	for i := 0; i < len(lines); {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.EqualFold(line, "WEBVTT") || strings.HasPrefix(strings.ToUpper(line), "NOTE") {
			i++
			continue
		}
		if !strings.Contains(line, "-->") && i+1 < len(lines) {
			i++
			line = strings.TrimSpace(lines[i])
		}
		match := timestampLine.FindStringSubmatch(line)
		if match == nil {
			i++
			continue
		}
		start, err := parseTimestamp(match[1])
		if err != nil {
			return nil, fmt.Errorf("subtitle cue %d: %w", len(cues)+1, err)
		}
		end, err := parseTimestamp(match[2])
		if err != nil {
			return nil, fmt.Errorf("subtitle cue %d: %w", len(cues)+1, err)
		}
		if end <= start {
			return nil, errors.New("subtitle cue end must be after start")
		}
		i++
		text := []string{}
		for i < len(lines) && strings.TrimSpace(lines[i]) != "" {
			text = append(text, strings.TrimSpace(lines[i]))
			i++
		}
		joined := strings.TrimSpace(strings.Join(text, " "))
		if joined == "" {
			continue
		}
		cues = append(cues, Cue{Index: len(cues) + 1, StartUS: start, EndUS: end, Text: joined})
	}
	for i := 1; i < len(cues); i++ {
		if cues[i].StartUS < cues[i-1].StartUS {
			return nil, errors.New("subtitle cues must be ordered by start time")
		}
	}
	return cues, nil
}

func parseTimestamp(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	parts := strings.Split(raw, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, errors.New("invalid timestamp")
	}
	secPart := parts[len(parts)-1]
	sec, err := strconv.ParseFloat(strings.Replace(secPart, ",", ".", 1), 64)
	if err != nil {
		return 0, errors.New("invalid timestamp")
	}
	minutes, err := strconv.ParseInt(parts[len(parts)-2], 10, 64)
	if err != nil {
		return 0, errors.New("invalid timestamp")
	}
	hours := int64(0)
	if len(parts) == 3 {
		hours, err = strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return 0, errors.New("invalid timestamp")
		}
	}
	if minutes < 0 || minutes >= 60 || sec < 0 || sec >= 60 {
		return 0, errors.New("invalid timestamp range")
	}
	return int64((float64(hours)*3600+float64(minutes)*60+sec)*1e6 + 0.5), nil
}

func ToEvidence(project string, asset domain.MediaAsset, cues []Cue, provider, version, cacheKey string) []domain.Evidence {
	out := make([]domain.Evidence, 0, len(cues))
	suffix := ""
	if len(cacheKey) >= 12 {
		suffix = "-" + cacheKey[:12]
	}
	for _, cue := range cues {
		out = append(out, domain.Evidence{ID: fmt.Sprintf("%s-cue%s-%06d", asset.ID, suffix, cue.Index), ProjectID: project, AssetID: asset.ID, StartUS: cue.StartUS, EndUS: cue.EndUS, AssetContentHash: asset.ContentHash, Transcript: cue.Text, Provider: provider, AnalyzerVersion: version, CacheKey: cacheKey})
	}
	return out
}
