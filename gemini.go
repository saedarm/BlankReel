package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

var interactionsURL = "https://generativelanguage.googleapis.com/v1beta/interactions"

// Gemini makes the two calls the trailer needs: one image per shot and one
// narration line per shot. Both go through the Interactions API.
type Gemini struct {
	Key        string
	ImageModel string
	TTSModel   string
	Voice      string
	HTTP       *http.Client
}

type interactionResp struct {
	Steps []struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Data string `json:"data"`
		} `json:"content"`
	} `json:"steps"`
}

// Image returns JPEG bytes for a 16:9 still.
func (g *Gemini) Image(ctx context.Context, prompt string) ([]byte, error) {
	body := map[string]any{
		"model": g.ImageModel,
		"input": []map[string]any{{"type": "text", "text": prompt}},
		"response_format": map[string]any{
			"type":         "image",
			"mime_type":    "image/jpeg",
			"aspect_ratio": "16:9",
		},
	}
	return g.call(ctx, body, "image")
}

// Speech returns a WAV file (24 kHz, mono, 16-bit) of the narrator reading text.
func (g *Gemini) Speech(ctx context.Context, text string) ([]byte, error) {
	body := map[string]any{
		"model": g.TTSModel,
		"input": []map[string]any{{
			"type": "user_input",
			"content": []map[string]any{{
				"type": "text",
				"text": text,
				"annotations": []map[string]any{{
					"type":  "speech_metadata",
					"style": "dramatic movie-trailer narrator, deadpan comic timing",
				}},
			}},
		}},
		"response_format":   map[string]any{"type": "audio"},
		"generation_config": map[string]any{"speech_config": []map[string]any{{"voice": g.Voice}}},
	}
	return g.call(ctx, body, "audio")
}

func (g *Gemini) call(ctx context.Context, body any, want string) ([]byte, error) {
	payload, _ := json.Marshal(body)
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt*attempt) * 4 * time.Second):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, interactionsURL, bytes.NewReader(payload))
		req.Header.Set("x-goog-api-key", g.Key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := g.HTTP.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("gemini %s: HTTP %d: %s", want, resp.StatusCode, snippet(raw))
			continue
		}
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("gemini %s: HTTP %d: %s", want, resp.StatusCode, snippet(raw))
		}
		var r interactionResp
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, fmt.Errorf("gemini %s: bad JSON: %w", want, err)
		}
		// Take the last block of the wanted type from the model's output.
		var data string
		for _, s := range r.Steps {
			if s.Type != "model_output" {
				continue
			}
			for _, c := range s.Content {
				if c.Type == want && c.Data != "" {
					data = c.Data
				}
			}
		}
		if data == "" {
			return nil, fmt.Errorf("gemini %s: no %s in response (often a safety block): %s", want, want, snippet(raw))
		}
		return base64.StdEncoding.DecodeString(data)
	}
	return nil, lastErr
}

func snippet(b []byte) string {
	if len(b) > 300 {
		return string(b[:300]) + "..."
	}
	return string(b)
}
