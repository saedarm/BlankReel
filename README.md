# Blank Reel

A daily fill-in-the-blank story. Fill in the blanks, see the story, and the server turns it into a narrated, captioned trailer (about 30 seconds) with a link you can drop in the group chat.

Each shot is one AI image (Gemini, Nano Banana 2 Lite) with a slow camera push or pan, the line read by an AI narrator (Gemini TTS), and the caption burned in. ffmpeg stitches it into an MP4.

## What you need

- Go 1.24+
- ffmpeg on your PATH (Windows: `winget install Gyan.FFmpeg`)
- A Gemini API key from Google AI Studio, with billing on. Without a key the server runs in test mode: plain color frames, no narration.

## Run it

```powershell
$env:GEMINI_API_KEY = "your-key"
$env:BLANKREEL_CODE = "pickle"     # master invite code; keeps strangers from spending your money
$env:BLANKREEL_ADMIN_KEY = "some-long-secret"   # turns on /admin
go run .
```

Open http://localhost:8080.

## Playing with friends

- **Names.** Everyone picks a display name the first time they open Rooms or make a trailer.
- **Invite codes.** Making a trailer needs an invite code, entered once per device. Anyone who enters a valid code becomes a beta tester: no paywall when payments arrive, up to 10 trailers a day each. `BLANKREEL_CODE` always works as a code, and you can make more on the admin page (one per friend, so you can switch off just one).
- **Rooms.** Make a room on the Rooms tab and send the invite link (`/r/<code>`). Each day's room wall shows everyone's trailer for the daily story. Today's wall stays hidden until you've made your own, so nobody spoils the script. Bonus stories don't go on room walls.
- **Votes.** One vote per person per day per room, never for yourself. The most-voted trailer is the day's top pick, and the room has a monthly leaderboard.
- **Random button.** The 🎲 next to each blank picks a word from `words.json`. "Fill the rest at random" does the remaining blanks in one go.
- **Another device.** Rooms → "Play on another device" gives a personal link that logs you in as the same player.

## Public feed

- **Publish.** After a trailer renders, its owner can tap "Publish to the feed." It goes into the review queue on the admin page. Once you approve it, it's on `/feed` for everyone.
- **Review.** On by default. Set `BLANKREEL_REVIEW=off` to skip the queue. Answers that hit the word filter (`blocklist.txt`, plus any words in the `BLANKREEL_BLOCKLIST` secret) always wait for review.
- **👍 / 👎.** Anyone can like or dislike, one reaction per player per trailer, and never on their own. "Today's best" and "This week" sort by likes minus dislikes. "Newest" sorts by publish time.
- **Report.** Three reports from different players hide a trailer automatically. Unhide it from the admin page if it was fine.
- **Trailer of the Day.** Feature any approved trailer from the admin page. It's pinned at the top of the feed for two days, and it's your daily social post.
- **Originality score.** When a player reveals today's story, their answers are saved (that costs nothing) and scored like Krillion: each blank is worth up to 100, and the fewer other players typed the same thing, the more it's worth. Scores drop as more people play. The feed shows today's most-original leaderboard.

## Admin page

Set `BLANKREEL_ADMIN_KEY`, then open `/admin?key=<that key>`. It shows today's trailer count and estimated AI spend. You can also approve or reject trailers in the review queue, feature a Trailer of the Day, see likes, dislikes, and reports on everything published, create and turn off invite codes, hide any trailer (it disappears from rooms and its share link stops working), and see all rooms. Without the key set, the page doesn't exist.

## Put it online (Fly.io)

This gives you a permanent `https://<app>.fly.dev` link, and your PC can be off. Your friends just open the link.

One-time setup, in PowerShell from this folder:

1. Install the Fly CLI: `iwr https://fly.io/install.ps1 -useb | iex`, then reopen PowerShell.
2. Sign up or log in: `fly auth signup` (or `fly auth login`). Fly asks for a card.
3. Open `fly.toml` and change `app = "blankreel-sam"` to a name nobody else has taken.
4. Create the app: `fly apps create <your-app-name>`
5. Create a 1 GB disk for the videos: `fly volumes create blankreel_data --region ord --size 1`
6. Add your secrets (these never go in the code):
   `fly secrets set GEMINI_API_KEY=your-key BLANKREEL_CODE=pickle BLANKREEL_ADMIN_KEY=some-long-secret`
7. Ship it: `fly deploy`

Open `https://<your-app-name>.fly.dev`, make one trailer, then send your friends the link and the crew code.

After that:

- Change code or stories, then run `fly deploy` again.
- `fly logs` shows what the server is doing, including failed renders.
- `fly secrets set BLANKREEL_CODE=newcode` changes the crew code.

The server sleeps when nobody is using it and wakes on the next visit. The first load after a nap takes a few seconds. If someone closes the page in the middle of a render, the server can fall asleep and that trailer gets lost. They just make it again, and it doesn't count against the daily limit.

Run one machine only (the volume makes that the default). The render queue lives in memory, so two machines would each keep their own.

## Share from your own PC instead

`cloudflared tunnel --url http://localhost:8080` prints a public `https://….trycloudflare.com` link to the server running on your PC. It's free, but the link changes on every restart and only works while your PC is on. Set `BLANKREEL_CODE` first.

## Settings

| Variable | Default | What it does |
|---|---|---|
| `GEMINI_API_KEY` | none | Turns on real images and narration |
| `BLANKREEL_CODE` | none | Master invite code. With no code and no admin-made codes, anyone can make trailers |
| `BLANKREEL_ADMIN_KEY` | none | Turns on `/admin?key=…` |
| `BLANKREEL_DAILY_LIMIT` | 40 | Max trailers per day for everyone combined. Failed renders don't count |
| `BLANKREEL_PLAYER_LIMIT` | 10 | Max trailers per player per day |
| `BLANKREEL_REVIEW` | on | `off` sends published trailers straight to the feed (filtered words still wait) |
| `BLANKREEL_BLOCKLIST` | none | Extra filtered words, comma-separated, on top of `blocklist.txt` |
| `BLANKREEL_VOICE` | Charon | Narrator voice (Puck, Kore, Fenrir, Zephyr…) |
| `BLANKREEL_IMAGE_MODEL` | gemini-3.1-flash-lite-image | Swap to `gemini-nano-banana-2.1` for nicer, pricier images |
| `BLANKREEL_TTS_MODEL` | gemini-3.8-flash-lite-tts | Narration model |
| `BLANKREEL_LAUNCH` | 2026-10-01 | Day #1 of the daily count |
| `PORT` | 8080 | |
| `DATA_DIR` | data | Where trailers and the `blankreel.db` database live |

## Cost

Six images plus six short narration lines per trailer. At current Gemini prices that's roughly a quarter or less per trailer, so the default daily limit of 40 caps you around $10 a day. Check your usage in AI Studio after the first few runs to confirm.

## Adding stories

Stories live in `stories.json`, compiled into the binary. Each one has `blanks` (label, example hint, and `kind`, which picks the 🎲 word list in `words.json`) and six `scenes`. In each scene, `line` is what the narrator reads and the caption shows, and `shot` is the image prompt. `{0}`, `{1}`… are the answers by blank number. The daily story rotates through the list.

## How it fits together

- `main.go`: server setup, the daily story, the one-at-a-time render queue
- `api.go`: players, invite codes, trailers, rooms, votes, share pages
- `store.go`: the SQLite database (players, codes, rooms, trailers, votes)
- `feed.go`: publishing, the public feed, likes and dislikes, reports, originality scores
- `admin.go`: the admin page
- `blocklist.txt`: words that force a trailer into review
- `words.json`: word lists for the random button
- `gemini.go`: image and speech calls to the Gemini Interactions API
- `render.go`: ffmpeg title card, per-shot clips, stitching
- `web/index.html`: the game page
- `Dockerfile`, `fly.toml`: the container and Fly.io settings

Players are identified by a random token the page keeps in the browser and sends as `X-Player-Token`. There are no passwords. `GET /t/{id}` is the public share page, with link-preview tags so the video shows up when you text it.
