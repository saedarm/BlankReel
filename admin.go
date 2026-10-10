package main

import (
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"time"
)

// The admin page is plain HTML forms, locked by BLANKREEL_ADMIN_KEY.
// Open it at /admin?key=<your key>.

const costPerTrailer = 0.21

var adminTmpl = template.Must(template.New("admin").Funcs(template.FuncMap{
	"ago": func(unix int64) string {
		d := time.Since(time.Unix(unix, 0)).Round(time.Minute)
		switch {
		case d < time.Minute:
			return "just now"
		case d < time.Hour:
			return d.String()[:len(d.String())-2] + " ago"
		default:
			return time.Unix(unix, 0).Format("Jan 2 3:04pm")
		}
	},
	"date": func(t time.Time) string { return t.Format("Jan 2") },
}).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Blank Reel admin</title>
<style>
:root{--bg:#eceef6;--paper:#fff;--ink:#151a3e;--muted:#5b6188;--line:#d4d8ea;--cobalt:#2b3fd1;--bad:#c2412d;--ok:#1f8a5b}
@media (prefers-color-scheme:dark){:root{--bg:#0c0f24;--paper:#151938;--ink:#eef0ff;--muted:#9ea5cc;--line:#2a2f57;--cobalt:#8f9dff;--bad:#ff8a70;--ok:#4cc58f;color-scheme:dark}}
body{margin:0;background:var(--bg);color:var(--ink);font:15px/1.5 system-ui,sans-serif}
main{max-width:980px;margin:0 auto;padding:24px 16px 64px;display:flex;flex-direction:column;gap:20px}
h1{margin:0;font-size:1.5rem}h2{margin:0 0 10px;font-size:1.1rem}
section{background:var(--paper);border:1px solid var(--line);border-radius:12px;padding:16px;overflow-x:auto}
.stats{display:flex;gap:24px;flex-wrap:wrap}.stats b{display:block;font-size:1.6rem;font-variant-numeric:tabular-nums}
.stats span{color:var(--muted);font-size:.85rem}
table{border-collapse:collapse;width:100%;font-size:.9rem}th,td{text-align:left;padding:6px 8px;border-top:1px solid var(--line);vertical-align:top}
th{color:var(--muted);font-weight:500;border-top:0}
code{font-size:.95rem}
form{display:inline}
input[type=text]{font:inherit;padding:6px 8px;border:1px solid var(--line);border-radius:6px;background:var(--bg);color:var(--ink)}
button{font:inherit;padding:5px 10px;border-radius:6px;border:1px solid var(--line);background:var(--bg);color:var(--ink);cursor:pointer}
button.primary{background:var(--cobalt);color:#fff;border:0}
.muted{color:var(--muted)}.bad{color:var(--bad)}.ok{color:var(--ok)}
a{color:var(--cobalt)}
</style></head><body><main>
<h1>Blank Reel admin</h1>
{{if .Flash}}<p class="ok">{{.Flash}}</p>{{end}}
<section><h2>Today</h2><div class="stats">
<div><b>{{.Today}}</b><span>trailers today (limit {{.Limit}})</span></div>
<div><b>{{.Spend}}</b><span>est. AI spend today</span></div>
<div><b>{{.Month}}</b><span>trailers in the last 30 days</span></div>
<div><b>{{.MonthSpend}}</b><span>est. AI spend, 30 days</span></div>
</div>{{if .Mock}}<p class="muted">Test mode: no Gemini key, so nothing is actually spent.</p>{{end}}</section>

<section><h2>Invite codes</h2>
<p class="muted">Anyone who enters an active code becomes a beta tester: no paywall, {{.PlayerLimit}} trailers a day.{{if .Master}} The BLANKREEL_CODE secret also works as a code.{{end}}</p>
<form method="post" action="/admin/invite"><input type="hidden" name="key" value="{{.Key}}">
<input type="text" name="code" placeholder="Code (blank = random)" maxlength="24">
<input type="text" name="note" placeholder="Who it's for" maxlength="60">
<button class="primary">Create code</button></form>
<table><tr><th>Code</th><th>For</th><th>Uses</th><th>Created</th><th></th></tr>
{{range .Invites}}<tr><td><code>{{.Code}}</code></td><td>{{.Note}}</td><td>{{.Uses}}</td><td>{{date .Created}}</td>
<td><form method="post" action="/admin/invite-toggle"><input type="hidden" name="key" value="{{$.Key}}"><input type="hidden" name="code" value="{{.Code}}">
{{if .Active}}<button>Turn off</button>{{else}}<span class="bad">off</span> <button>Turn on</button>{{end}}</form></td></tr>
{{else}}<tr><td colspan="5" class="muted">No codes yet.</td></tr>{{end}}</table></section>

<section><h2>Review queue{{if .Pending}} ({{len .Pending}}){{end}}</h2>
<p class="muted">{{if .Review}}Everything published waits here until you approve it.{{else}}Review is off (BLANKREEL_REVIEW=off), so only answers that hit the word filter wait here.{{end}}</p>
<table><tr><th>Published</th><th>Who</th><th>Story</th><th>Answers</th><th></th></tr>
{{range .Pending}}<tr><td>{{ago .Published}}</td><td>{{.Player}}</td><td><a href="/t/{{.ID}}" target="_blank" rel="noopener">#{{.Day}} {{.Title}}</a>{{if .Flagged}} <span class="bad">word filter</span>{{end}}</td>
<td class="muted">{{range $i, $a := .Answers}}{{if $i}}, {{end}}{{$a}}{{end}}</td>
<td style="white-space:nowrap"><form method="post" action="/admin/approve"><input type="hidden" name="key" value="{{$.Key}}"><input type="hidden" name="id" value="{{.ID}}"><button class="primary">Approve</button></form>
<form method="post" action="/admin/reject"><input type="hidden" name="key" value="{{$.Key}}"><input type="hidden" name="id" value="{{.ID}}"><button>Reject</button></form></td></tr>
{{else}}<tr><td colspan="5" class="muted">Nothing waiting.</td></tr>{{end}}</table></section>

<section><h2>On the feed</h2>
<p class="muted">Feature one a day as the Trailer of the Day; it stays pinned at the top of the feed for two days. Three reports from players hide a trailer automatically.</p>
<table><tr><th>Published</th><th>Who</th><th>Story</th><th>👍</th><th>👎</th><th>Reports</th><th></th></tr>
{{range .Live}}<tr><td>{{ago .Published}}</td><td>{{.Player}}</td><td><a href="/t/{{.ID}}" target="_blank" rel="noopener">#{{.Day}} {{.Title}}</a>{{if .Featured}} <span class="ok">featured</span>{{end}}{{if .Hidden}} <span class="bad">hidden</span>{{end}}</td>
<td>{{.Likes}}</td><td>{{.Dislikes}}</td><td>{{if .Reports}}<span class="bad">{{.Reports}}</span>{{else}}0{{end}}</td>
<td style="white-space:nowrap"><form method="post" action="/admin/feature"><input type="hidden" name="key" value="{{$.Key}}"><input type="hidden" name="id" value="{{.ID}}">
{{if .Featured}}<input type="hidden" name="on" value="0"><button>Unfeature</button>{{else}}<input type="hidden" name="on" value="1"><button>Feature</button>{{end}}</form>
<form method="post" action="/admin/hide"><input type="hidden" name="key" value="{{$.Key}}"><input type="hidden" name="id" value="{{.ID}}">
{{if .Hidden}}<input type="hidden" name="hide" value="0"><button>Unhide</button>{{else}}<input type="hidden" name="hide" value="1"><button>Hide</button>{{end}}</form></td></tr>
{{else}}<tr><td colspan="7" class="muted">Nothing published yet.</td></tr>{{end}}</table></section>

<section><h2>Recent trailers</h2>
<table><tr><th>When</th><th>Who</th><th>Story</th><th>Answers</th><th>Status</th><th></th></tr>
{{range .Trailers}}<tr><td>{{ago .Created}}</td><td>{{if .Player}}{{.Player}}{{else}}<span class="muted">no name</span>{{end}}</td>
<td>#{{.Day}} {{.Title}}</td><td class="muted">{{range $i, $a := .Answers}}{{if $i}}, {{end}}{{$a}}{{end}}</td>
<td>{{if eq .Status "done"}}<a href="/t/{{.ID}}" target="_blank" rel="noopener">done</a>{{else if eq .Status "failed"}}<span class="bad" title="{{.Error}}">failed</span>{{else}}{{.Status}}{{end}}</td>
<td><form method="post" action="/admin/hide"><input type="hidden" name="key" value="{{$.Key}}"><input type="hidden" name="id" value="{{.ID}}">
{{if .Hidden}}<input type="hidden" name="hide" value="0"><span class="bad">hidden</span> <button>Unhide</button>{{else}}<input type="hidden" name="hide" value="1"><button>Hide</button>{{end}}</form></td></tr>
{{else}}<tr><td colspan="6" class="muted">Nothing yet.</td></tr>{{end}}</table></section>

<section><h2>Rooms</h2>
<table><tr><th>Name</th><th>Join code</th><th>Members</th></tr>
{{range .Rooms}}<tr><td>{{.Name}}</td><td><code>{{.JoinCode}}</code></td><td>{{.Members}}</td></tr>
{{else}}<tr><td colspan="3" class="muted">No rooms yet.</td></tr>{{end}}</table></section>
</main></body></html>`))

func (s *server) adminOK(r *http.Request) bool {
	return sameSecret(r.FormValue("key"), s.adminKey)
}

func (s *server) adminPage(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		http.NotFound(w, r)
		return
	}
	today, _ := s.store.countSince("", midnight(time.Now()))
	month, _ := s.store.countSince("", time.Now().AddDate(0, 0, -30))
	invites, _ := s.store.invites()
	trailers, _ := s.store.recentTrailers(60)
	rooms, _ := s.store.rooms()
	pending, _ := s.store.feed("", `t.public='pending' AND t.hidden=0 ORDER BY t.published LIMIT 100`)
	live, _ := s.store.feed("", `t.public='approved' ORDER BY t.published DESC LIMIT 40`)
	rate := costPerTrailer
	if s.gemini == nil {
		rate = 0
	}
	money := func(n int) string { return fmt.Sprintf("$%.2f", float64(n)*rate) }
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	err := adminTmpl.Execute(w, map[string]any{
		"Key": r.FormValue("key"), "Flash": r.FormValue("flash"), "Today": today, "Limit": s.dailyLimit,
		"Spend": money(today), "Month": month, "MonthSpend": money(month), "Mock": s.gemini == nil,
		"Invites": invites, "Trailers": trailers, "Rooms": rooms, "PlayerLimit": s.playerLimit, "Master": s.masterCode != "",
		"Pending": pending, "Live": live, "Review": s.review,
	})
	if err != nil {
		log.Printf("admin page: %v", err)
	}
}

func (s *server) adminAction(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		http.NotFound(w, r)
		return
	}
	flash := ""
	switch r.PathValue("action") {
	case "invite":
		code := normCode(cleanText(r.FormValue("code"), 24))
		if code == "" {
			code = newJoinCode()
		}
		if err := s.store.createInvite(code, cleanText(r.FormValue("note"), 60)); err != nil {
			flash = "Couldn't create " + code + " (already exists?)"
		} else {
			flash = "Created code " + code
		}
	case "invite-toggle":
		code := normCode(r.FormValue("code"))
		ivs, _ := s.store.invites()
		for _, iv := range ivs {
			if iv.Code == code {
				s.store.setInviteActive(code, !iv.Active)
				flash = "Updated " + code
			}
		}
	case "hide":
		id := r.FormValue("id")
		if idRe.MatchString(id) {
			s.store.setHidden(id, r.FormValue("hide") == "1")
			flash = "Updated trailer " + id
		}
	case "approve", "reject":
		id := r.FormValue("id")
		if t, err := s.store.trailer(id); err == nil && idRe.MatchString(id) {
			state := map[string]string{"approve": "approved", "reject": "rejected"}[r.PathValue("action")]
			s.store.setPublic(id, state, t.Published, t.Flagged)
			flash = "Trailer " + id + " " + state
		}
	case "feature":
		id := r.FormValue("id")
		if idRe.MatchString(id) {
			s.store.setFeatured(id, r.FormValue("on") == "1")
			flash = "Updated trailer " + id
		}
	default:
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/admin?key="+url.QueryEscape(r.FormValue("key"))+"&flash="+url.QueryEscape(flash), http.StatusSeeOther)
}
