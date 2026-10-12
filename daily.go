package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Every day gets its own story. The server asks Gemini to write tomorrow's
// story ahead of time. If that fails (or there's no API key), the day falls
// back to the least-recently-used story from stories.json. Once a day's story
// is chosen it's locked in the database, so it never changes mid-day.
//
// Story IDs: 0..N-1 are stories.json entries. dailyBase+day is the story the
// AI wrote for that day.

const (
	dailyBase   = 1_000_000
	legacyCount = 3 // days before this feature rotated through the first 3 stories
)

type dailyStore struct {
	mu    sync.Mutex
	cache map[int]dailyRow // by day
}

type dailyRow struct {
	Day     int
	StoryID int
	Story   *Story // set for AI stories
	Source  string // ai or pool
}

func (s *Store) getDaily(day int) (dailyRow, bool) {
	var r dailyRow
	var js sql.NullString
	err := s.db.QueryRow(`SELECT day, story_id, story, source FROM daily_stories WHERE day=?`, day).Scan(&r.Day, &r.StoryID, &js, &r.Source)
	if err != nil {
		return r, false
	}
	if js.Valid && js.String != "" {
		var st Story
		if json.Unmarshal([]byte(js.String), &st) == nil {
			r.Story = &st
		}
	}
	return r, true
}

// putDaily stores a day's story unless one is already there; the first writer wins.
func (s *Store) putDaily(day, storyID int, st *Story, source string) error {
	var js any
	if st != nil {
		b, _ := json.Marshal(st)
		js = string(b)
	}
	_, err := s.db.Exec(`INSERT OR IGNORE INTO daily_stories(day, story_id, story, source, created) VALUES(?,?,?,?,?)`, day, storyID, js, source, now())
	return err
}

func (s *Store) deleteDaily(day int) error {
	_, err := s.db.Exec(`DELETE FROM daily_stories WHERE day=?`, day)
	return err
}

func (s *Store) dailyRows(since int) ([]dailyRow, error) {
	rows, err := s.db.Query(`SELECT day, story_id, source FROM daily_stories WHERE day>=? ORDER BY day`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []dailyRow
	for rows.Next() {
		var r dailyRow
		rows.Scan(&r.Day, &r.StoryID, &r.Source)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) dayHasActivity(day int) bool {
	var n int
	s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM plays WHERE day=?) + (SELECT COUNT(*) FROM trailers WHERE day=? AND daily=1)`, day, day).Scan(&n)
	return n > 0
}

// recentTitles lists AI story titles so the writer doesn't repeat itself.
func (s *Store) recentTitles(n int) []string {
	rows, err := s.db.Query(`SELECT story FROM daily_stories WHERE source='ai' ORDER BY day DESC LIMIT ?`, n)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var js string
		var st Story
		if rows.Scan(&js) == nil && json.Unmarshal([]byte(js), &st) == nil {
			out = append(out, st.Title)
		}
	}
	return out
}

// ---- lookups ----

// daily returns the locked-in story for a day, choosing one if the day is
// today or earlier and nothing is stored yet.
func (s *server) daily(day int) dailyRow {
	s.days.mu.Lock()
	defer s.days.mu.Unlock()
	if r, ok := s.days.cache[day]; ok {
		return r
	}
	if r, ok := s.store.getDaily(day); ok && s.validID(r) {
		s.days.cache[day] = r
		return r
	}
	today := s.dayNo()
	if day > today {
		return dailyRow{Day: day, StoryID: s.pickPool(day), Source: "pool"} // preview only, not stored
	}
	// A day with no stored story: before this feature, or today before the writer ran.
	id := (day - 1) % legacyCount
	if id < 0 {
		id += legacyCount
	}
	if day < today {
		r := dailyRow{Day: day, StoryID: id, Source: "pool"}
		s.days.cache[day] = r
		return r
	}
	// Today: keep the old rotation if anyone already played it, otherwise the stalest backup story.
	if !s.store.dayHasActivity(day) {
		id = s.pickPool(day)
	}
	s.store.putDaily(day, id, nil, "pool")
	r, ok := s.store.getDaily(day)
	if !ok {
		r = dailyRow{Day: day, StoryID: id, Source: "pool"}
	}
	s.days.cache[day] = r
	return r
}

func (s *server) validID(r dailyRow) bool {
	if r.StoryID >= dailyBase {
		return r.Story != nil
	}
	return r.StoryID >= 0 && r.StoryID < len(s.stories)
}

// storyFor is the story ID for a day.
func (s *server) storyFor(day int) int { return s.daily(day).StoryID }

// storyByID resolves a story ID from stories.json or an AI-written day.
func (s *server) storyByID(id int) (Story, bool) {
	if id >= 0 && id < len(s.stories) {
		return s.stories[id], true
	}
	if id >= dailyBase {
		if r := s.daily(id - dailyBase); r.Story != nil && r.StoryID == id {
			return *r.Story, true
		}
	}
	return Story{}, false
}

// mustStory is storyByID for IDs that came from storyFor.
func (s *server) mustStory(id int) Story {
	if st, ok := s.storyByID(id); ok {
		return st
	}
	return s.stories[0]
}

// pickPool chooses the stories.json entry used longest ago.
func (s *server) pickPool(day int) int {
	last := make([]int, len(s.stories))
	for i := range last {
		last[i] = -1 << 30
	}
	rows, _ := s.store.dailyRows(day - 400)
	stored := map[int]bool{}
	for _, r := range rows {
		stored[r.Day] = true
		if r.StoryID < len(s.stories) && r.Day < day {
			last[r.StoryID] = max(last[r.StoryID], r.Day)
		}
	}
	for d := max(1, day-400); d < day; d++ {
		if !stored[d] {
			i := (d - 1) % legacyCount
			last[i] = max(last[i], d)
		}
	}
	best := 0
	for i := range last {
		if last[i] < last[best] {
			best = i
		}
	}
	return best
}

// ---- the writer ----

var storySchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"title":   map[string]any{"type": "string"},
		"logline": map[string]any{"type": "string"},
		"blanks": map[string]any{"type": "array", "items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"label": map[string]any{"type": "string"},
				"hint":  map[string]any{"type": "string"},
				"kind":  map[string]any{"type": "string"},
			},
			"required": []string{"label", "hint", "kind"},
		}},
		"scenes": map[string]any{"type": "array", "items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"line": map[string]any{"type": "string"},
				"shot": map[string]any{"type": "string"},
			},
			"required": []string{"line", "shot"},
		}},
	},
	"required": []string{"title", "logline", "blanks", "scenes"},
}

func (s *server) storyPrompt(avoid []string) string {
	ex, _ := json.MarshalIndent(s.stories[0], "", "  ")
	kinds := append(append([]string{}, s.kinds...), "number")
	return fmt.Sprintf(`You write the daily story for Blank Reel, a fill-in-the-blank comedy game (like Mad Libs). Players fill in the blanks without seeing the story, then the story becomes a 30-second narrated trailer with one AI image per scene.

Write ONE new story as JSON in exactly this shape:
%s

Rules:
- "title": 2 to 6 words. "logline": one short teaser sentence that doesn't give away the jokes.
- Exactly 9 blanks. Each has a "label" the player sees (like "Animal (plural)" or "Verb ending in -ing"), a short example "hint", and a "kind" chosen from this list only: %s.
- Exactly 6 scenes that tell one story with a beginning, a middle, and a punchline ending.
- "line" is what the narrator reads aloud: one or two sentences, 25 words max. Use {0} through {8} where the answers go. Every blank must appear in at least one line. The answer must fit grammatically wherever it lands.
- "shot" describes the image for that scene: a concrete visual of people, places, and objects, using the same {n} slots. No text, signs with words, logos, brand names, or real people.
- Comedy for adults that's still fine to share at work: no sex, slurs, gore, drugs, or politics.
- Pick an everyday setting people know (work, school, family events, sports, holidays, small towns, jobs) or a silly genre (heist, space, monsters, westerns). Make the narrator deadpan.
- Don't reuse these recent titles or their premises: %s.
Return only the JSON.`, ex, strings.Join(kinds, ", "), strings.Join(avoid, "; "))
}

var slotAny = regexp.MustCompile(`\{([^}]*)\}`)

// validateStory makes sure an AI story will work in the game before it's used.
func (s *server) validateStory(st *Story) error {
	st.Title, st.Logline = cleanText(st.Title, 60), cleanText(st.Logline, 140)
	if st.Title == "" || st.Logline == "" {
		return errors.New("missing title or logline")
	}
	if len(st.Blanks) < 6 || len(st.Blanks) > 10 {
		return fmt.Errorf("%d blanks", len(st.Blanks))
	}
	if len(st.Scenes) != 6 {
		return fmt.Errorf("%d scenes", len(st.Scenes))
	}
	known := map[string]bool{"number": true}
	for _, k := range s.kinds {
		known[k] = true
	}
	for i := range st.Blanks {
		b := &st.Blanks[i]
		b.Label, b.Hint, b.Kind = cleanText(b.Label, 40), cleanText(b.Hint, 30), strings.TrimSpace(b.Kind)
		if b.Label == "" || b.Hint == "" || !known[b.Kind] {
			return fmt.Errorf("blank %d: bad label/hint/kind %q", i, b.Kind)
		}
	}
	used := map[int]bool{}
	for i := range st.Scenes {
		sc := &st.Scenes[i]
		sc.Line, sc.Shot = cleanText(sc.Line, 220), cleanText(sc.Shot, 400)
		if sc.Line == "" || sc.Shot == "" {
			return fmt.Errorf("scene %d empty", i)
		}
		for _, field := range []string{sc.Line, sc.Shot} {
			for _, m := range slotAny.FindAllStringSubmatch(field, -1) {
				if len(m[1]) != 1 || m[1][0] < '0' || int(m[1][0]-'0') >= len(st.Blanks) {
					return fmt.Errorf("scene %d: bad slot {%s}", i, m[1])
				}
				if field == sc.Line {
					used[int(m[1][0]-'0')] = true
				}
			}
		}
	}
	if len(used) != len(st.Blanks) {
		return fmt.Errorf("only %d of %d blanks used in lines", len(used), len(st.Blanks))
	}
	var text []string
	for _, sc := range st.Scenes {
		text = append(text, sc.Line, sc.Shot)
	}
	if s.flagged(append(text, st.Title, st.Logline)) {
		return errors.New("hit the word filter")
	}
	return nil
}

// writeStory asks Gemini for a new story, retrying a few times on bad output.
func (s *server) writeStory(ctx context.Context) (*Story, error) {
	if s.gemini == nil {
		return nil, errors.New("no GEMINI_API_KEY")
	}
	avoid := s.store.recentTitles(30)
	for _, st := range s.stories {
		avoid = append(avoid, st.Title)
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		raw, err := s.gemini.JSON(ctx, s.storyPrompt(avoid), storySchema)
		if err != nil {
			lastErr = err
			continue
		}
		var st Story
		if err := json.Unmarshal(raw, &st); err != nil {
			lastErr = fmt.Errorf("bad JSON: %w", err)
			continue
		}
		if err := s.validateStory(&st); err != nil {
			lastErr = err
			log.Printf("story writer: rejected draft %q: %v", st.Title, err)
			continue
		}
		return &st, nil
	}
	return nil, lastErr
}

// ensureDay writes and stores an AI story for a day that doesn't have one yet.
func (s *server) ensureDay(ctx context.Context, day int) error {
	if _, ok := s.store.getDaily(day); ok {
		return nil
	}
	st, err := s.writeStory(ctx)
	if err != nil {
		return err
	}
	if err := s.store.putDaily(day, dailyBase+day, st, "ai"); err != nil {
		return err
	}
	log.Printf("story writer: day %d is %q", day, st.Title)
	return nil
}

// storyWriter keeps tomorrow's story ready. It checks at startup and every 15 minutes.
func (s *server) storyWriter() {
	for {
		if s.gemini != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			if err := s.ensureDay(ctx, s.dayNo()+1); err != nil {
				log.Printf("story writer: tomorrow's story failed, will retry: %v", err)
			}
			cancel()
		}
		time.Sleep(15 * time.Minute)
	}
}

// rewriteTomorrow throws out tomorrow's story and writes a new one (admin button).
func (s *server) rewriteTomorrow(ctx context.Context) error {
	day := s.dayNo() + 1
	st, err := s.writeStory(ctx)
	if err != nil {
		return err
	}
	s.store.deleteDaily(day)
	s.days.mu.Lock()
	delete(s.days.cache, day)
	s.days.mu.Unlock()
	return s.store.putDaily(day, dailyBase+day, st, "ai")
}
