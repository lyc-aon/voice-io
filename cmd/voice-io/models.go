package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"time"

	"gitlab.com/integral-inference/voice-io/internal/models"
	"gitlab.com/integral-inference/voice-io/internal/paths"
)

func cmdModels(args []string) error {
	if len(args) == 0 {
		return errors.New("expected install or verify")
	}
	action := args[0]
	fs := flag.NewFlagSet("models "+action, flag.ContinueOnError)
	meeting := fs.Bool("meeting", false, "include the optional speaker-labelling models")
	if err := fs.Parse(args[1:]); err != nil {
		return exitCode(2)
	}
	groups := []models.Group{models.Core}
	if *meeting {
		groups = append(groups, models.Meeting)
	}
	root := paths.ModelsDir()
	var found map[string]string
	var err error
	switch action {
	case "install":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		bar := newProgressPrinter()
		in := &models.Installer{Root: root, Progress: bar.update}
		found, err = in.Install(ctx, groups...)
		bar.finish()
	case "verify":
		found, err = models.Verify(root, groups...)
	default:
		return fmt.Errorf("unknown action %q; expected install or verify", action)
	}
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(found))
	for id := range found {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		fmt.Printf("%s: %s\n", id, found[id])
	}
	return nil
}

type progressPrinter struct {
	last     time.Time
	model    string
	terminal bool
}

func newProgressPrinter() *progressPrinter {
	info, err := os.Stderr.Stat()
	return &progressPrinter{terminal: err == nil && info.Mode()&os.ModeCharDevice != 0}
}

func (p *progressPrinter) update(pr models.Progress) {
	switch pr.Phase {
	case "present":
		fmt.Fprintf(os.Stderr, "%-18s already installed\n", pr.ModelID)
		return
	case "installed":
		p.clear()
		fmt.Fprintf(os.Stderr, "%-18s installed (%s)\n", pr.ModelID, humanBytes(pr.Total))
		return
	case "extract":
		p.clear()
		fmt.Fprintf(os.Stderr, "%-18s extracting…\n", pr.ModelID)
		return
	}
	now := time.Now()
	if pr.ModelID == p.model && now.Sub(p.last) < 250*time.Millisecond {
		return
	}
	p.model, p.last = pr.ModelID, now
	line := fmt.Sprintf("%-18s %5.1f%%  %s / %s   (all: %s / %s)", pr.ModelID,
		percent(pr.Done, pr.Total), humanBytes(pr.Done), humanBytes(pr.Total),
		humanBytes(pr.TotalDone), humanBytes(pr.TotalBytes))
	if p.terminal {
		fmt.Fprintf(os.Stderr, "\r\033[K%s", line)
	} else if now.Second()%10 == 0 {
		fmt.Fprintln(os.Stderr, line)
	}
}

func (p *progressPrinter) clear() {
	if p.terminal {
		fmt.Fprint(os.Stderr, "\r\033[K")
	}
}

func (p *progressPrinter) finish() { p.clear() }

func percent(done, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return 100 * float64(done) / float64(total)
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
