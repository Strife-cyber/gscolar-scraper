// Command gscolar is the Google Scholar scraper's single executable.
//
// Usage:
//
//	gscolar -config config.json -status
//	gscolar -config config.json -plan            # plan every conference
//	gscolar -config config.json -plan -conference ICML
//	gscolar -config config.json -crawl           # crawl pending tasks
//	gscolar -config config.json -crawl -conference ICML
//	gscolar -config config.json -plan -crawl -conference ICML   # plan then crawl
//	gscolar -config config.json -gen-tasks       # build tasks from the DB only (no browser)
//	gscolar -config config.json -rerun-shortfall -crawl -conference ICML  # retry under-delivered tasks
//
// -plan and -crawl both drive the real browser and are resumable: re-running
// them continues from the database's checkpoints (every page is committed
// atomically with the task's page pointer). -gen-tasks performs no searches at
// all — it materializes a plan purely from the papers and count cache already
// in the DB, so you can generate crawl tasks with what's available.
// -rerun-shortfall re-queues 'incomplete' and 'error' tasks to 'pending'
// (no browser) so a following -crawl retries them from their checkpoint;
// a 'needs_split' task is instead retried by re-running -plan.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"gscolar-scraper/internal/browser"
	"gscolar-scraper/internal/config"
	"gscolar-scraper/internal/crawl"
	"gscolar-scraper/internal/db"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to the JSON config")
	dbPath := flag.String("db", "", "SQLite database path (overrides config.database)")
	doPlan := flag.Bool("plan", false, "compute/sync the task plan (drives the browser)")
	doCrawl := flag.Bool("crawl", false, "crawl pending tasks (drives the browser)")
	doGen := flag.Bool("gen-tasks", false, "build tasks from the DB only (no browser)")
	doRerunShortfall := flag.Bool("rerun-shortfall", false, "re-queue incomplete/error tasks to pending (no browser)")
	doStatus := flag.Bool("status", false, "print progress statistics and exit")
	confName := flag.String("conference", "", "restrict -plan/-crawl/-gen-tasks/-rerun-shortfall to one conference by name")
	flag.Usage = usage
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		logger.Error("config", "err", err)
		os.Exit(1)
	}
	if *dbPath != "" {
		cfg.Database = *dbPath
	}

	ctx := context.Background()
	d, err := db.Open(cfg.Database)
	if err != nil {
		logger.Error("db", "err", err)
		os.Exit(1)
	}
	defer func(d *db.DB) {
		err := d.Close()
		if err != nil {
			return
		}
	}(d)

	// Keep the conference table in sync with the config (upserts are idempotent).
	if err := seedConferences(ctx, d, cfg.Conferences); err != nil {
		logger.Error("seed conferences", "err", err)
		os.Exit(1)
	}

	if *doStatus {
		printStatus(ctx, d)
		if !*doPlan && !*doCrawl && !*doGen && !*doRerunShortfall {
			return
		}
		fmt.Println()
	}

	if !*doPlan && !*doCrawl && !*doGen && !*doRerunShortfall {
		flag.Usage()
		return
	}

	confs, err := selectConferences(ctx, d, *confName)
	if err != nil {
		logger.Error("select conferences", "err", err)
		os.Exit(1)
	}

	// -gen-tasks builds the plan offline from the DB (count_cache + papers) and
	// never touches the browser; only -plan/-crawl launch it. browser.New(cfg)
	// merely constructs the client — Connect() is what spawns/attaches Chrome.
	br := browser.New(cfg)
	if *doPlan || *doCrawl {
		if err := br.Connect(); err != nil {
			logger.Error("browser", "err", err)
			os.Exit(1)
		}
		defer func(br *browser.Browser) {
			err := br.Close()
			if err != nil {
				return
			}
		}(br)
	}
	c := crawl.New(cfg, d, br, logger)

	// Catch Ctrl+C / SIGTERM by cancelling ctx: crawlTask/CrawlConference only
	// observe it at safe checkpoints (right after a page's CommitPage has
	// durably saved it), so the current page's already-scraped results are
	// never abandoned — the task is left 'running' and resumes cleanly from
	// its committed page pointer next time. A second signal is a genuine
	// "stop now" escape hatch (e.g. a hung browser call ctx cannot interrupt)
	// and force-exits immediately, same as before.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		sig := <-sigCh
		logger.Info(fmt.Sprintf("received %v, shutting down after the current page (press again to force-quit)", sig))
		cancel()
		sig = <-sigCh
		logger.Info(fmt.Sprintf("received %v again: force-quitting", sig))
		for _, conf := range confs {
			if err := d.ResetStaleRunning(context.Background(), conf.ID); err != nil {
				logger.Info(fmt.Sprintf("reset running tasks %s: %v", conf.Name, err))
			}
		}
		if err := d.Close(); err != nil {
			logger.Info(fmt.Sprintf("db close: %v", err))
		}
		os.Exit(130)
	}()

	for _, conf := range confs {
		if err := ctx.Err(); err != nil {
			logger.Info(fmt.Sprintf("shutdown requested, stopping before conference %s", conf.Name))
			break
		}

		if *doRerunShortfall {
			n, err := c.RerunShortfall(ctx, conf.ID)
			if err != nil {
				logger.Info(fmt.Sprintf("rerun-shortfall %s: %v", conf.Name, err))
				continue
			}
			logger.Info(fmt.Sprintf("%s: %d incomplete/error tasks re-queued to pending", conf.Name, n))
		}
		if *doGen {
			need, err := c.OfflineGenTasks(ctx, conf)
			if err != nil {
				logger.Info(fmt.Sprintf("gen-tasks %s: %v", conf.Name, err))
				continue
			}
			logger.Info(fmt.Sprintf("%s tasks generated from DB: %d leaves (%d need re-split)", conf.Name, countLeaves(ctx, d, conf.ID), need))
		}
		if *doPlan && ctx.Err() == nil {
			need, err := c.PlanConference(ctx, conf)
			if err != nil {
				logger.Info(fmt.Sprintf("plan %s: %v", conf.Name, err))
				continue
			}
			logger.Info(fmt.Sprintf("%s planned: %d leaves (%d need re-split)", conf.Name, countLeaves(ctx, d, conf.ID), need))
		}
		if *doCrawl && ctx.Err() == nil {
			if err := c.CrawlConference(ctx, conf.ID); err != nil {
				logger.Info(fmt.Sprintf("crawl %s: %v", conf.Name, err))
				continue
			}
		}
	}
}

// seedConferences upserts every config entry into the conferences table.
func seedConferences(ctx context.Context, d *db.DB, cfgs []config.ConferenceCfg) error {
	for _, c := range cfgs {
		if _, err := d.UpsertConference(ctx, c.Name, c.Query); err != nil {
			return fmt.Errorf("%s: %w", c.Name, err)
		}
	}
	return nil
}

// selectConferences filters the stored list by name, or returns all.
func selectConferences(ctx context.Context, d *db.DB, name string) ([]db.Conference, error) {
	all, err := d.ListConferences(ctx)
	if err != nil {
		return nil, fmt.Errorf("list conferences: %w", err)
	}
	if name == "" {
		return all, nil
	}
	for _, c := range all {
		if c.Name == name {
			return []db.Conference{c}, nil
		}
	}
	names := make([]string, 0, len(all))
	for _, c := range all {
		names = append(names, c.Name)
	}
	return nil, fmt.Errorf("no conference named %q (known: %v)", name, names)
}

// countLeaves returns the number of tasks recorded for a conference.
func countLeaves(ctx context.Context, d *db.DB, confID int64) int {
	tasks, err := d.AllTasks(ctx, confID)
	if err != nil {
		return -1
	}
	return len(tasks)
}

// printStatus prints global and per-conference progress counters.
func printStatus(ctx context.Context, d *db.DB) {
	s, err := d.GetStats(ctx)
	if err != nil {
		slog.Error("status", "err", err)
		return
	}
	fmt.Printf("conferences:  %d\n", s.Conferences)
	fmt.Printf("papers:       %d unique\n", s.Papers)
	fmt.Printf("pages:        %d raw pages stored\n", s.Pages)
	fmt.Printf("tasks:        %d pending, %d running, %d completed, %d needs_split, %d incomplete\n",
		s.Pending, s.Running, s.Completed, s.NeedsSplit, s.Incomplete)
}

func usage() {
	_, err := fmt.Fprintf(os.Stderr, `gscolar - stealthy Google Scholar scraper

Usage:
  gscolar [flags]

Flags:
  -config path     JSON config file (default "config.json")
  -db path         SQLite database path (overrides config.database)
  -plan            compute/sync the task plan for each conference (also
                    retries any needs_split leaf from scratch)
  -crawl           crawl pending tasks (resumable from checkpoints)
  -gen-tasks       build tasks from the DB only (count_cache + papers, no browser)
  -rerun-shortfall re-queue incomplete/error tasks to pending (no browser);
                    combine with -crawl to retry them in the same run
  -conference name restrict -plan/-crawl/-gen-tasks/-rerun-shortfall to one
                    conference (e.g. ICML)
  -status          print progress statistics

A task is marked 'incomplete' when Scholar's own pagination ends a crawl (no
next page) far short of the task's planned result estimate — Scholar
under-delivering relative to its own "About N results" count, not a crawl
failure. -rerun-shortfall re-queues it (and any 'error' task) to pending
without resetting its page pointer, so the retry resumes and paginates
forward to wherever the previous run stopped instead of re-scraping from page
1. A 'needs_split' task (over the cap, never resolved) is instead retried by
re-running -plan, which re-attempts the recursive keyword split from scratch.

The crawler drives your real Chrome/Edge on its normal profile. Before the
first run, close your browser so the scraper can relaunch it with remote
debugging enabled.
`)
	if err != nil {
		return
	}
}
