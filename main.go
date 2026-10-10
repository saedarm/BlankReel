// Blank Reel: a daily fill-in-the-blank story that renders into a short trailer.
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // so TZ=America/New_York works in a bare container
)

//go:embed stories.json
var storiesJSON []byte

//go:embed words.json
var wordsJSON []byte

//go:embed web/index.html
var indexHTML []byte

type Blank struct {
	Label string `json:"label"`
	Hint  string `json:"hint"`
	Kind  string `json:"kind"`
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

// Job is render progress, kept in memory only. The trailer row in the
// database is the record that survives restarts.
type Job struct {
	ID    string
	Step  string
	Done  int
	Total int
}

type server struct {
	stories     []Story
	gemini      *Gemini // nil = test mode
	store       *Store
	dataDir     string
	masterCode  string
	adminKey    string
	dailyLimit  int
	playerLimit int
	review      bool     // published trailers wait for approval
	blocklist   []string // answers containing these always wait for approval
	launch      time.Time

	mu      sync.Mutex
	jobs    map[string]*Job
	waiting []string
	queue   chan *Trailer
}

var idRe = regexp.MustCompile(`^[a-f0-9]{10}$`)

func main() {
	s := &server{
		dataDir:    env("DATA_DIR", "data"),
		masterCode: normCode(os.Getenv("BLANKREEL_CODE")),
		adminKey:   os.Getenv("BLANKREEL_ADMIN_KEY"),
		jobs:       map[string]*Job{},
		queue:      make(chan *Trailer, 200),
	}
	if err := json.Unmarshal(storiesJSON, &s.stories); err != nil {
		log.Fatalf("stories.json: %v", err)
	}
	var words map[string][]string
	if err := json.Unmarshal(wordsJSON, &words); err != nil {
		log.Fatalf("words.json: %v", err)
	}
	for i, st := range s.stories {
		for _, b := range st.Blanks {
			if b.Kind != "number" && len(words[b.Kind]) == 0 {
				log.Printf("warning: story %d blank %q has no word list for kind %q", i, b.Label, b.Kind)
			}
		}
	}
	s.dailyLimit, _ = strconv.Atoi(env("BLANKREEL_DAILY_LIMIT", "40"))
	s.playerLimit, _ = strconv.Atoi(env("BLANKREEL_PLAYER_LIMIT", "10"))
	s.review = !strings.EqualFold(os.Getenv("BLANKREEL_REVIEW"), "off")
	s.blocklist = loadBlocklist()
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
		log.Printf("No GEMINI_API_KEY: test mode (color frames, silent audio)")
	}
	if err := os.MkdirAll(s.dataDir, 0o755); err != nil {
		log.Fatal(err)
	}
	var err error
	if s.store, err = openStore(filepath.Join(s.dataDir, "blankreel.db")); err != nil {
		log.Fatalf("database: %v", err)
	}
	if s.adminKey == "" {
		log.Printf("No BLANKREEL_ADMIN_KEY: admin page is off")
	}
	go s.worker()

	mux := http.NewServeMux()
	page := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(indexHTML)
	}
	mux.HandleFunc("GET /{$}", page)
	mux.HandleFunc("GET /rooms", page)
	mux.HandleFunc("GET /room/{id}", page)
	mux.HandleFunc("GET /r/{code}", page)
	mux.HandleFunc("GET /feed", page)

	mux.HandleFunc("GET /api/today", s.today)
	mux.HandleFunc("GET /api/words", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Write(wordsJSON)
	})
	mux.HandleFunc("POST /api/players", s.newPlayer)
	mux.HandleFunc("GET /api/me", s.me)
	mux.HandleFunc("POST /api/me/name", s.setName)
	mux.HandleFunc("POST /api/redeem", s.redeem)
	mux.HandleFunc("POST /api/trailers", s.create)
	mux.HandleFunc("GET /api/trailers/{id}", s.status)
	mux.HandleFunc("POST /api/trailers/{id}/publish", s.publish)
	mux.HandleFunc("POST /api/trailers/{id}/unpublish", s.unpublish)
	mux.HandleFunc("POST /api/trailers/{id}/react", s.react)
	mux.HandleFunc("POST /api/trailers/{id}/report", s.reportTrailer)
	mux.HandleFunc("POST /api/plays", s.recordPlay)
	mux.HandleFunc("GET /api/feed", s.feedHandler)
	mux.HandleFunc("GET /api/rooms", s.listRooms)
	mux.HandleFunc("POST /api/rooms", s.createRoom)
	mux.HandleFunc("POST /api/rooms/join", s.joinRoom)
	mux.HandleFunc("GET /api/rooms/{id}", s.roomWall)
	mux.HandleFunc("POST /api/rooms/{id}/vote", s.vote)
	mux.HandleFunc("POST /api/rooms/{id}/leave", s.leaveRoom)
	mux.HandleFunc("GET /v/{file}", s.media)
	mux.HandleFunc("GET /t/{id}", s.sharePage)
	mux.HandleFunc("GET /admin", s.adminPage)
	mux.HandleFunc("POST /admin/{action}", s.adminAction)

	addr := ":" + env("PORT", "8080")
	log.Printf("Blank Reel on http://localhost%s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// ---- days ----

func midnight(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local)
}

func (s *server) dayNo() int { return s.dayOf(time.Now()) }

func (s *server) dayOf(t time.Time) int { return int(midnight(t).Sub(s.launch).Hours()/24+0.5) + 1 }

func (s *server) dateOf(day int) time.Time { return s.launch.AddDate(0, 0, day-1) }

func (s *server) storyFor(day int) int {
	n := len(s.stories)
	return ((day-1)%n + n) % n
}

// monthDays is the first and last day number of the calendar month a day falls in.
func (s *server) monthDays(day int) (int, int) {
	d := s.dateOf(day)
	first := time.Date(d.Year(), d.Month(), 1, 0, 0, 0, 0, time.Local)
	return s.dayOf(first), s.dayOf(first.AddDate(0, 1, -1))
}

// ---- render queue ----

// worker renders one trailer at a time so API spend and CPU stay predictable.
func (s *server) worker() {
	for t := range s.queue {
		s.mu.Lock()
		if len(s.waiting) > 0 {
			s.waiting = s.waiting[1:]
		}
		job := &Job{ID: t.ID, Step: "Starting"}
		s.jobs[t.ID] = job
		s.mu.Unlock()
		s.store.setTrailerStatus(t.ID, "rendering", "")

		dir := filepath.Join(s.dataDir, t.ID)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		err := os.MkdirAll(dir, 0o755)
		if err == nil {
			err = renderTrailer(ctx, s.gemini, dir, s.stories[t.Story], t.Answers, t.Day, func(step string, done, total int) {
				s.mu.Lock()
				job.Step, job.Done, job.Total = step, done, total
				s.mu.Unlock()
			})
		}
		cancel()
		s.mu.Lock()
		delete(s.jobs, t.ID)
		s.mu.Unlock()
		if err != nil {
			log.Printf("trailer %s failed: %v", t.ID, err)
			s.store.setTrailerStatus(t.ID, "failed", "The trailer didn't render: "+err.Error())
			os.RemoveAll(dir)
			continue
		}
		s.store.setTrailerStatus(t.ID, "done", "")
		log.Printf("trailer %s done (%s)", t.ID, t.Title)
	}
}

// ---- helpers ----

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func newID() string    { return randHex(5) }
func newToken() string { return randHex(16) }

func newJoinCode() string {
	const alpha = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"
	b := make([]byte, 6)
	rand.Read(b)
	for i := range b {
		b[i] = alpha[int(b[i])%len(alpha)]
	}
	return string(b)
}

func sameSecret(a, b string) bool {
	return a != "" && b != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func sortScores(sc []Score) {
	sort.Slice(sc, func(i, j int) bool {
		if sc[i].Votes != sc[j].Votes {
			return sc[i].Votes > sc[j].Votes
		}
		if sc[i].Crowns != sc[j].Crowns {
			return sc[i].Crowns > sc[j].Crowns
		}
		return sc[i].Name < sc[j].Name
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

// cleanText trims, collapses spaces, drops control characters, and caps length.
func cleanText(s string, max int) string {
	s = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, s)), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max])
	}
	return s
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(v); err != nil {
		writeErr(w, 400, "Couldn't read that request.")
		return false
	}
	return true
}
