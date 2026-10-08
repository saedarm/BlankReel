package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/image/font/gofont/gobold"
)

const (
	fps       = 30
	titleSec  = 2.5
	minShot   = 3.5
	padAfter  = 0.7 // breathing room after each narration line
	imgSuffix = ". Single cinematic still frame, live-action comedy, 35mm film look, warm practical lighting. No text, captions, logos, or watermarks."
)

// renderTrailer turns a filled-in story into dir/trailer.mp4 and dir/poster.jpg.
// progress is called with a short status line before each step.
func renderTrailer(ctx context.Context, g *Gemini, dir string, st Story, answers []string, day int, progress func(step string, done, total int)) error {
	n := len(st.Scenes)
	total := 2*n + 2 // images+voice per shot, clips, stitch
	done := 0
	var mu sync.Mutex
	bump := func(msg string) { mu.Lock(); done++; progress(msg, done, total); mu.Unlock() }

	if err := os.WriteFile(filepath.Join(dir, "font.ttf"), gobold.TTF, 0o644); err != nil {
		return err
	}

	// 1. Images and narration, three shots at a time.
	durs := make([]float64, n)
	errs := make([]error, n)
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	progress("Drawing the shots", 0, total)
	for k, sc := range st.Scenes {
		wg.Add(1)
		go func(k int, sc Scene) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			line := fillPlain(sc.Line, answers)
			var img, wav []byte
			var err error
			if g == nil {
				img = mockImage(k)
			} else if img, err = g.Image(ctx, fillPlain(sc.Shot, answers)+imgSuffix); err != nil {
				errs[k] = fmt.Errorf("shot %d image: %w", k+1, err)
				return
			}
			if err = os.WriteFile(filepath.Join(dir, fmt.Sprintf("shot%d.jpg", k)), img, 0o644); err != nil {
				errs[k] = err
				return
			}
			bump(fmt.Sprintf("Drew shot %d", k+1))
			if g == nil {
				durs[k] = math.Max(minShot, float64(len(strings.Fields(line)))*0.36+padAfter)
			} else {
				if wav, err = g.Speech(ctx, line); err != nil {
					errs[k] = fmt.Errorf("shot %d narration: %w", k+1, err)
					return
				}
				if err = os.WriteFile(filepath.Join(dir, fmt.Sprintf("line%d.wav", k)), wav, 0o644); err != nil {
					errs[k] = err
					return
				}
				durs[k] = math.Max(minShot, wavSeconds(wav)+padAfter)
			}
			bump(fmt.Sprintf("Recorded line %d", k+1))
		}(k, sc)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}

	// 2. Title card, then one clip per shot with a slow push or pan and the caption burned in.
	progress("Cutting the clips", done, total)
	if err := os.WriteFile(filepath.Join(dir, "title.txt"), []byte(wrap(st.Title, 24)), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "sub.txt"), []byte(fmt.Sprintf("BLANK REEL #%d", day)), 0o644); err != nil {
		return err
	}
	err := ffmpeg(ctx, dir,
		"-f", "lavfi", "-i", fmt.Sprintf("color=c=0x141a3e:s=1280x720:r=%d:d=%.2f", fps, titleSec),
		"-f", "lavfi", "-i", "anullsrc=r=44100:cl=stereo",
		"-vf", "drawtext=fontfile=font.ttf:textfile=title.txt:expansion=none:fontcolor=white:fontsize=72:line_spacing=12:x=(w-text_w)/2:y=(h-text_h)/2-30,"+
			"drawtext=fontfile=font.ttf:textfile=sub.txt:expansion=none:fontcolor=0xffd94a:fontsize=28:x=(w-text_w)/2:y=h/2+120,format=yuv420p",
		"-t", fmt.Sprintf("%.2f", titleSec), "-shortest",
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "22", "-c:a", "aac", "-ar", "44100", "-ac", "2", "title.mp4")
	if err != nil {
		return err
	}

	list := []string{"file 'title.mp4'"}
	for k, sc := range st.Scenes {
		cap := fmt.Sprintf("cap%d.txt", k)
		if err := os.WriteFile(filepath.Join(dir, cap), []byte(wrap(fillPlain(sc.Line, answers), 44)), 0o644); err != nil {
			return err
		}
		frames := int(durs[k] * fps)
		z, x, y := motion(k, frames)
		audioIn := []string{"-f", "lavfi", "-i", "anullsrc=r=44100:cl=stereo"}
		if g != nil {
			audioIn = []string{"-i", fmt.Sprintf("line%d.wav", k)}
		}
		args := append([]string{"-i", fmt.Sprintf("shot%d.jpg", k)}, audioIn...)
		args = append(args,
			"-filter_complex",
			fmt.Sprintf("[0:v]scale=2560:1440:force_original_aspect_ratio=increase,crop=2560:1440,"+
				"zoompan=z='%s':x='%s':y='%s':d=%d:s=1280x720:fps=%d,"+
				"drawtext=fontfile=font.ttf:textfile=%s:expansion=none:fontcolor=white:fontsize=38:line_spacing=10:"+
				"box=1:boxcolor=black@0.55:boxborderw=16:x=(w-text_w)/2:y=h-text_h-56,format=yuv420p[v];"+
				"[1:a]aresample=44100,apad[a]", z, x, y, frames, fps, cap),
			"-map", "[v]", "-map", "[a]", "-t", fmt.Sprintf("%.2f", durs[k]),
			"-c:v", "libx264", "-preset", "veryfast", "-crf", "22", "-c:a", "aac", "-ar", "44100", "-ac", "2",
			fmt.Sprintf("clip%d.mp4", k))
		if err := ffmpeg(ctx, dir, args...); err != nil {
			return fmt.Errorf("shot %d clip: %w", k+1, err)
		}
		list = append(list, fmt.Sprintf("file 'clip%d.mp4'", k))
	}

	// 3. Stitch.
	progress("Stitching the trailer", done, total)
	if err := os.WriteFile(filepath.Join(dir, "list.txt"), []byte(strings.Join(list, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	if err := ffmpeg(ctx, dir, "-f", "concat", "-safe", "0", "-i", "list.txt", "-c", "copy", "-movflags", "+faststart", "trailer.mp4"); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(dir, "shot0.jpg"), filepath.Join(dir, "poster.jpg")); err != nil {
		return err
	}
	cleanup(dir)
	progress("Done", total, total)
	return nil
}

// motion picks a camera move per shot: push in, drift right, pull out, drift left.
func motion(k, frames int) (z, x, y string) {
	f := float64(frames)
	center := "iw/2-(iw/zoom/2)"
	mid := "ih/2-(ih/zoom/2)"
	switch k % 4 {
	case 0:
		return fmt.Sprintf("1+0.14*on/%.0f", f), center, mid
	case 1:
		return "1.15", fmt.Sprintf("(iw-iw/zoom)*on/%.0f", f), mid
	case 2:
		return fmt.Sprintf("1.16-0.14*on/%.0f", f), center, mid
	default:
		return "1.15", fmt.Sprintf("(iw-iw/zoom)*(1-on/%.0f)", f), mid
	}
}

func ffmpeg(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "ffmpeg", append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)...)
	cmd.Dir = dir // keep every path relative; avoids drive-letter escaping on Windows
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func cleanup(dir string) {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		switch e.Name() {
		case "trailer.mp4", "poster.jpg", "meta.json":
		default:
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// wrap breaks text into lines of at most n characters for drawtext, which does not wrap.
func wrap(s string, n int) string {
	var lines []string
	cur := ""
	for _, w := range strings.Fields(s) {
		if cur != "" && len(cur)+1+len(w) > n {
			lines = append(lines, cur)
			cur = w
		} else if cur == "" {
			cur = w
		} else {
			cur += " " + w
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return strings.Join(lines, "\n")
}

// wavSeconds reads the duration from a RIFF/WAV header.
func wavSeconds(b []byte) float64 {
	if len(b) < 44 || string(b[0:4]) != "RIFF" {
		return float64(len(b)) / (24000 * 2)
	}
	var byteRate uint32
	for i := 12; i+8 <= len(b); {
		id := string(b[i : i+4])
		size := binary.LittleEndian.Uint32(b[i+4 : i+8])
		switch id {
		case "fmt ":
			if i+16 <= len(b) {
				byteRate = binary.LittleEndian.Uint32(b[i+16 : i+20])
			}
		case "data":
			if size == 0 || size == 0xffffffff || int(size) > len(b)-i-8 {
				size = uint32(len(b) - i - 8)
			}
			if byteRate == 0 {
				byteRate = 48000
			}
			return float64(size) / float64(byteRate)
		}
		i += 8 + int(size) + int(size%2)
	}
	return float64(len(b)) / 48000
}

// mockImage draws a colored gradient so the pipeline runs without an API key.
func mockImage(k int) []byte {
	hues := []float64{228, 268, 18, 196, 320, 44}
	h := hues[k%len(hues)]
	img := image.NewRGBA(image.Rect(0, 0, 1280, 720))
	for y := 0; y < 720; y++ {
		for x := 0; x < 1280; x++ {
			t := (float64(x)/1280 + float64(y)/720) / 2
			img.Set(x, y, hsl(h+40*t, 0.6, 0.18+0.22*(1-t)))
		}
	}
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85})
	return buf.Bytes()
}

func hsl(h, s, l float64) color.RGBA {
	h = math.Mod(h, 360) / 360
	f := func(n float64) uint8 {
		k := math.Mod(n+h*12, 12)
		a := s * math.Min(l, 1-l)
		return uint8(255 * (l - a*math.Max(-1, math.Min(math.Min(k-3, 9-k), 1))))
	}
	return color.RGBA{f(0), f(8), f(4), 255}
}
