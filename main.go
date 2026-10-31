package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"tblocker/blocker"
	"tblocker/config"
	"tblocker/logwatch"
	"tblocker/notify"
	"tblocker/panel"
	"tblocker/storage"
)

// Version is set at build time with -X main.Version=...
var Version = "dev"

const detectionQueueSize = 1024

func main() {
	log.SetFlags(log.LstdFlags)

	opts := parseFlags()
	if opts.showVersion {
		fmt.Printf("PasarGuard torrent-blocker: %s\n", Version)
		return
	}

	cfg, warnings, err := config.Load(opts.configPath)
	if err != nil {
		log.Fatalf("FATAL: %v", err)
	}
	for _, warning := range warnings {
		log.Printf("config warning: %s", warning)
	}

	log.Printf("PasarGuard torrent-blocker %s starting", Version)
	log.Printf("host=%s log=%s block=%dm identifier=%s polling=%t perf=%t",
		cfg.Hostname, cfg.LogFile, cfg.BlockDuration, cfg.PasarGuard.Identifier, cfg.UsePolling, opts.perf)

	store, err := storage.New(cfg.StorageDir)
	if err != nil {
		log.Fatalf("FATAL: open block store: %v", err)
	}
	defer func() {
		_ = store.Close()
	}()
	log.Printf("journal=%s records=%d", store.Path(), store.Len())

	if dir := filepath.Dir(cfg.LogFile); dir != "." {
		if info, statErr := os.Stat(dir); statErr != nil || !info.IsDir() {
			log.Printf("WARN: log directory %s is not accessible; waiting for the log file to appear", dir)
		}
	}

	panelClient, err := panel.New(
		cfg.PasarGuard.BaseURL,
		cfg.PasarGuard.APIKey,
		time.Duration(cfg.PasarGuard.TimeoutSeconds)*time.Second,
		panel.WithInsecureTLS(cfg.PasarGuard.InsecureSkipVerify),
		panel.WithUserAgent("tblocker/"+Version),
	)
	if err != nil {
		log.Fatalf("FATAL: create PasarGuard client: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.PasarGuard.StartupCheckEnabled() {
		checkCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.PasarGuard.TimeoutSeconds)*time.Second)
		err := panelClient.Check(checkCtx)
		cancel()
		if err != nil {
			log.Printf("ERROR: PasarGuard startup check failed (the service will keep running and retry on each block): %v", err)
		} else {
			log.Printf("PasarGuard API check OK (%s)", cfg.PasarGuard.BaseURL)
		}
	}

	stats := newRuntimeStats()
	if opts.perf {
		go stats.report(ctx, time.Minute)
	}

	manager := blocker.New(cfg, store, panelClient, notify.New(cfg))
	go manager.Run(ctx, time.Duration(cfg.CheckIntervalSeconds)*time.Second)

	// Reading the log and calling the panel are deliberately decoupled. A slow
	// API must not stop the tail from draining the access log; duplicate
	// detections are cheap because the manager's identity cache short-circuits
	// them once the first block is active.
	detections := make(chan logwatch.Detection, detectionQueueSize)
	var worker sync.WaitGroup
	worker.Add(1)
	go func() {
		defer worker.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case detection, ok := <-detections:
				if !ok {
					return
				}
				if ctx.Err() != nil {
					return
				}
				if err := manager.Handle(ctx, detection); err != nil {
					log.Printf("ERROR: %v", err)
				}
			}
		}
	}()

	var lastDropLog time.Time
	runErr := logwatch.Follow(ctx, cfg.LogFile, cfg.UsePolling, func(line string) {
		var parseStart time.Time
		if opts.perf {
			stats.lines.Add(1)
			parseStart = time.Now()
		}

		detection, err := logwatch.ParseLine(cfg, line)
		if opts.perf && !parseStart.IsZero() {
			stats.parseNanos.Add(time.Since(parseStart).Nanoseconds())
		}
		if err != nil {
			if errors.Is(err, logwatch.ErrNoTorrentTag) {
				return
			}
			if opts.perf {
				stats.parseErrors.Add(1)
			}
			log.Printf("WARN: %v", err)
			return
		}

		if opts.perf {
			stats.detections.Add(1)
		}
		select {
		case detections <- detection:
		default:
			if opts.perf {
				stats.queueDrops.Add(1)
			}
			now := time.Now()
			if now.Sub(lastDropLog) >= 5*time.Second {
				log.Printf("WARN: detection queue full (%d); dropping detection for %s", detectionQueueSize, detection.Identity)
				lastDropLog = now
			}
		}
	})

	close(detections)
	worker.Wait()

	if opts.perf {
		log.Printf("PERF final %s", stats.snapshot())
	}
	if runErr != nil {
		log.Fatalf("FATAL: %v", runErr)
	}
	log.Printf("shutdown complete")
}

type cliOptions struct {
	configPath  string
	showVersion bool
	perf        bool
}

func parseFlags() cliOptions {
	var opts cliOptions

	flag.StringVar(&opts.configPath, "c", "", "path to the configuration file")
	flag.BoolVar(&opts.showVersion, "v", false, "display version and exit")
	flag.BoolVar(&opts.perf, "perf", false, "log performance metrics once per minute")
	flag.Parse()

	if !opts.showVersion && opts.configPath == "" {
		executable, err := os.Executable()
		if err != nil {
			log.Fatalf("FATAL: determine executable path: %v", err)
		}
		opts.configPath = filepath.Join(filepath.Dir(executable), "config.yaml")
	}
	return opts
}
