package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// player reads the X-Player-Token header. Missing or unknown → 401.
func (s *server) player(w http.ResponseWriter, r *http.Request) *Player {
	p, err := s.store.playerByToken(r.Header.Get("X-Player-Token"))
	if err != nil {
		if !errors.Is(err, errNotFound) {
			log.Printf("player lookup: %v", err)
		}
		writeErr(w, 401, "We don't know who you are yet. Reload the page and try again.")
		return nil
	}
	return p
}

// codeRequired is true when making trailers needs an invite code.
func (s *server) codeRequired() bool {
	if s.masterCode != "" {
		return true
	}
	ivs, _ := s.store.invites()
	return len(ivs) > 0
}

func (s *server) today(w http.ResponseWriter, r *http.Request) {
	day := s.dayNo()
	idx := s.storyFor(day)
	practice := false
	if p := r.URL.Query().Get("story"); p != "" {
		// Bonus rounds come from the backup stories in stories.json.
		if n, err := strconv.Atoi(p); err == nil && n >= 0 && n < len(s.stories) && n != idx {
			idx, practice = n, true
		}
	}
	writeJSON(w, 200, map[string]any{
		"day": day, "storyIndex": idx, "storyCount": len(s.stories), "practice": practice,
		"story": s.mustStory(idx), "needsCode": s.codeRequired(), "mock": s.gemini == nil,
	})
}

func (s *server) newPlayer(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.newPlayer()
	if err != nil {
		log.Printf("new player: %v", err)
		writeErr(w, 500, "Couldn't set you up. Try again.")
		return
	}
	writeJSON(w, 201, map[string]any{"token": p.Token, "player": p})
}

func (s *server) me(w http.ResponseWriter, r *http.Request) {
	p := s.player(w, r)
	if p == nil {
		return
	}
	rooms, _ := s.store.myRooms(p.ID)
	used, _ := s.store.countSince(p.ID, midnight(time.Now()))
	out := map[string]any{"player": p, "rooms": rooms, "usedToday": used, "playerLimit": s.playerLimit,
		"canMake": p.Beta || !s.codeRequired()}
	if t, err := s.store.latestDaily(p.ID, s.dayNo()); err == nil {
		out["todayTrailer"] = t.ID
	}
	writeJSON(w, 200, out)
}

func (s *server) setName(w http.ResponseWriter, r *http.Request) {
	p := s.player(w, r)
	if p == nil {
		return
	}
	var in struct{ Name string }
	if !readJSON(w, r, &in) {
		return
	}
	name := cleanText(in.Name, 24)
	if name == "" {
		writeErr(w, 400, "Pick a name with at least one letter.")
		return
	}
	if err := s.store.setName(p.ID, name); err != nil {
		writeErr(w, 500, "Couldn't save your name.")
		return
	}
	p.Name = name
	writeJSON(w, 200, map[string]any{"player": p})
}

func (s *server) redeem(w http.ResponseWriter, r *http.Request) {
	p := s.player(w, r)
	if p == nil {
		return
	}
	var in struct{ Code string }
	if !readJSON(w, r, &in) {
		return
	}
	if !s.redeemCode(p, in.Code) {
		writeErr(w, 403, "That invite code doesn't work. Codes aren't case-sensitive; check for typos.")
		return
	}
	writeJSON(w, 200, map[string]any{"player": p})
}

func (s *server) redeemCode(p *Player, code string) bool {
	if p.Beta {
		return true
	}
	ok := sameSecret(normCode(code), s.masterCode)
	if !ok && normCode(code) != "" {
		ok, _ = s.store.redeem(code)
	}
	if ok {
		s.store.setBeta(p.ID)
		p.Beta = true
	}
	return ok
}

func (s *server) create(w http.ResponseWriter, r *http.Request) {
	p := s.player(w, r)
	if p == nil {
		return
	}
	var in struct {
		Story   int      `json:"story"`
		Answers []string `json:"answers"`
		Code    string   `json:"code"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if s.codeRequired() && !s.redeemCode(p, in.Code) {
		writeErr(w, 401, "Enter an invite code to make trailers. Ask whoever runs Blank Reel for one.")
		return
	}
	day := s.dayNo()
	st, ok := s.storyByID(in.Story)
	if !ok || (in.Story >= dailyBase && in.Story != s.storyFor(day)) {
		writeErr(w, 400, "That story isn't playable anymore. Reload to get today's story.")
		return
	}
	if len(in.Answers) != len(st.Blanks) {
		writeErr(w, 400, fmt.Sprintf("Fill all %d blanks first.", len(st.Blanks)))
		return
	}
	for i, a := range in.Answers {
		a = cleanText(a, 40)
		if a == "" {
			writeErr(w, 400, fmt.Sprintf("Blank %d is empty.", i+1))
			return
		}
		in.Answers[i] = a
	}
	since := midnight(time.Now())
	if n, _ := s.store.countSince(p.ID, since); n >= s.playerLimit {
		writeErr(w, 429, fmt.Sprintf("You've made %d trailers today, which is the daily max. More tomorrow.", n))
		return
	}
	if n, _ := s.store.countSince("", since); n >= s.dailyLimit {
		writeErr(w, 429, "Blank Reel hit today's trailer limit for everyone. Try again tomorrow.")
		return
	}
	t := &Trailer{ID: newID(), PlayerID: p.ID, Day: day, Story: in.Story, Daily: in.Story == s.storyFor(day),
		Title: st.Title, Answers: in.Answers, Status: "queued", Created: time.Now().Unix()}
	if err := s.store.insertTrailer(t); err != nil {
		log.Printf("insert trailer: %v", err)
		writeErr(w, 500, "Couldn't start the trailer. Try again.")
		return
	}
	s.mu.Lock()
	s.waiting = append(s.waiting, t.ID)
	s.mu.Unlock()
	select {
	case s.queue <- t:
	default:
		s.store.setTrailerStatus(t.ID, "failed", "The render line is full. Try again in a few minutes.")
	}
	writeJSON(w, 202, map[string]string{"id": t.ID})
}

func (s *server) status(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idRe.MatchString(id) {
		writeErr(w, 404, "No trailer with that ID.")
		return
	}
	t, err := s.store.trailer(id)
	if err != nil {
		writeErr(w, 404, "No trailer with that ID.")
		return
	}
	out := map[string]any{"id": t.ID, "status": t.Status, "error": t.Error, "title": t.Title, "day": t.Day, "daily": t.Daily,
		"public": t.Public, "hidden": t.Hidden}
	s.mu.Lock()
	if j := s.jobs[id]; j != nil {
		out["step"], out["done"], out["total"] = j.Step, j.Done, j.Total
	}
	for i, q := range s.waiting {
		if q == id {
			out["position"] = i + 1
		}
	}
	s.mu.Unlock()
	writeJSON(w, 200, out)
}

// ---- rooms ----

func (s *server) listRooms(w http.ResponseWriter, r *http.Request) {
	p := s.player(w, r)
	if p == nil {
		return
	}
	rooms, err := s.store.myRooms(p.ID)
	if err != nil {
		writeErr(w, 500, "Couldn't load your rooms.")
		return
	}
	writeJSON(w, 200, map[string]any{"rooms": rooms})
}

func (s *server) needName(w http.ResponseWriter, p *Player) bool {
	if p.Name == "" {
		writeErr(w, 400, "Pick a name first so your friends know who you are.")
		return true
	}
	return false
}

func (s *server) createRoom(w http.ResponseWriter, r *http.Request) {
	p := s.player(w, r)
	if p == nil || s.needName(w, p) {
		return
	}
	var in struct{ Name string }
	if !readJSON(w, r, &in) {
		return
	}
	name := cleanText(in.Name, 32)
	if name == "" {
		writeErr(w, 400, "Give the room a name.")
		return
	}
	room, err := s.store.createRoom(name, p.ID)
	if err != nil {
		log.Printf("create room: %v", err)
		writeErr(w, 500, "Couldn't create the room.")
		return
	}
	writeJSON(w, 201, map[string]any{"room": room})
}

func (s *server) joinRoom(w http.ResponseWriter, r *http.Request) {
	p := s.player(w, r)
	if p == nil || s.needName(w, p) {
		return
	}
	var in struct{ Code string }
	if !readJSON(w, r, &in) {
		return
	}
	room, err := s.store.roomByCode(in.Code)
	if err != nil {
		writeErr(w, 404, "No room with that code. Check the link your friend sent.")
		return
	}
	if err := s.store.join(room.ID, p.ID); err != nil {
		writeErr(w, 500, "Couldn't join the room.")
		return
	}
	writeJSON(w, 200, map[string]any{"room": room})
}

func (s *server) leaveRoom(w http.ResponseWriter, r *http.Request) {
	p := s.player(w, r)
	if p == nil {
		return
	}
	s.store.leave(r.PathValue("id"), p.ID)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *server) roomWall(w http.ResponseWriter, r *http.Request) {
	p := s.player(w, r)
	if p == nil {
		return
	}
	room, err := s.store.room(r.PathValue("id"))
	if err != nil || !s.store.isMember(room.ID, p.ID) {
		writeErr(w, 404, "That room doesn't exist, or you're not in it. Ask for the invite link.")
		return
	}
	today := s.dayNo()
	day := today
	if d, err := strconv.Atoi(r.URL.Query().Get("day")); err == nil && d >= 1 && d <= today {
		day = d
	}
	members, _ := s.store.members(room.ID)
	entries, err := s.store.wall(room.ID, day)
	if err != nil {
		log.Printf("wall: %v", err)
		writeErr(w, 500, "Couldn't load the room.")
		return
	}
	iPlayed := false
	played := map[string]string{}
	for _, t := range entries {
		played[t.PlayerID] = t.Status
		if t.PlayerID == p.ID {
			iPlayed = true
		}
	}
	type memberOut struct {
		Member
		Status string `json:"status"` // "", queued, rendering, done
	}
	mout := make([]memberOut, 0, len(members))
	for _, m := range members {
		mout = append(mout, memberOut{m, played[m.ID]})
	}
	// Today's entries stay hidden until you've played, so nobody spoils the script.
	locked := day == today && !iPlayed
	if locked {
		entries = []*Trailer{}
	}
	from, to := s.monthDays(day)
	board, _ := s.store.leaderboard(room.ID, from, to)
	si := s.storyFor(day)
	writeJSON(w, 200, map[string]any{
		"room": room, "day": day, "today": today, "date": s.dateOf(day).Format("Mon Jan 2"),
		"month": s.dateOf(day).Format("January"), "storyIndex": si, "story": s.mustStory(si),
		"members": mout, "entries": entries, "locked": locked, "myVote": s.store.myVote(room.ID, day, p.ID),
		"me": p.ID, "leaderboard": board,
	})
}

func (s *server) vote(w http.ResponseWriter, r *http.Request) {
	p := s.player(w, r)
	if p == nil {
		return
	}
	roomID := r.PathValue("id")
	if !s.store.isMember(roomID, p.ID) {
		writeErr(w, 404, "You're not in that room.")
		return
	}
	var in struct {
		Day     int    `json:"day"`
		Trailer string `json:"trailer"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.Trailer != "" {
		entries, _ := s.store.wall(roomID, in.Day)
		var target *Trailer
		mine := false
		for _, t := range entries {
			if t.ID == in.Trailer {
				target = t
			}
			if t.PlayerID == p.ID {
				mine = true
			}
		}
		switch {
		case target == nil:
			writeErr(w, 400, "That trailer isn't on this room's wall.")
			return
		case target.PlayerID == p.ID:
			writeErr(w, 400, "No voting for yourself.")
			return
		case target.Status != "done":
			writeErr(w, 400, "Wait until it finishes rendering.")
			return
		case !mine && in.Day == s.dayNo():
			writeErr(w, 400, "Play today's story before you vote.")
			return
		}
	}
	if err := s.store.vote(roomID, in.Day, p.ID, in.Trailer); err != nil {
		writeErr(w, 500, "Couldn't save your vote.")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---- media and share pages ----

func (s *server) media(w http.ResponseWriter, r *http.Request) {
	file := r.PathValue("file")
	id, ext, _ := strings.Cut(file, ".")
	name := map[string]string{"mp4": "trailer.mp4", "jpg": "poster.jpg"}[ext]
	if !idRe.MatchString(id) || name == "" {
		http.NotFound(w, r)
		return
	}
	if t, err := s.store.trailer(id); err == nil && t.Hidden {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFile(w, r, filepath.Join(s.dataDir, id, name))
}

// legacy reads a trailer made before the database existed (data/<id>/meta.json).
func (s *server) legacy(id string) *Trailer {
	b, err := os.ReadFile(filepath.Join(s.dataDir, id, "meta.json"))
	if err != nil {
		return nil
	}
	var m struct {
		Day     int      `json:"day"`
		Story   int      `json:"story"`
		Title   string   `json:"title"`
		Answers []string `json:"answers"`
		Status  string   `json:"status"`
	}
	if json.Unmarshal(b, &m) != nil || m.Status != "done" {
		return nil
	}
	return &Trailer{ID: id, Day: m.Day, Story: m.Story, Title: m.Title, Answers: m.Answers, Status: "done"}
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
<p>Blank Reel #{{.Day}}{{if .By}} · by {{.By}}{{end}}</p><h1>{{.Title}}</h1>
<video src="/v/{{.ID}}.mp4" poster="/v/{{.ID}}.jpg" controls playsinline></video>
<a href="/">Fill in today's blanks</a>
</main></body></html>`))

func (s *server) sharePage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idRe.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	t, err := s.store.trailer(id)
	if err != nil {
		t = s.legacy(id)
	}
	st, ok := Story{}, false
	if t != nil {
		st, ok = s.storyByID(t.Story)
	}
	if t == nil || t.Status != "done" || t.Hidden || !ok {
		http.NotFound(w, r)
		return
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	scenes := st.Scenes
	shareTmpl.Execute(w, map[string]any{
		"ID": t.ID, "Title": t.Title, "Day": t.Day, "By": t.Player, "Base": scheme + "://" + r.Host,
		"Last": fillPlain(scenes[len(scenes)-1].Line, t.Answers),
	})
}
