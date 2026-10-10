package main

import (
	_ "embed"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// The public feed: players publish a finished trailer, it waits in the review
// queue (unless BLANKREEL_REVIEW=off and nothing trips the word filter), and
// once approved anyone can like or dislike it. Three reports hide it.

//go:embed blocklist.txt
var blocklistTxt string

const reportsToHide = 3

func loadBlocklist() []string {
	var out []string
	for _, l := range strings.Split(blocklistTxt, "\n") {
		// Keep surrounding spaces: " ass " means whole word only.
		l = strings.ToLower(strings.TrimRight(l, "\r"))
		if t := strings.TrimSpace(l); t != "" && !strings.HasPrefix(t, "#") {
			out = append(out, l)
		}
	}
	if extra := os.Getenv("BLANKREEL_BLOCKLIST"); extra != "" {
		for _, w := range strings.Split(extra, ",") {
			if w = strings.ToLower(strings.TrimSpace(w)); w != "" {
				out = append(out, w)
			}
		}
	}
	return out
}

// flagged reports whether any answer contains a blocked word or phrase.
func (s *server) flagged(answers []string) bool {
	for _, a := range answers {
		raw := " " + strings.ToLower(a) + " "
		words := " " + strings.Join(strings.FieldsFunc(strings.ToLower(a), func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
		}), " ") + " "
		for _, w := range s.blocklist {
			if strings.Contains(raw, w) || strings.Contains(words, w) {
				return true
			}
		}
	}
	return false
}

// ---- store ----

func (s *Store) setPublic(id, state string, published int64, flagged bool) error {
	_, err := s.db.Exec(`UPDATE trailers SET public=?, published=?, flagged=? WHERE id=?`, state, published, b2i(flagged), id)
	return err
}

func (s *Store) setFeatured(id string, on bool) error {
	v := int64(0)
	if on {
		v = now()
	}
	_, err := s.db.Exec(`UPDATE trailers SET featured=? WHERE id=?`, v, id)
	return err
}

func (s *Store) react(trailerID, playerID string, value int) error {
	if value == 0 {
		_, err := s.db.Exec(`DELETE FROM reactions WHERE trailer_id=? AND player_id=?`, trailerID, playerID)
		return err
	}
	_, err := s.db.Exec(`INSERT INTO reactions(trailer_id, player_id, value, created) VALUES(?,?,?,?)
		ON CONFLICT(trailer_id, player_id) DO UPDATE SET value=excluded.value, created=excluded.created`, trailerID, playerID, value, now())
	return err
}

// report records one report per player and returns the total.
func (s *Store) report(trailerID, playerID string) (int, error) {
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO reports(trailer_id, player_id, created) VALUES(?,?,?)`, trailerID, playerID, now()); err != nil {
		return 0, err
	}
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM reports WHERE trailer_id=?`, trailerID).Scan(&n)
	return n, err
}

const feedCounts = `,
	(SELECT COUNT(*) FROM reactions r WHERE r.trailer_id=t.id AND r.value=1),
	(SELECT COUNT(*) FROM reactions r WHERE r.trailer_id=t.id AND r.value=-1),
	(SELECT COUNT(*) FROM reports x WHERE x.trailer_id=t.id),
	COALESCE((SELECT value FROM reactions r WHERE r.trailer_id=t.id AND r.player_id=?), 0)`

// feed lists trailers with like, dislike, and report counts, plus the viewer's own reaction.
func (s *Store) feed(viewer, where string, args ...any) ([]*Trailer, error) {
	rows, err := s.db.Query(`SELECT `+trailerCols+feedCounts+` FROM trailers t LEFT JOIN players p ON p.id=t.player_id WHERE `+where,
		append([]any{viewer}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Trailer{}
	for rows.Next() {
		var likes, dislikes, reports, mine int
		t, err := scanTrailer(rows, &likes, &dislikes, &reports, &mine)
		if err != nil {
			return nil, err
		}
		t.Likes, t.Dislikes, t.Reports, t.Mine = likes, dislikes, reports, mine
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) recordPlay(playerID string, day, story int, answers []string) error {
	a, _ := json.Marshal(answers)
	_, err := s.db.Exec(`INSERT INTO plays(player_id, day, story, answers, created) VALUES(?,?,?,?,?)
		ON CONFLICT(player_id, day, story) DO UPDATE SET answers=excluded.answers`, playerID, day, story, string(a), now())
	return err
}

type play struct {
	PlayerID string
	Name     string
	Answers  []string
}

func (s *Store) plays(day, story int) ([]play, error) {
	rows, err := s.db.Query(`SELECT pl.player_id, COALESCE(p.name,''), pl.answers FROM plays pl LEFT JOIN players p ON p.id=pl.player_id WHERE pl.day=? AND pl.story=?`, day, story)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []play
	for rows.Next() {
		var pl play
		var a string
		rows.Scan(&pl.PlayerID, &pl.Name, &a)
		json.Unmarshal([]byte(a), &pl.Answers)
		out = append(out, pl)
	}
	return out, rows.Err()
}

// ---- originality ----

// Each blank scores up to 100: the fewer players who typed the same answer,
// the more it's worth. Krillion's rule, applied to ad-libs.
type blankScore struct {
	Answer string `json:"answer"`
	Same   int    `json:"same"` // players with this answer, including you
	Points int    `json:"points"`
}

type scoreCard struct {
	Total   int          `json:"total"`
	Max     int          `json:"max"`
	Players int          `json:"players"`
	Blanks  []blankScore `json:"blanks"`
}

func normAnswer(a string) string {
	a = strings.ToLower(strings.Join(strings.Fields(a), " "))
	return strings.Trim(a, ".!?,'\"")
}

func scoreAnswers(answers []string, all []play) scoreCard {
	total := len(all)
	if total == 0 {
		total = 1
	}
	sc := scoreCard{Max: 100 * len(answers), Players: total}
	for i, a := range answers {
		same := 0
		for _, pl := range all {
			if i < len(pl.Answers) && normAnswer(pl.Answers[i]) == normAnswer(a) {
				same++
			}
		}
		same = max(same, 1)
		pts := int(100*float64(total-same+1)/float64(total) + 0.5)
		sc.Blanks = append(sc.Blanks, blankScore{Answer: a, Same: same, Points: pts})
		sc.Total += pts
	}
	return sc
}

// ---- handlers ----

// recordPlay saves the answers for today's story when the story is revealed,
// which costs nothing, and returns the player's originality score.
func (s *server) recordPlay(w http.ResponseWriter, r *http.Request) {
	p := s.player(w, r)
	if p == nil {
		return
	}
	var in struct {
		Story   int      `json:"story"`
		Answers []string `json:"answers"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	day := s.dayNo()
	if in.Story != s.storyFor(day) || len(in.Answers) != len(s.stories[in.Story].Blanks) {
		writeJSON(w, 200, map[string]any{"daily": false})
		return
	}
	for i, a := range in.Answers {
		in.Answers[i] = cleanText(a, 40)
	}
	if err := s.store.recordPlay(p.ID, day, in.Story, in.Answers); err != nil {
		log.Printf("record play: %v", err)
	}
	all, _ := s.store.plays(day, in.Story)
	writeJSON(w, 200, map[string]any{"daily": true, "score": scoreAnswers(in.Answers, all)})
}

type boardRow struct {
	Name  string `json:"name"`
	Score int    `json:"score"`
	Me    bool   `json:"me"`
}

// originalBoard ranks today's plays by originality.
func (s *server) originalBoard(viewer string) ([]boardRow, int) {
	day := s.dayNo()
	all, _ := s.store.plays(day, s.storyFor(day))
	rows := make([]boardRow, 0, len(all))
	for _, pl := range all {
		name := pl.Name
		if name == "" {
			name = "Anonymous"
		}
		rows = append(rows, boardRow{Name: name, Score: scoreAnswers(pl.Answers, all).Total, Me: pl.PlayerID == viewer})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Score > rows[j].Score })
	return rows, len(all)
}

func (s *server) feedHandler(w http.ResponseWriter, r *http.Request) {
	viewer := ""
	if p, err := s.store.playerByToken(r.Header.Get("X-Player-Token")); err == nil {
		viewer = p.ID
	}
	base := `t.public='approved' AND t.hidden=0 AND t.status='done'`
	sortBy := r.URL.Query().Get("sort")
	var items []*Trailer
	var err error
	switch sortBy {
	case "new":
		items, err = s.store.feed(viewer, base+` ORDER BY t.published DESC LIMIT 40`)
	case "week":
		items, err = s.store.feed(viewer, base+` AND t.published>=? LIMIT 400`, time.Now().AddDate(0, 0, -7).Unix())
	default:
		sortBy = "today"
		items, err = s.store.feed(viewer, base+` AND t.day=? LIMIT 400`, s.dayNo())
	}
	if err != nil {
		log.Printf("feed: %v", err)
		writeErr(w, 500, "Couldn't load the feed.")
		return
	}
	if sortBy != "new" { // best first: net likes, then newest
		sort.SliceStable(items, func(i, j int) bool {
			a, b := items[i].Likes-items[i].Dislikes, items[j].Likes-items[j].Dislikes
			if a != b {
				return a > b
			}
			return items[i].Published > items[j].Published
		})
		if len(items) > 40 {
			items = items[:40]
		}
	}
	for _, t := range items {
		s.addTemplate(t)
	}
	// Trailer of the Day: the most recent pick you featured in the last two days.
	var featured *Trailer
	if f, _ := s.store.feed(viewer, base+` AND t.featured>=? ORDER BY t.featured DESC LIMIT 1`, time.Now().AddDate(0, 0, -2).Unix()); len(f) == 1 {
		featured = f[0]
		s.addTemplate(featured)
	}
	board, players := s.originalBoard(viewer)
	if len(board) > 10 {
		mine := -1
		for i, b := range board {
			if b.Me {
				mine = i
			}
		}
		top := board[:10]
		if mine >= 10 {
			top = append(top, board[mine])
		}
		board = top
	}
	writeJSON(w, 200, map[string]any{"sort": sortBy, "items": items, "featured": featured, "me": viewer,
		"original": board, "players": players, "storyTitle": s.stories[s.storyFor(s.dayNo())].Title})
}

func (s *server) addTemplate(t *Trailer) {
	if t.Story < len(s.stories) {
		for _, sc := range s.stories[t.Story].Scenes {
			t.Template = append(t.Template, sc.Line)
		}
	}
}

// ownTrailer loads a trailer the caller made, or writes an error.
func (s *server) ownTrailer(w http.ResponseWriter, r *http.Request, p *Player) *Trailer {
	id := r.PathValue("id")
	t, err := s.store.trailer(id)
	if !idRe.MatchString(id) || err != nil || t.PlayerID != p.ID {
		writeErr(w, 404, "That's not one of your trailers.")
		return nil
	}
	return t
}

func (s *server) publish(w http.ResponseWriter, r *http.Request) {
	p := s.player(w, r)
	if p == nil || s.needName(w, p) {
		return
	}
	t := s.ownTrailer(w, r, p)
	if t == nil {
		return
	}
	switch {
	case t.Status != "done":
		writeErr(w, 400, "Wait for it to finish rendering.")
		return
	case t.Hidden:
		writeErr(w, 400, "This trailer was taken down, so it can't go on the feed.")
		return
	case t.Public == "rejected":
		writeErr(w, 400, "This one wasn't approved for the feed. You can still share the link with friends.")
		return
	case t.Public == "pending" || t.Public == "approved":
		writeJSON(w, 200, map[string]string{"public": t.Public})
		return
	}
	flag := s.flagged(t.Answers)
	state := "pending"
	if !s.review && !flag {
		state = "approved"
	}
	if err := s.store.setPublic(t.ID, state, now(), flag); err != nil {
		writeErr(w, 500, "Couldn't publish it.")
		return
	}
	writeJSON(w, 200, map[string]string{"public": state})
}

func (s *server) unpublish(w http.ResponseWriter, r *http.Request) {
	p := s.player(w, r)
	if p == nil {
		return
	}
	t := s.ownTrailer(w, r, p)
	if t == nil {
		return
	}
	if t.Public == "rejected" {
		writeJSON(w, 200, map[string]string{"public": "rejected"})
		return
	}
	s.store.setPublic(t.ID, "", 0, t.Flagged)
	writeJSON(w, 200, map[string]string{"public": ""})
}

// feedTrailer loads a trailer that's live on the feed, or writes an error.
func (s *server) feedTrailer(w http.ResponseWriter, r *http.Request) *Trailer {
	id := r.PathValue("id")
	t, err := s.store.trailer(id)
	if !idRe.MatchString(id) || err != nil || t.Public != "approved" || t.Hidden {
		writeErr(w, 404, "That trailer isn't on the feed.")
		return nil
	}
	return t
}

func (s *server) react(w http.ResponseWriter, r *http.Request) {
	p := s.player(w, r)
	if p == nil {
		return
	}
	t := s.feedTrailer(w, r)
	if t == nil {
		return
	}
	var in struct {
		Value int `json:"value"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.Value < -1 || in.Value > 1 {
		writeErr(w, 400, "A reaction is like, dislike, or nothing.")
		return
	}
	if t.PlayerID == p.ID {
		writeErr(w, 400, "You can't rate your own trailer.")
		return
	}
	if err := s.store.react(t.ID, p.ID, in.Value); err != nil {
		writeErr(w, 500, "Couldn't save that.")
		return
	}
	items, _ := s.store.feed(p.ID, `t.id=?`, t.ID)
	if len(items) == 1 {
		writeJSON(w, 200, map[string]int{"likes": items[0].Likes, "dislikes": items[0].Dislikes, "myReaction": items[0].Mine})
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *server) reportTrailer(w http.ResponseWriter, r *http.Request) {
	p := s.player(w, r)
	if p == nil {
		return
	}
	t := s.feedTrailer(w, r)
	if t == nil {
		return
	}
	if t.PlayerID == p.ID {
		writeErr(w, 400, "That's your own trailer. Use Remove from feed instead.")
		return
	}
	n, err := s.store.report(t.ID, p.ID)
	if err != nil {
		writeErr(w, 500, "Couldn't send the report.")
		return
	}
	if n >= reportsToHide {
		s.store.setHidden(t.ID, true)
		log.Printf("trailer %s auto-hidden after %d reports", t.ID, n)
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
