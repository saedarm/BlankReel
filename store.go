package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store is a single SQLite file on the data volume. One writer at a time is
// plenty for this game, so the pool is pinned to one connection.
type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS players (
  id TEXT PRIMARY KEY,
  token TEXT UNIQUE NOT NULL,
  name TEXT NOT NULL DEFAULT '',
  beta INTEGER NOT NULL DEFAULT 0,
  created INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS invite_codes (
  code TEXT PRIMARY KEY,
  note TEXT NOT NULL DEFAULT '',
  uses INTEGER NOT NULL DEFAULT 0,
  active INTEGER NOT NULL DEFAULT 1,
  created INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS rooms (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  join_code TEXT UNIQUE NOT NULL,
  owner TEXT NOT NULL,
  created INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS room_members (
  room_id TEXT NOT NULL,
  player_id TEXT NOT NULL,
  joined INTEGER NOT NULL,
  PRIMARY KEY (room_id, player_id)
);
CREATE TABLE IF NOT EXISTS trailers (
  id TEXT PRIMARY KEY,
  player_id TEXT NOT NULL,
  day INTEGER NOT NULL,
  story INTEGER NOT NULL,
  daily INTEGER NOT NULL DEFAULT 0,
  title TEXT NOT NULL,
  answers TEXT NOT NULL,
  status TEXT NOT NULL,
  error TEXT NOT NULL DEFAULT '',
  hidden INTEGER NOT NULL DEFAULT 0,
  created INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS trailers_day ON trailers(day, daily);
CREATE INDEX IF NOT EXISTS trailers_player ON trailers(player_id, created);
CREATE TABLE IF NOT EXISTS reactions (
  trailer_id TEXT NOT NULL,
  player_id TEXT NOT NULL,
  value INTEGER NOT NULL,
  created INTEGER NOT NULL,
  PRIMARY KEY (trailer_id, player_id)
);
CREATE TABLE IF NOT EXISTS reports (
  trailer_id TEXT NOT NULL,
  player_id TEXT NOT NULL,
  created INTEGER NOT NULL,
  PRIMARY KEY (trailer_id, player_id)
);
CREATE TABLE IF NOT EXISTS plays (
  player_id TEXT NOT NULL,
  day INTEGER NOT NULL,
  story INTEGER NOT NULL,
  answers TEXT NOT NULL,
  created INTEGER NOT NULL,
  PRIMARY KEY (player_id, day, story)
);
CREATE TABLE IF NOT EXISTS votes (
  room_id TEXT NOT NULL,
  day INTEGER NOT NULL,
  voter_id TEXT NOT NULL,
  trailer_id TEXT NOT NULL,
  PRIMARY KEY (room_id, day, voter_id)
);
`

// migrations add columns to databases created by earlier versions.
var migrations = []string{
	`ALTER TABLE trailers ADD COLUMN public TEXT NOT NULL DEFAULT ''`, // '', pending, approved, rejected
	`ALTER TABLE trailers ADD COLUMN published INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE trailers ADD COLUMN featured INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE trailers ADD COLUMN flagged INTEGER NOT NULL DEFAULT 0`,
	`CREATE INDEX IF NOT EXISTS trailers_public ON trailers(public, published)`,
}

func openStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	for _, m := range migrations {
		if _, err := db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return nil, err
		}
	}
	// Anything mid-render when the server stopped will never finish.
	db.Exec(`UPDATE trailers SET status='failed', error='The server restarted during the render. Make it again; it did not count against your limit.' WHERE status IN ('queued','rendering')`)
	return &Store{db: db}, nil
}

var errNotFound = errors.New("not found")

func now() int64 { return time.Now().Unix() }

// ---- players ----

type Player struct {
	ID    string `json:"id"`
	Token string `json:"-"`
	Name  string `json:"name"`
	Beta  bool   `json:"beta"`
}

func (s *Store) newPlayer() (*Player, error) {
	p := &Player{ID: newID(), Token: newToken()}
	_, err := s.db.Exec(`INSERT INTO players(id, token, created) VALUES(?,?,?)`, p.ID, p.Token, now())
	return p, err
}

func (s *Store) playerByToken(tok string) (*Player, error) {
	if tok == "" {
		return nil, errNotFound
	}
	p := &Player{}
	var beta int
	err := s.db.QueryRow(`SELECT id, token, name, beta FROM players WHERE token=?`, tok).Scan(&p.ID, &p.Token, &p.Name, &beta)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	p.Beta = beta == 1
	return p, err
}

func (s *Store) setName(id, name string) error {
	_, err := s.db.Exec(`UPDATE players SET name=? WHERE id=?`, name, id)
	return err
}

func (s *Store) setBeta(id string) error {
	_, err := s.db.Exec(`UPDATE players SET beta=1 WHERE id=?`, id)
	return err
}

// ---- invite codes ----

type Invite struct {
	Code    string
	Note    string
	Uses    int
	Active  bool
	Created time.Time
}

func normCode(c string) string { return strings.ToUpper(strings.TrimSpace(c)) }

func (s *Store) redeem(code string) (bool, error) {
	res, err := s.db.Exec(`UPDATE invite_codes SET uses=uses+1 WHERE code=? AND active=1`, normCode(code))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (s *Store) createInvite(code, note string) error {
	_, err := s.db.Exec(`INSERT INTO invite_codes(code, note, created) VALUES(?,?,?)`, normCode(code), note, now())
	return err
}

func (s *Store) setInviteActive(code string, on bool) error {
	_, err := s.db.Exec(`UPDATE invite_codes SET active=? WHERE code=?`, b2i(on), normCode(code))
	return err
}

func (s *Store) invites() ([]Invite, error) {
	rows, err := s.db.Query(`SELECT code, note, uses, active, created FROM invite_codes ORDER BY created DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Invite
	for rows.Next() {
		var iv Invite
		var a int
		var c int64
		rows.Scan(&iv.Code, &iv.Note, &iv.Uses, &a, &c)
		iv.Active, iv.Created = a == 1, time.Unix(c, 0)
		out = append(out, iv)
	}
	return out, rows.Err()
}

// ---- trailers ----

type Trailer struct {
	ID       string   `json:"id"`
	PlayerID string   `json:"playerId"`
	Player   string   `json:"player,omitempty"`
	Day      int      `json:"day"`
	Story    int      `json:"story"`
	Daily    bool     `json:"daily"`
	Title    string   `json:"title"`
	Answers  []string `json:"answers"`
	Status   string   `json:"status"`
	Error    string   `json:"error,omitempty"`
	Hidden   bool     `json:"hidden,omitempty"`
	Created  int64    `json:"created"`
	Votes    int      `json:"votes"`

	Public    string   `json:"public"`
	Published int64    `json:"published,omitempty"`
	Featured  int64    `json:"featured,omitempty"`
	Flagged   bool     `json:"flagged,omitempty"`
	Likes     int      `json:"likes"`
	Dislikes  int      `json:"dislikes"`
	Reports   int      `json:"reports,omitempty"`
	Mine      int      `json:"myReaction"`
	Template  []string `json:"template,omitempty"`
}

func (s *Store) insertTrailer(t *Trailer) error {
	a, _ := json.Marshal(t.Answers)
	_, err := s.db.Exec(`INSERT INTO trailers(id, player_id, day, story, daily, title, answers, status, created) VALUES(?,?,?,?,?,?,?,?,?)`,
		t.ID, t.PlayerID, t.Day, t.Story, b2i(t.Daily), t.Title, string(a), t.Status, t.Created)
	return err
}

func (s *Store) setTrailerStatus(id, status, msg string) error {
	_, err := s.db.Exec(`UPDATE trailers SET status=?, error=? WHERE id=?`, status, msg, id)
	return err
}

func (s *Store) setHidden(id string, h bool) error {
	_, err := s.db.Exec(`UPDATE trailers SET hidden=? WHERE id=?`, b2i(h), id)
	return err
}

const trailerCols = `t.id, t.player_id, COALESCE(p.name,''), t.day, t.story, t.daily, t.title, t.answers, t.status, t.error, t.hidden, t.created, t.public, t.published, t.featured, t.flagged`

func scanTrailer(sc interface{ Scan(...any) error }, extra ...any) (*Trailer, error) {
	t := &Trailer{}
	var daily, hidden, flagged int
	var answers string
	dest := []any{&t.ID, &t.PlayerID, &t.Player, &t.Day, &t.Story, &daily, &t.Title, &answers, &t.Status, &t.Error, &hidden, &t.Created,
		&t.Public, &t.Published, &t.Featured, &flagged}
	if err := sc.Scan(append(dest, extra...)...); err != nil {
		return nil, err
	}
	t.Daily, t.Hidden, t.Flagged = daily == 1, hidden == 1, flagged == 1
	json.Unmarshal([]byte(answers), &t.Answers)
	return t, nil
}

func (s *Store) trailer(id string) (*Trailer, error) {
	t, err := scanTrailer(s.db.QueryRow(`SELECT `+trailerCols+` FROM trailers t LEFT JOIN players p ON p.id=t.player_id WHERE t.id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	return t, err
}

// countSince counts trailers that cost (or will cost) money since a time. Failed ones don't count.
func (s *Store) countSince(playerID string, since time.Time) (int, error) {
	q := `SELECT COUNT(*) FROM trailers WHERE created>=? AND status!='failed'`
	args := []any{since.Unix()}
	if playerID != "" {
		q += ` AND player_id=?`
		args = append(args, playerID)
	}
	var n int
	err := s.db.QueryRow(q, args...).Scan(&n)
	return n, err
}

func (s *Store) recentTrailers(limit int) ([]*Trailer, error) {
	rows, err := s.db.Query(`SELECT `+trailerCols+` FROM trailers t LEFT JOIN players p ON p.id=t.player_id ORDER BY t.created DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Trailer
	for rows.Next() {
		t, err := scanTrailer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// latestDaily is the player's newest non-failed trailer for a day's daily story.
func (s *Store) latestDaily(playerID string, day int) (*Trailer, error) {
	t, err := scanTrailer(s.db.QueryRow(`SELECT `+trailerCols+` FROM trailers t LEFT JOIN players p ON p.id=t.player_id
		WHERE t.player_id=? AND t.day=? AND t.daily=1 AND t.status!='failed' ORDER BY t.created DESC LIMIT 1`, playerID, day))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	return t, err
}

// ---- rooms ----

type Room struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	JoinCode string `json:"joinCode"`
	Owner    string `json:"owner"`
	Members  int    `json:"members"`
}

func (s *Store) createRoom(name, owner string) (*Room, error) {
	for tries := 0; tries < 5; tries++ {
		r := &Room{ID: newID(), Name: name, JoinCode: newJoinCode(), Owner: owner}
		tx, err := s.db.Begin()
		if err != nil {
			return nil, err
		}
		_, err = tx.Exec(`INSERT INTO rooms(id, name, join_code, owner, created) VALUES(?,?,?,?,?)`, r.ID, r.Name, r.JoinCode, r.Owner, now())
		if err == nil {
			_, err = tx.Exec(`INSERT INTO room_members(room_id, player_id, joined) VALUES(?,?,?)`, r.ID, owner, now())
		}
		if err != nil {
			tx.Rollback()
			if strings.Contains(err.Error(), "UNIQUE") {
				continue // join code collision; try another
			}
			return nil, err
		}
		r.Members = 1
		return r, tx.Commit()
	}
	return nil, errors.New("couldn't pick a unique room code")
}

func (s *Store) roomByCode(code string) (*Room, error) {
	r := &Room{}
	err := s.db.QueryRow(`SELECT id, name, join_code, owner FROM rooms WHERE join_code=?`, normCode(code)).Scan(&r.ID, &r.Name, &r.JoinCode, &r.Owner)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	return r, err
}

func (s *Store) room(id string) (*Room, error) {
	r := &Room{}
	err := s.db.QueryRow(`SELECT id, name, join_code, owner FROM rooms WHERE id=?`, id).Scan(&r.ID, &r.Name, &r.JoinCode, &r.Owner)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	return r, err
}

func (s *Store) join(roomID, playerID string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO room_members(room_id, player_id, joined) VALUES(?,?,?)`, roomID, playerID, now())
	return err
}

func (s *Store) leave(roomID, playerID string) error {
	_, err := s.db.Exec(`DELETE FROM room_members WHERE room_id=? AND player_id=?`, roomID, playerID)
	return err
}

func (s *Store) isMember(roomID, playerID string) bool {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM room_members WHERE room_id=? AND player_id=?`, roomID, playerID).Scan(&n)
	return n == 1
}

func (s *Store) myRooms(playerID string) ([]Room, error) {
	rows, err := s.db.Query(`SELECT r.id, r.name, r.join_code, r.owner, (SELECT COUNT(*) FROM room_members m2 WHERE m2.room_id=r.id)
		FROM rooms r JOIN room_members m ON m.room_id=r.id WHERE m.player_id=? ORDER BY m.joined`, playerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Room{}
	for rows.Next() {
		var r Room
		rows.Scan(&r.ID, &r.Name, &r.JoinCode, &r.Owner, &r.Members)
		out = append(out, r)
	}
	return out, rows.Err()
}

type Member struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (s *Store) members(roomID string) ([]Member, error) {
	rows, err := s.db.Query(`SELECT p.id, p.name FROM room_members m JOIN players p ON p.id=m.player_id WHERE m.room_id=? ORDER BY m.joined`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var m Member
		rows.Scan(&m.ID, &m.Name)
		out = append(out, m)
	}
	return out, rows.Err()
}

// wall returns each member's latest finished daily trailer for a day, with this room's vote counts.
func (s *Store) wall(roomID string, day int) ([]*Trailer, error) {
	rows, err := s.db.Query(`SELECT `+trailerCols+`,
		(SELECT COUNT(*) FROM votes v WHERE v.room_id=? AND v.day=t.day AND v.trailer_id=t.id)
		FROM trailers t JOIN players p ON p.id=t.player_id
		JOIN room_members m ON m.player_id=t.player_id AND m.room_id=?
		WHERE t.day=? AND t.daily=1 AND t.hidden=0 AND t.status IN ('done','queued','rendering')
		AND t.created = (SELECT MAX(t2.created) FROM trailers t2 WHERE t2.player_id=t.player_id AND t2.day=t.day AND t2.daily=1 AND t2.hidden=0 AND t2.status IN ('done','queued','rendering'))
		ORDER BY t.created`, roomID, roomID, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Trailer
	for rows.Next() {
		var votes int
		t, err := scanTrailer(rows, &votes)
		if err != nil {
			return nil, err
		}
		t.Votes = votes
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) myVote(roomID string, day int, voter string) string {
	var id string
	s.db.QueryRow(`SELECT trailer_id FROM votes WHERE room_id=? AND day=? AND voter_id=?`, roomID, day, voter).Scan(&id)
	return id
}

func (s *Store) vote(roomID string, day int, voter, trailerID string) error {
	if trailerID == "" {
		_, err := s.db.Exec(`DELETE FROM votes WHERE room_id=? AND day=? AND voter_id=?`, roomID, day, voter)
		return err
	}
	_, err := s.db.Exec(`INSERT INTO votes(room_id, day, voter_id, trailer_id) VALUES(?,?,?,?)
		ON CONFLICT(room_id, day, voter_id) DO UPDATE SET trailer_id=excluded.trailer_id`, roomID, day, voter, trailerID)
	return err
}

type Score struct {
	PlayerID string `json:"playerId"`
	Name     string `json:"name"`
	Votes    int    `json:"votes"`
	Crowns   int    `json:"crowns"`
}

// leaderboard totals votes received and daily wins in a room across a range of days.
func (s *Store) leaderboard(roomID string, fromDay, toDay int) ([]Score, error) {
	rows, err := s.db.Query(`SELECT t.player_id, COALESCE(p.name,''), t.day, COUNT(*) FROM votes v
		JOIN trailers t ON t.id=v.trailer_id LEFT JOIN players p ON p.id=t.player_id
		WHERE v.room_id=? AND v.day BETWEEN ? AND ? AND t.hidden=0 GROUP BY t.player_id, t.day`, roomID, fromDay, toDay)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type dv struct {
		pid   string
		votes int
	}
	byDay := map[int][]dv{}
	tot := map[string]*Score{}
	for rows.Next() {
		var pid, name string
		var day, n int
		rows.Scan(&pid, &name, &day, &n)
		if tot[pid] == nil {
			tot[pid] = &Score{PlayerID: pid, Name: name}
		}
		tot[pid].Votes += n
		byDay[day] = append(byDay[day], dv{pid, n})
	}
	for _, list := range byDay { // ties share the crown
		best := 0
		for _, x := range list {
			best = max(best, x.votes)
		}
		for _, x := range list {
			if x.votes == best {
				tot[x.pid].Crowns++
			}
		}
	}
	out := make([]Score, 0, len(tot))
	for _, sc := range tot {
		out = append(out, *sc)
	}
	sortScores(out)
	return out, rows.Err()
}

func (s *Store) rooms() ([]Room, error) {
	rows, err := s.db.Query(`SELECT r.id, r.name, r.join_code, r.owner, (SELECT COUNT(*) FROM room_members m WHERE m.room_id=r.id) FROM rooms r ORDER BY r.created DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Room
	for rows.Next() {
		var r Room
		rows.Scan(&r.ID, &r.Name, &r.JoinCode, &r.Owner, &r.Members)
		out = append(out, r)
	}
	return out, rows.Err()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
