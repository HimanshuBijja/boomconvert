package adapters

import (
	"context"
	"fmt"
	"os/exec"
)

var imageFormats = []string{"jpg", "png", "gif", "webp", "bmp", "tiff", "ico"}

type ImageMagickAdapter struct{}

func (ImageMagickAdapter) Name() string         { return "imagemagick" }
func (ImageMagickAdapter) RequiredTool() string { return "imagemagick" }

func (ImageMagickAdapter) Supports(src, dst string) bool {
	return inSet(src, imageFormats) && inSet(dst, imageFormats) && src != dst
}

func (ImageMagickAdapter) Convert(ctx context.Context, src, dst string, opts ConvertOptions) error {
	args := []string{src}
	// Flatten alpha to white when targeting a non-alpha format.
	if dst != "" {
		ext := Canonical(extOf(dst))
		if ext == "jpg" || ext == "bmp" {
			args = append(args, "-background", "white", "-alpha", "remove", "-alpha", "off")
		}
		if (ext == "jpg" || ext == "webp") && opts.Quality > 0 && opts.Quality <= 100 {
			args = append(args, "-quality", fmt.Sprintf("%d", opts.Quality))
		}
	}
	args = append(args, dst)
	cmd := exec.CommandContext(ctx, "magick", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("magick: %w: %s", err, string(out))
	}
	return nil
}

func (ImageMagickAdapter) Rules() []Rule {
	var rules []Rule
	for _, s := range imageFormats {
		for _, d := range imageFormats {
			if s == d {
				continue
			}
			rules = append(rules, Rule{Source: s, Target: d})
		}
	}
	return rules
}

func inSet(v string, set []string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

func extOf(p string) string {
	for i := len(p) - 1; i >= 0 && p[i] != '/' && p[i] != '\\'; i-- {
		if p[i] == '.' {
			return p[i+1:]
		}
	}
	return ""
}
