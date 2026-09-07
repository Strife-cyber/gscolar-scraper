# gscolar — stealthy Google Scholar scraper

Collects as many **unique** papers as possible from a list of AI/ML
conferences (2000 → present) by driving your **real** Chrome/Edge on its normal
profile, emulating human typing, mouse movement and reading pauses. Every raw
results page is stored in SQLite, and the crawl is fully **resumable** across
CAPTCHA breaks and crashes.

Built for a single, non-commercial personal PC. This document is about the
technical implementation only.

---

## How it works

1. **Plan** (`-plan`) — for each conference phrase, the planner asks Scholar
   for the result count of the whole `2000→present` range, then recursively
   **halves oversized year ranges**. A single year still over the 1000-result
   cap is partitioned by **sequential keyword subtraction**
   (`q AND "A"`, `q AND "B" -"A"`, `q AND "C" -"A" -"B"`, … plus a residual
   bucket `q -"A" -"B" -"C"`), so every paper lands in exactly one task. Counts
   are cached so re-planning never re-queries Scholar. Leaves still over the
   cap are recorded as `needs_split` TODOs for a later, finer re-plan.

2. **Crawl** (`-crawl`) — each pending task is searched with the UI (query is
   typed character-by-character, the custom year range is set through the
   sidebar), then paged via the real "Next" link. Each page is committed in one
   WAL transaction: **raw page HTML + deduped papers + the task's page
   pointer**. Stop it any time (or get CAPTCHA'd); the next run resumes exactly
   where it stopped.

3. **CAPTCHA / blocks** — when a block page appears you get a desktop
   notification, solve it in the browser, and the scraper resumes automatically
   once the page is unblocked. It then waits a "human returning from a break"
   delay before reloading. New `timing` options (`CaptchaPollIntervalMS`,
   `CaptchaTimeoutMS`, `LoginTimeoutMS`) will be added by another subagent to
   control CAPTCHA and sign-in polling and timeouts. Each CAPTCHA also raises
   an adaptive throttle that slows the crawl.

### Deduplication

A paper's primary key is `sha256(normalized title + year + first-author last
name)`. Titles are normalized so punctuation doesn't split spellings
("Model-Agnostic" ≡ "Model Agnostic", case-insensitive, whitespace-collapsed),
so the same paper found under several keywords/venues is stored once.

---

## Build

Requires **Go ≥ 1.21** (go.mod targets 1.26).

```powershell
# Windows
.\scripts\build.ps1        # → bin\gscolar.exe (static, no runtime deps)

# or
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin\gscolar.exe ./cmd/gscolar
```

---

## Configure

```powershell
Copy-Item config.example.json config.json
```

Edit `config.json`:

| key | meaning |
| --- | --- |
| `database` | SQLite file path (default `scholar.db`) |
| `start_year` | lower bound of the date range (default `2000`) |
| `max_results` | Scholar's per-query cap (default `1000`) |
| `browser.kind` | `chrome`, `edge`, or `auto` (detect) |
| `browser.profile_dir` | override the user-data dir; empty = your real profile |
| `browser.debug_port` | CDP port (default `9222`) |
| `browser.headless` | keep `false` — headless looks like a bot |
| `timing.*` | all `[min,max]` millisecond ranges for human delays |
| `adaptive_throttle` | delay multiplier growth per CAPTCHA |
| `keywords` | ordered subtraction chain (order matters, see above) |
| `conferences` | `name` + `query` per venue; `use_short_name` uses `name` as the query |

The shipped `config.example.json` already lists all 20 conferences.

---

## Usage

> **First run:** close your Chrome/Edge. The scraper relaunches it with remote
> debugging on your real profile. If it is already running, Chrome ignores the
> debug flag and the scrape fails with a hint.

```powershell
# Progress report (no browser needed)
.\bin\gscolar.exe -config config.json -status

# Plan all conferences (drives the browser, ~15–30 s between count queries)
.\bin\gscolar.exe -config config.json -plan

# Plan then crawl just one conference
.\bin\gscolar.exe -config config.json -plan -crawl -conference ICML

# Crawl everything pending; Ctrl+C is safe (each page is committed atomically)
.\bin\gscolar.exe -config config.json -crawl

# Re-plan with finer keywords to clear needs_split TODOs, then crawl again
.\bin\gscolar.exe -config config.json -plan -crawl
```

Run `-plan -crawl` repeatedly: each pass clears completed tasks and re-splits
what `needs_split` flagged until the TODOs are gone.

---

## Data

`scholar.db` (WAL mode):

| table | contents |
| --- | --- |
| `conferences` | name + search phrase |
| `tasks` | one row per query × year-range × keyword-chain; `page` is the resume pointer |
| `papers` | deduped papers (hash PK, title/authors/year/snippet/citations/URL, raw `.gs_r` HTML block) |
| `page_html` | the full raw HTML of every results page |
| `count_cache` | "About N results" counts so re-planning is free |

```sql
-- how many unique papers per conference
SELECT c.name, COUNT(p.hash) FROM papers p
JOIN tasks t ON t.id = p.task_id
JOIN conferences c ON c.id = t.conference_id
GROUP BY c.name ORDER BY COUNT(p.hash) DESC;
```

---

## Anti-detection posture

- Real browser profile (your cookies, history, JS fingerprint), never headless.
- `stealth` patches automation telltales (`navigator.webdriver`, …).
- Query typed 80–250 ms/char; mouse moves along Bézier curves in small steps
  with ±4 px click offsets; occasional "reading" scrolls.
- 30–60 s between pages, 60–120 s between searches, longer after a CAPTCHA.
- Adaptive throttle: delay multiplier grows by `step` after each CAPTCHA (cap
  `max`), so repeated blocks slow the crawl down automatically.
- Search, year-range and pagination are all done through the **UI** — URLs are
  never hand-edited and `?start=` is never constructed.

## Troubleshooting

- **"No browser found on the debug port …"** — close all browser windows and
  retry.
- **`custom-range trigger not found`** — Scholar changed its hamburger-menu or
  advanced-search markup; update `selMenuBtn`, `selAdvYearFrom`, and
  `selAdvYearTo` in `internal/browser/scholar.go` (these are the selectors most
  likely to drift).
- **CAPTCHA loop** — the scraper auto-resumes once the page is unblocked; if
  blocks are frequent, raise `timing.CaptchaPollIntervalMS` and/or
  `between_pages_ms` / `between_searches_ms` in the config.
- **`needs_split` never clears** — a single year for that venue genuinely
  exceeds 1000 even after subtraction; add venue-specific keywords to `keywords`
  (earlier in the list wins) and re-run `-plan`.
