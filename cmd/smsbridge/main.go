// smsbridge copies Google Messages (SMS, MMS, RCS) into SQLite and a Google Sheet.
//
// Commands:
//
//	smsbridge pair -firefox-profile DIR   pair with your phone (tap the emoji shown)
//	smsbridge sync [-backfill-pages N]    fetch new messages (and continue history backfill)
//	smsbridge push                        append unpushed messages to the Sheet
//	smsbridge run [-force]                scheduled entry point: sync+push when 10h have passed
//	smsbridge status                      show counts and last run
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/automationinternsdotcom/sms-bridge/internal/cookies"
	"github.com/automationinternsdotcom/sms-bridge/internal/gm"
	"github.com/automationinternsdotcom/sms-bridge/internal/sheets"
	"github.com/automationinternsdotcom/sms-bridge/internal/store"
)

type config struct {
	SpreadsheetID string `json:"spreadsheet_id"`
	KeyFile       string `json:"service_account_key"`
	GcloudAccount string `json:"gcloud_account"`
	// KeepLocalText keeps message content in the local SQLite after it is pushed.
	// Default false: text lives only in the Sheet; the DB keeps IDs/timestamps.
	KeepLocalText bool   `json:"keep_local_text"`
	GcloudBin     string `json:"gcloud_bin"`
	IntervalHours int    `json:"interval_hours"`
}

var dataDir = filepath.Join(os.Getenv("HOME"), "Library", "Application Support", "sms-bridge")

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		die(err)
	}
	_ = os.Chmod(dataDir, 0o700)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var err error
	switch os.Args[1] {
	case "pair":
		err = cmdPair(ctx, os.Args[2:])
	case "sync":
		err = cmdSync(ctx, os.Args[2:])
	case "push":
		err = cmdPush(ctx)
	case "run":
		err = cmdRun(ctx, os.Args[2:])
	case "daemon":
		err = cmdDaemon(ctx)
	case "status":
		err = cmdStatus()
	default:
		usage()
	}
	if err != nil {
		die(err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: smsbridge pair|sync|push|run|daemon|status")
	os.Exit(2)
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

func logger(verbose bool) zerolog.Logger {
	f, err := os.OpenFile(filepath.Join(dataDir, "smsbridge.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	var w io.Writer = os.Stderr
	if err == nil {
		if verbose {
			w = zerolog.MultiLevelWriter(f, zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.Kitchen})
		} else {
			w = f
		}
	}
	return zerolog.New(w).With().Timestamp().Logger().Level(zerolog.InfoLevel)
}

func openAll(verbose bool) (*store.Store, *gm.Client, error) {
	st, err := store.Open(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		return nil, nil, err
	}
	_ = os.Chmod(filepath.Join(dataDir, "messages.db"), 0o600)
	return st, &gm.Client{SessionPath: filepath.Join(dataDir, "session.json"), Log: logger(verbose), Store: st}, nil
}

func loadConfig() (*config, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, "config.json"))
	if err != nil {
		return nil, fmt.Errorf("read config.json: %w", err)
	}
	c := &config{IntervalHours: 10}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, err
	}
	if c.GcloudBin == "" {
		c.GcloudBin = "/opt/homebrew/bin/gcloud"
	}
	if c.KeyFile == "" {
		c.KeyFile = filepath.Join(dataDir, "service-account.json")
	}
	return c, nil
}

func cmdPair(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pair", flag.ExitOnError)
	profile := fs.String("firefox-profile", filepath.Join(dataDir, "firefox-profile"), "Firefox profile dir signed in to messages.google.com")
	_ = fs.Parse(args)
	ck, err := cookies.FromFirefoxProfile(*profile)
	if err != nil {
		return err
	}
	fmt.Printf("Found %d Google cookies. Starting pairing...\n", len(ck))
	lock, err := syncLock()
	if err != nil {
		return fmt.Errorf("%w: stop the daemon first (launchctl bootout gui/$(id -u)/%s)", err, launchLabel())
	}
	defer lock.Close()
	st, c, err := openAll(true)
	if err != nil {
		return err
	}
	defer st.Close()
	pctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	err = c.Pair(pctx, ck, func(emoji string) {
		fmt.Printf("\n>>> On your phone, Google Messages will ask you to confirm. Tap this emoji:  %s\n\n", emoji)
		_ = os.WriteFile(filepath.Join(dataDir, "pair-emoji.txt"), []byte(emoji+"\n"), 0o600)
		notify("Tap " + emoji + " on your phone to pair")
	})
	if err != nil {
		return fmt.Errorf("pairing failed: %w", err)
	}
	_ = st.SetMeta("paired_at", time.Now().Format(time.RFC3339))
	fmt.Println("Paired. Session saved.")
	lock.Close()
	if exec.Command("launchctl", "kickstart", "-k", fmt.Sprintf("gui/%d/%s", os.Getuid(), launchLabel())).Run() == nil {
		fmt.Println("Restarted the background daemon.")
	}
	return nil
}

func launchLabel() string {
	if l := os.Getenv("SMSBRIDGE_LABEL"); l != "" {
		return l
	}
	return "com.smsbridge.daemon"
}

// syncLock ensures only one process uses the Google Messages session at a time.
func syncLock() (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dataDir, "sync.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errSyncBusy
	}
	return f, nil
}

var errSyncBusy = errors.New("another sync is already running")

func doSync(ctx context.Context, verbose bool, backfillPages int) (*gm.SyncStats, error) {
	lock, err := syncLock()
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	st, c, err := openAll(verbose)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	sctx, cancel := context.WithTimeout(ctx, 3*time.Hour)
	defer cancel()
	if err := c.Connect(sctx); err != nil {
		return nil, err
	}
	defer c.Disconnect()
	_ = st.SetMeta("account_email", c.Email())
	stats, err := c.Sync(sctx, backfillPages)
	if stats != nil {
		b, _ := json.Marshal(stats)
		c.Log.Info().RawJSON("stats", b).Msg("sync finished")
	}
	if err == nil {
		_ = st.SetMeta("last_sync", time.Now().Format(time.RFC3339))
	}
	return stats, err
}

func cmdSync(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	pages := fs.Int("backfill-pages", 0, "max history pages per run (0 = unlimited)")
	_ = fs.Parse(args)
	stats, err := doSync(ctx, true, *pages)
	if stats != nil {
		fmt.Printf("conversations=%d checked=%d new=%d pages=%d live=%d backfill_pending=%d\n",
			stats.Conversations, stats.Checked, stats.NewMessages, stats.Pages, stats.LiveEvents, stats.BackfillPending)
	}
	return err
}

func doPush(ctx context.Context) (int, error) {
	cfg, err := loadConfig()
	if err != nil {
		return 0, err
	}
	lock, err := syncLock()
	if err != nil {
		return 0, fmt.Errorf("%w (the daemon pushes on its own)", err)
	}
	defer lock.Close()
	st, err := store.Open(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		return 0, err
	}
	defer st.Close()
	p, err := sheets.New(ctx, cfg.KeyFile, cfg.GcloudBin, cfg.GcloudAccount, cfg.SpreadsheetID)
	if err != nil {
		return 0, err
	}
	n, err := p.Push(ctx, st, 30*time.Minute)
	if err == nil {
		_ = st.SetMeta("last_push", time.Now().Format(time.RFC3339))
		if !cfg.KeepLocalText {
			_, _ = st.ScrubPushed()
		}
	}
	s, _ := st.Stats()
	status := [][2]string{
		{"last_success", st.GetMeta("last_success")},
		{"last_sync", st.GetMeta("last_sync")},
		{"last_push", st.GetMeta("last_push")},
		{"last_error", st.GetMeta("last_error")},
		{"account", st.GetMeta("account_email")},
		{"messages_in_db", strconv.Itoa(s.Messages)},
		{"conversations", strconv.Itoa(s.Conversations)},
		{"backfill_pending_conversations", strconv.Itoa(s.BackfillPending)},
		{"rows_written_last_push", strconv.Itoa(n)},
	}
	if serr := p.WriteStatus(ctx, status); serr != nil && err == nil {
		err = serr
	}
	return n, err
}

func cmdPush(ctx context.Context) error {
	n, err := doPush(ctx)
	fmt.Printf("rows written: %d\n", n)
	return err
}

// cmdRun is the launchd entry point. It runs hourly and only works when the
// interval has elapsed since the last success (so failures retry within an hour).
func cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	force := fs.Bool("force", false, "ignore the interval gate")
	_ = fs.Parse(args)
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		return err
	}
	last, _ := time.Parse(time.RFC3339, st.GetMeta("last_success"))
	st.Close()
	if !*force && time.Since(last) < time.Duration(cfg.IntervalHours)*time.Hour {
		return nil
	}
	lock, err := os.OpenFile(filepath.Join(dataDir, "run.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil // another run is in progress
	}
	_, syncErr := doSync(ctx, false, 400)
	if errors.Is(syncErr, errSyncBusy) {
		return nil
	}
	_, pushErr := doPush(ctx) // push whatever we have even if sync failed
	st, _ = store.Open(filepath.Join(dataDir, "messages.db"))
	defer st.Close()
	if err := errors.Join(syncErr, pushErr); err != nil {
		_ = st.SetMeta("last_error", time.Now().Format(time.RFC3339)+" "+err.Error())
		if errors.Is(err, gm.ErrLoggedOut) {
			notify("SMS bridge logged out: run `smsbridge pair` again")
		} else if fails := bumpFailures(st); fails >= 3 {
			notify("SMS bridge failing: " + err.Error())
		}
		return err
	}
	_ = st.SetMeta("last_success", time.Now().Format(time.RFC3339))
	_ = st.SetMeta("consecutive_failures", "0")
	_ = st.SetMeta("last_error", "")
	return nil
}

func bumpFailures(st *store.Store) int {
	n, _ := strconv.Atoi(st.GetMeta("consecutive_failures"))
	n++
	_ = st.SetMeta("consecutive_failures", strconv.Itoa(n))
	return n
}

func cmdStatus() error {
	st, err := store.Open(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	s, err := st.Stats()
	if err != nil {
		return err
	}
	fmt.Printf("messages=%d unpushed=%d conversations=%d backfill_pending=%d\nby_kind=%v by_direction=%v\n",
		s.Messages, s.Unpushed, s.Conversations, s.BackfillPending, s.ByKind, s.ByDirection)
	for _, k := range []string{"paired_at", "last_sync", "last_push", "last_success", "last_error", "consecutive_failures"} {
		fmt.Printf("%s=%s\n", k, st.GetMeta(k))
	}
	_, serr := os.Stat(filepath.Join(dataDir, "session.json"))
	fmt.Printf("session_file=%v\n", serr == nil)
	return nil
}

func notify(msg string) {
	_ = exec.Command("osascript", "-e", fmt.Sprintf("display notification %q with title \"SMS bridge\"", msg)).Run()
}

// cmdDaemon stays connected as a tablet session (coexists with Messages for
// Web), stores live messages, pushes them to the Sheet within seconds, and runs
// reconciliation + history backfill in the background (at start, then every 6h).
func cmdDaemon(ctx context.Context) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	lock, err := syncLock()
	if err != nil {
		return err
	}
	defer lock.Close()
	st, c, err := openAll(false)
	if err != nil {
		return err
	}
	defer st.Close()
	c.NewMessages = make(chan struct{}, 1)
	if err := c.Connect(ctx); err != nil {
		if errors.Is(err, gm.ErrLoggedOut) {
			_ = st.SetMeta("last_error", time.Now().Format(time.RFC3339)+" "+err.Error())
			notify("SMS bridge logged out: run `smsbridge pair` again")
			return nil // exit 0 so launchd does not restart-loop until re-paired
		}
		return err
	}
	defer c.Disconnect()
	_ = st.SetMeta("account_email", c.Email())
	_ = st.SetMeta("daemon_started", time.Now().Format(time.RFC3339))
	c.Log.Info().Msg("daemon connected")

	p, err := sheets.New(ctx, cfg.KeyFile, cfg.GcloudBin, cfg.GcloudAccount, cfg.SpreadsheetID)
	if err != nil {
		return err
	}

	// Background reconciliation + backfill: immediately, then every 6 hours.
	syncDone := make(chan error, 1)
	go func() {
		for {
			stats, err := c.Sync(ctx, 0)
			if stats != nil {
				b, _ := json.Marshal(stats)
				c.Log.Info().RawJSON("stats", b).Msg("reconcile finished")
			}
			if err != nil {
				if errors.Is(err, gm.ErrLoggedOut) || ctx.Err() != nil {
					syncDone <- err
					return
				}
				c.Log.Warn().Err(err).Msg("reconcile failed; will retry")
			} else {
				_ = st.SetMeta("last_sync", time.Now().Format(time.RFC3339))
			}
			wait := 6 * time.Hour
			if err != nil {
				wait = 10 * time.Minute
			}
			select {
			case c.NewMessages <- struct{}{}:
			default:
			}
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return
			}
		}
	}()

	pushFails := 0
	push := func() {
		pctx := ctx
		if ctx.Err() != nil {
			pctx = context.Background() // shutting down: flush with a fresh context
		}
		pctx, cancel := context.WithTimeout(pctx, 2*time.Minute)
		defer cancel()
		n, err := p.Push(pctx, st, 0)
		if err != nil {
			pushFails++
			c.Log.Warn().Err(err).Int("consecutive", pushFails).Msg("push failed")
			_ = st.SetMeta("last_error", time.Now().Format(time.RFC3339)+" push: "+err.Error())
			if pushFails == 5 || pushFails%60 == 0 { // ~10 min, then hourly
				notify("SMS bridge can't write to the Sheet: " + err.Error())
			}
			return
		}
		pushFails = 0
		if !cfg.KeepLocalText {
			if _, err := st.ScrubPushed(); err != nil {
				c.Log.Warn().Err(err).Msg("scrub failed")
			}
		}
		if n > 0 {
			c.Log.Info().Int("rows", n).Msg("pushed")
		}
		now := time.Now().Format(time.RFC3339)
		_ = st.SetMeta("last_push", now)
		_ = st.SetMeta("last_success", now)
	}
	writeStatus := func() {
		s, _ := st.Stats()
		if s == nil {
			return
		}
		_ = p.WriteStatus(ctx, [][2]string{
			{"mode", "always-on (live)"},
			{"last_success", st.GetMeta("last_success")},
			{"last_sync", st.GetMeta("last_sync")},
			{"daemon_started", st.GetMeta("daemon_started")},
			{"last_error", st.GetMeta("last_error")},
			{"messages_in_db", strconv.Itoa(s.Messages)},
			{"conversations", strconv.Itoa(s.Conversations)},
			{"backfill_pending_conversations", strconv.Itoa(s.BackfillPending)},
		})
	}
	push()
	writeStatus()
	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	safety := time.NewTicker(2 * time.Minute) // catches rows that missed a signal
	statusTick := time.NewTicker(5 * time.Minute)
	saveTick := time.NewTicker(10 * time.Minute)
	defer safety.Stop()
	defer statusTick.Stop()
	defer saveTick.Stop()
	for {
		select {
		case <-ctx.Done():
			push()
			return nil
		case <-c.NewMessages:
			debounce.Reset(3 * time.Second) // batch bursts into one append
		case <-debounce.C:
			push()
		case <-safety.C:
			push()
		case <-statusTick.C:
			writeStatus()
		case <-saveTick.C:
			_ = c.SaveSession()
		case err := <-syncDone:
			if errors.Is(err, gm.ErrLoggedOut) {
				_ = st.SetMeta("last_error", time.Now().Format(time.RFC3339)+" "+err.Error())
				writeStatus()
				notify("SMS bridge logged out: run `smsbridge pair` again")
				return nil
			}
			return err
		case err := <-c.FatalChan():
			_ = st.SetMeta("last_error", time.Now().Format(time.RFC3339)+" "+err.Error())
			writeStatus()
			if errors.Is(err, gm.ErrLoggedOut) {
				notify("SMS bridge logged out: run `smsbridge pair` again")
				return nil
			}
			return err // launchd restarts us
		}
	}
}
