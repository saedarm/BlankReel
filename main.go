// Blank Reel: a daily fill-in-the-blank story that renders into a short trailer.
package main

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // so TZ=America/New_York works in a bare container
	"unicode"
)

//go:embed stories.json
var storiesJSON []byte

//go:embed web/index.html
var indexHTML []byte

type Blank struct {
	Label string `json:"label"`
	Hint  string `json:"hint"`
}
type Scene struct {
	Line string `json:"line"`
	Shot string `json:"shot"`
}
type Story struct {
	Title   string  `json:"title"`
	Logline string  `json:"logline"`
	Blanks  []Blank `json:"blanks"`
	Scenes  []Scene `json:"scenes"`
}

type Job struct {
	ID       string    `json:"id"`
	Day      int       `json:"day"`
	Story    int       `json:"story"`
	Title    string    `json:"title"`
	Answers  []string  `json:"answers"`
	Status   string    `json:"status"` // queued, rendering, done, failed
	Step     string    `json:"step"`
	Done     int       `json:"done"`
	Total    int       `json:"total"`
	Error    string    `json:"error,omitempty"`
	Created  time.Time `json:"created"`
	Position int       `json:"position,omitempty"`
}

type server struct {
	stories []Story
	gemini  *Gemini // nil = mock mode
	dataDir string
	code    string
	limit   int
	launch  time.Time
	mu      sync.Mutex
	jobs    map[string]*Job
	queue   chan *Job
	waiting []*Job
	perDay  map[string]int
}

var idRe = regexp.MustCompile(`^[a-f0-9]{10}$`)

func main() {
	s := &server{
		dataDir: env("DATA_DIR", "data"),
		code:    os.Getenv("BLANKREEL_CODE"),
		jobs:    map[string]*Job{},
		queue:   make(chan *Job, 200),
		perDay:  map[string]int{},
	}
	if err := json.Unmarshal(storiesJSON, &s.stories); err != nil {
		log.Fatalf("stories.json: %v", err)
	}
	s.limit, _ = strconv.Atoi(env("BLANKREEL_DAILY_LIMIT", "40"))
	s.launch, _ = time.ParseInLocation("2006-01-02", env("BLANKREEL_LAUNCH", "2026-10-01"), time.Local)
	if key := os.Getenv("GEMINI_API_KEY"); key != "" {
		s.gemini = &Gemini{
			Key:        key,
			ImageModel: env("BLANKREEL_IMAGE_MODEL", "gemini-3.1-flash-lite-image"),
			TTSModel:   env("BLANKREEL_TTS_MODEL", "gemini-3.8-flash-lite-tts"),
			Voice:      env("BLANKREEL_VOICE", "Charon"),
			HTTP:       &http.Client{Timeout: 3 * time.Minute},
		}
		log.Printf("Gemini on: images=%s voice=%s/%s", s.gemini.ImageModel, s.gemini.TTSModel, s.gemini.Voice)
	} else {
		log.Printf("No GEMINI_API_KEY: mock mode (gradient frames, silent audio)")
	}
	if err := os.MkdirAll(s.dataDir, 0o755); err != nil {
		log.Fatal(err)
	}
	go s.worker()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/today", s.today)
	mux.HandleFunc("POST /api/trailers", s.create)
	mux.HandleFunc("GET /api/trailers/{id}", s.status)
	mux.HandleFunc("GET /v/{file}", s.media)
	mux.HandleFunc("GET /t/{id}", s.sharePage)

	addr := ":" + env("PORT", "8080")
	log.Printf("Blank Reel on http://localhost%s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func (s *server) dayNo() int {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	return int(today.Sub(s.launch).Hours()/24+0.5) + 1
}

func (s *server) today(w http.ResponseWriter, r *http.Request) {
	day := s.dayNo()
	idx := ((day-1)%len(s.stories) + len(s.stories)) % len(s.stories)
	practice := false
	if p := r.URL.Query().Get("story"); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n >= 0 && n < len(s.stories) {
			idx, practice = n, n != idx
		}
	}
	writeJSON(w, 200, map[string]any{
		"day": day, "storyIndex": idx, "storyCount": len(s.stories), "practice": practice,
		"story": s.stories[idx], "needsCode": s.code != "", "mock": s.gemini == nil,
	})
}

func (s *server) create(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Story   int      `json:"story"`
		Answers []string `json:"answers"`
		Code    string   `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&in); err != nil {
		writeErr(w, 400, "Couldn't read that request.")
		return
	}
	if s.code != "" && in.Code != s.code {
		writeErr(w, 401, "Wrong crew code. Ask whoever runs this server for it.")
		return
	}
	if in.Story < 0 || in.Story >= len(s.stories) {
		writeErr(w, 400, "That story doesn't exist.")
		return
	}
	st := s.stories[in.Story]
	if len(in.Answers) != len(st.Blanks) {
		writeErr(w, 400, fmt.Sprintf("Fill all %d blanks first.", len(st.Blanks)))
		return
	}
	for i, a := range in.Answers {
		a = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, a)), " ")
		if a == "" || len([]rune(a)) > 40 {
			writeErr(w, 400, fmt.Sprintf("Blank %d needs 1 to 40 characters.", i+1))
			return
		}
		in.Answers[i] = a
	}

	dayKey := time.Now().Format("2006-01-02")
	s.mu.Lock()
	if s.perDay[dayKey] >= s.limit {
		s.mu.Unlock()
		writeErr(w, 429, fmt.Sprintf("We've hit today's limit of %d trailers. Try again tomorrow.", s.limit))
		return
	}
	s.perDay[dayKey]++
	j := &Job{ID: newID(), Day: s.dayNo(), Story: in.Story, Title: st.Title, Answers: in.Answers,
		Status: "queued", Step: "Waiting in line", Created: time.Now()}
	s.jobs[j.ID] = j
	s.waiting = append(s.waiting, j)
	s.mu.Unlock()

	select {
	case s.queue <- j:
	default:
		s.fail(j, "The render queue is full. Try again in a few minutes.")
	}
	writeJSON(w, 202, map[string]string{"id": j.ID})
}

// worker renders one trailer at a time so API spend and CPU stay predictable.
func (s *server) worker() {
	for j := range s.queue {
		s.mu.Lock()
		j.Status, j.Step = "rendering", "Starting"
		if len(s.waiting) > 0 {
			s.waiting = s.waiting[1:]
		}
		s.mu.Unlock()

		dir := filepath.Join(s.dataDir, j.ID)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		err := os.MkdirAll(dir, 0o755)
		if err == nil {
			err = renderTrailer(ctx, s.gemini, dir, s.stories[j.Story], j.Answers, j.Day, func(step string, done, total int) {
				s.mu.Lock()
				j.Step, j.Done, j.Total = step, done, total
				s.mu.Unlock()
			})
		}
		cancel()
		if err != nil {
			log.Printf("trailer %s failed: %v", j.ID, err)
			s.fail(j, "The trailer didn't render: "+err.Error())
			s.mu.Lock()
			s.perDay[j.Created.Format("2006-01-02")]-- // failed renders don't count against the limit
			s.mu.Unlock()
			os.RemoveAll(dir)
			continue
		}
		s.mu.Lock()
		j.Status, j.Step = "done", "Done"
		meta, _ := json.MarshalIndent(j, "", "  ")
		s.mu.Unlock()
		os.WriteFile(filepath.Join(dir, "meta.json"), meta, 0o644)
		log.Printf("trailer %s done (%s)", j.ID, j.Title)
	}
}

func (s *server) fail(j *Job, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j.Status, j.Error = "failed", msg
}

func (s *server) lookup(id string) *Job {
	if !idRe.MatchString(id) {
		return nil
	}
	s.mu.Lock()
	j := s.jobs[id]
	s.mu.Unlock()
	if j != nil {
		return j
	}
	// Finished trailers survive restarts on disk.
	b, err := os.ReadFile(filepath.Join(s.dataDir, id, "meta.json"))
	if err != nil {
		return nil
	}
	j = &Job{}
	if json.Unmarshal(b, j) != nil {
		return nil
	}
	return j
}

func (s *server) status(w http.ResponseWriter, r *http.Request) {
	j := s.lookup(r.PathValue("id"))
	if j == nil {
		writeErr(w, 404, "No trailer with that ID.")
		return
	}
	s.mu.Lock()
	out := *j
	for i, q := range s.waiting {
		if q == j {
			out.Position = i + 1
		}
	}
	s.mu.Unlock()
	writeJSON(w, 200, out)
}

func (s *server) media(w http.ResponseWriter, r *http.Request) {
	file := r.PathValue("file")
	id, ext, _ := strings.Cut(file, ".")
	name := map[string]string{"mp4": "trailer.mp4", "jpg": "poster.jpg"}[ext]
	if !idRe.MatchString(id) || name == "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFile(w, r, filepath.Join(s.dataDir, id, name))
}

var shareTmpl = template.Must(template.New("share").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}} · Blank Reel</title>
<meta property="og:title" content="{{.Title}} · Blank Reel #{{.Day}}">
<meta property="og:description" content="{{.Last}}">
<meta property="og:type" content="video.other">
<meta property="og:image" content="{{.Base}}/v/{{.ID}}.jpg">
<meta property="og:video" content="{{.Base}}/v/{{.ID}}.mp4">
<meta property="og:video:type" content="video/mp4">
<meta property="og:video:width" content="1280"><meta property="og:video:height" content="720">
<meta name="twitter:card" content="player">
<style>
body{margin:0;background:#0c0f24;color:#eef0ff;font:16px/1.5 system-ui,sans-serif}
main{max-width:860px;margin:0 auto;padding:24px 16px;display:flex;flex-direction:column;gap:16px}
h1{font-size:1.6rem;margin:0}p{margin:0;color:#9ea5cc}
video{width:100%;border-radius:12px;background:#000}
a{display:inline-block;background:#ffd94a;color:#151a3e;font-weight:700;padding:12px 18px;border-radius:10px;text-decoration:none;align-self:flex-start}
</style></head><body><main>
<p>Blank Reel #{{.Day}}</p><h1>{{.Title}}</h1>
<video src="/v/{{.ID}}.mp4" poster="/v/{{.ID}}.jpg" controls playsinline></video>
<a href="/">Fill in today's blanks</a>
</main></body></html>`))

func (s *server) sharePage(w http.ResponseWriter, r *http.Request) {
	j := s.lookup(r.PathValue("id"))
	if j == nil || j.Status != "done" {
		http.NotFound(w, r)
		return
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	scenes := s.stories[j.Story].Scenes
	shareTmpl.Execute(w, map[string]any{
		"ID": j.ID, "Title": j.Title, "Day": j.Day, "Base": scheme + "://" + r.Host,
		"Last": fillPlain(scenes[len(scenes)-1].Line, j.Answers),
	})
}

var slotRe = regexp.MustCompile(`\{(\d)\}`)

func fillPlain(s string, answers []string) string {
	return slotRe.ReplaceAllStringFunc(s, func(m string) string {
		i := int(m[1] - '0')
		if i < len(answers) {
			return answers[i]
		}
		return "___"
	})
}

func newID() string {
	b := make([]byte, 5)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
