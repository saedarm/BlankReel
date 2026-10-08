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
$env:BLANKREEL_CODE = "pickle"     # crew code your friends type once; keeps strangers from spending your money
go run .
```

Open http://localhost:8080.

## Put it online (Fly.io)

This gives you a permanent `https://<app>.fly.dev` link, and your PC can be off. Your friends just open the link.

One-time setup, in PowerShell from this folder:

1. Install the Fly CLI: `iwr https://fly.io/install.ps1 -useb | iex`, then reopen PowerShell.
2. Sign up or log in: `fly auth signup` (or `fly auth login`). Fly asks for a card.
3. Open `fly.toml` and change `app = "blankreel-sam"` to a name nobody else has taken.
4. Create the app: `fly apps create <your-app-name>`
5. Create a 1 GB disk for the videos: `fly volumes create blankreel_data --region ord --size 1`
6. Add your secrets (these never go in the code):
   `fly secrets set GEMINI_API_KEY=your-key BLANKREEL_CODE=pickle`
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
| `BLANKREEL_CODE` | none | Crew code required to make a trailer |
| `BLANKREEL_DAILY_LIMIT` | 40 | Max trailers per day. Failed renders don't count |
| `BLANKREEL_VOICE` | Charon | Narrator voice (Puck, Kore, Fenrir, Zephyr…) |
| `BLANKREEL_IMAGE_MODEL` | gemini-3.1-flash-lite-image | Swap to `gemini-nano-banana-2.1` for nicer, pricier images |
| `BLANKREEL_TTS_MODEL` | gemini-3.8-flash-lite-tts | Narration model |
| `BLANKREEL_LAUNCH` | 2026-10-01 | Day #1 of the daily count |
| `PORT` | 8080 | |
| `DATA_DIR` | data | Where finished trailers live |

## Cost

Six images plus six short narration lines per trailer. At current Gemini prices that's roughly a quarter or less per trailer, so the default daily limit of 40 caps you around $10 a day. Check your usage in AI Studio after the first few runs to confirm.

## Adding stories

Stories live in `stories.json`, compiled into the binary. Each one has `blanks` (label + example hint) and six `scenes`. In each scene, `line` is what the narrator reads and the caption shows, and `shot` is the image prompt. `{0}`, `{1}`… are the answers by blank number. The daily story rotates through the list.

## How it fits together

- `main.go`: HTTP server, the daily story, a one-at-a-time render queue, share pages
- `gemini.go`: image and speech calls to the Gemini Interactions API
- `render.go`: ffmpeg title card, per-shot clips, stitching
- `web/index.html`: the game page
- `Dockerfile`, `fly.toml`: the container and Fly.io settings

Routes: `GET /api/today`, `POST /api/trailers`, `GET /api/trailers/{id}`, `GET /v/{id}.mp4`, and `GET /t/{id}`, the share page, which has link-preview tags so the video shows up when you text it.
