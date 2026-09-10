package app

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/munlucky/codex-account-pool/internal/observability"
)

type reportOptions struct {
	since    time.Duration
	timezone string
	from     string
	to       string
	files    []string
	stdin    bool
}

func (a *App) executeReport(args []string) error {
	opts, err := parseReportOptions(args)
	if err != nil {
		return err
	}
	loc, err := time.LoadLocation(opts.timezone)
	if err != nil {
		return fmt.Errorf("%w: invalid timezone %q", ErrUsage, opts.timezone)
	}
	end := time.Now()
	if opts.to != "" {
		end, err = parseReportTime(opts.to, loc)
		if err != nil {
			return err
		}
	}
	start := end.Add(-opts.since)
	if opts.from != "" {
		start, err = parseReportTime(opts.from, loc)
		if err != nil {
			return err
		}
	}
	if !start.Before(end) {
		return fmt.Errorf("%w: report start must be before end", ErrUsage)
	}

	report := observability.NewReport(observability.ReportOptions{Start: start, End: end, Location: loc})
	readers := make([]io.ReadCloser, 0)
	defer func() {
		for _, reader := range readers {
			_ = reader.Close()
		}
	}()

	if opts.stdin {
		if a.In == nil {
			return fmt.Errorf("report stdin is unavailable")
		}
		if err := report.AddReader(a.In); err != nil {
			return fmt.Errorf("read observability stdin: %w", err)
		}
	} else {
		paths := opts.files
		if len(paths) == 0 {
			paths = defaultObservabilityLogPaths(filepath.Join(a.Store.Root(), "observability", "events.jsonl"))
		}
		if len(paths) == 0 {
			return fmt.Errorf("no observability logs found; start the gateway first or pass --file/--stdin")
		}
		for _, path := range paths {
			file, err := os.Open(path)
			if err != nil {
				return fmt.Errorf("open observability log %s: %w", path, err)
			}
			readers = append(readers, file)
			if err := report.AddReader(file); err != nil {
				return fmt.Errorf("read observability log %s: %w", path, err)
			}
		}
	}
	report.WriteText(a.Out)
	return nil
}

func parseReportOptions(args []string) (reportOptions, error) {
	opts := reportOptions{since: 3 * time.Hour, timezone: "Asia/Seoul"}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--since":
			if i+1 >= len(args) {
				return reportOptions{}, fmt.Errorf("%w: --since requires a duration", ErrUsage)
			}
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil || d <= 0 {
				return reportOptions{}, fmt.Errorf("%w: invalid --since duration %q", ErrUsage, args[i])
			}
			opts.since = d
		case "--timezone":
			if i+1 >= len(args) {
				return reportOptions{}, fmt.Errorf("%w: --timezone requires a value", ErrUsage)
			}
			i++
			opts.timezone = args[i]
		case "--from":
			if i+1 >= len(args) {
				return reportOptions{}, fmt.Errorf("%w: --from requires a timestamp", ErrUsage)
			}
			i++
			opts.from = args[i]
		case "--to":
			if i+1 >= len(args) {
				return reportOptions{}, fmt.Errorf("%w: --to requires a timestamp", ErrUsage)
			}
			i++
			opts.to = args[i]
		case "--file":
			if i+1 >= len(args) {
				return reportOptions{}, fmt.Errorf("%w: --file requires a path", ErrUsage)
			}
			i++
			if args[i] == "-" {
				opts.stdin = true
			} else {
				opts.files = append(opts.files, args[i])
			}
		case "--stdin":
			opts.stdin = true
		default:
			return reportOptions{}, fmt.Errorf("%w: usage: report [--since 3h] [--timezone Asia/Seoul] [--from <time>] [--to <time>] [--file <jsonl>|--stdin]", ErrUsage)
		}
	}
	if opts.stdin && len(opts.files) > 0 {
		return reportOptions{}, fmt.Errorf("%w: --stdin and --file cannot be combined", ErrUsage)
	}
	return opts, nil
}

func parseReportTime(value string, loc *time.Location) (time.Time, error) {
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed, nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		if parsed, err := time.ParseInLocation(layout, value, loc); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("%w: invalid report time %q; use RFC3339 or YYYY-MM-DDTHH:MM:SS", ErrUsage, value)
}

func defaultObservabilityLogPaths(current string) []string {
	paths := make([]string, 0, observability.DefaultMaxFiles)
	for i := observability.DefaultMaxFiles - 1; i >= 1; i-- {
		path := current + "." + strconv.Itoa(i)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			paths = append(paths, path)
		}
	}
	if info, err := os.Stat(current); err == nil && !info.IsDir() {
		paths = append(paths, current)
	}
	return paths
}

func observabilityPaths(root string) (string, string) {
	base := filepath.Join(root, "observability")
	return filepath.Join(base, "events.jsonl"), filepath.Join(base, "daily")
}
