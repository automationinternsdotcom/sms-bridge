// Package sheets appends captured messages to a Google Sheet using a service account.
package sheets

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/option"
	gsheets "google.golang.org/api/sheets/v4"

	"github.com/automationinternsdotcom/sms-bridge/internal/store"
)

const (
	MessagesTab = "Messages"
	StatusTab   = "Status"
	maxCell     = 49000 // Sheets cell limit is 50,000 characters
)

var header = []any{"Timestamp", "Direction", "Contact", "Number", "Conversation", "Type", "Message", "Attachments", "Status", "Conversation ID", "Row Key"}

type Pusher struct {
	SpreadsheetID string
	svc           *gsheets.Service
	tz            *time.Location
	ensured       bool
	existing      map[string]bool // cached Row Key column (long-running callers)
}

// gcloudTokens mints access tokens from the gcloud CLI login (Drive scope),
// so no service-account key is needed.
type gcloudTokens struct{ gcloud, account string }

func (g gcloudTokens) Token() (*oauth2.Token, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, g.gcloud, "auth", "print-access-token", "--account="+g.account).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("gcloud token for %s: %v: %s", g.account, err, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("gcloud token: %w", err)
	}
	// gcloud returns its own cached token, which may be close to expiry, and
	// refreshes it when needed. Re-ask every 2 minutes rather than trusting age.
	return &oauth2.Token{AccessToken: strings.TrimSpace(string(out)), Expiry: time.Now().Add(2 * time.Minute)}, nil
}

// New uses a service-account key when keyFile exists, else the gcloud login of gcloudAccount.
func New(ctx context.Context, keyFile, gcloudBin, gcloudAccount, spreadsheetID string) (*Pusher, error) {
	var opt option.ClientOption
	if key, err := os.ReadFile(keyFile); err == nil {
		opt = option.WithCredentialsJSON(key)
	} else if gcloudAccount != "" {
		opt = option.WithTokenSource(oauth2.ReuseTokenSource(nil, gcloudTokens{gcloudBin, gcloudAccount}))
	} else {
		return nil, fmt.Errorf("no service account key at %s and no gcloud_account configured", keyFile)
	}
	svc, err := gsheets.NewService(ctx, opt, option.WithScopes(gsheets.SpreadsheetsScope))
	if err != nil {
		return nil, err
	}
	return &Pusher{SpreadsheetID: spreadsheetID, svc: svc, tz: time.Local}, nil
}

// EnsureTabs creates the Messages and Status tabs and the header row if missing.
func (p *Pusher) EnsureTabs(ctx context.Context) error {
	ss, err := p.svc.Spreadsheets.Get(p.SpreadsheetID).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("open spreadsheet (is it shared with the service account?): %w", err)
	}
	have := map[string]bool{}
	for _, sh := range ss.Sheets {
		have[sh.Properties.Title] = true
	}
	var reqs []*gsheets.Request
	for _, t := range []string{MessagesTab, StatusTab} {
		if !have[t] {
			reqs = append(reqs, &gsheets.Request{AddSheet: &gsheets.AddSheetRequest{Properties: &gsheets.SheetProperties{
				Title: t, GridProperties: &gsheets.GridProperties{FrozenRowCount: 1},
			}}})
		}
	}
	if len(reqs) > 0 {
		if _, err := p.svc.Spreadsheets.BatchUpdate(p.SpreadsheetID, &gsheets.BatchUpdateSpreadsheetRequest{Requests: reqs}).Context(ctx).Do(); err != nil {
			return err
		}
	}
	got, err := p.svc.Spreadsheets.Values.Get(p.SpreadsheetID, MessagesTab+"!A1:K1").Context(ctx).Do()
	if err != nil {
		return err
	}
	if len(got.Values) == 0 {
		_, err = p.svc.Spreadsheets.Values.Update(p.SpreadsheetID, MessagesTab+"!A1:K1", &gsheets.ValueRange{Values: [][]any{header}}).
			ValueInputOption("RAW").Context(ctx).Do()
	}
	return err
}

// existingKeys reads the Row Key column so a re-push never duplicates rows.
func (p *Pusher) existingKeys(ctx context.Context) (map[string]bool, error) {
	got, err := p.svc.Spreadsheets.Values.Get(p.SpreadsheetID, MessagesTab+"!K2:K").Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	keys := make(map[string]bool, len(got.Values))
	for _, r := range got.Values {
		if len(r) > 0 {
			keys[fmt.Sprint(r[0])] = true
		}
	}
	return keys, nil
}

// Push appends unpushed rows (at least minAge old) in batches and marks them
// pushed. The Sheet's Row Key column is read once and cached. Returns rows written.
func (p *Pusher) Push(ctx context.Context, st *store.Store, minAge time.Duration) (int, error) {
	if !p.ensured {
		if err := p.EnsureTabs(ctx); err != nil {
			return 0, err
		}
		p.ensured = true
	}
	if p.existing == nil {
		keys, err := p.existingKeys(ctx)
		if err != nil {
			return 0, err
		}
		p.existing = keys
	}
	existing := p.existing
	written := 0
	for {
		rows, err := st.PushCandidates(minAge, 2000)
		if err != nil {
			return written, err
		}
		if len(rows) == 0 {
			return written, nil
		}
		var values [][]any
		var keys []string
		for _, r := range rows {
			keys = append(keys, r.RowKey)
			if existing[r.RowKey] {
				continue
			}
			existing[r.RowKey] = true
			dir := map[string]string{"in": "Received", "out": "Sent"}[r.Direction]
			values = append(values, []any{
				time.UnixMicro(r.TsUS).In(p.tz).Format("2006-01-02 15:04:05"),
				dir, clip(r.Contact), clip(r.Number), clip(r.ConvName), r.Kind, clip(r.Body), clip(r.Attachments),
				r.StatusName, r.ConvID, r.RowKey,
			})
		}
		if len(values) > 0 {
			_, err = p.svc.Spreadsheets.Values.Append(p.SpreadsheetID, MessagesTab+"!A1", &gsheets.ValueRange{Values: values}).
				ValueInputOption("RAW").InsertDataOption("INSERT_ROWS").Context(ctx).Do()
			if err != nil {
				// Ambiguous failure: drop the cache so the next push re-reads the Sheet's keys.
				p.existing = nil
				return written, fmt.Errorf("append: %w", err)
			}
			written += len(values)
		}
		if err := st.MarkPushed(keys); err != nil {
			return written, err
		}
	}
}

// WriteStatus records health information on the Status tab.
func (p *Pusher) WriteStatus(ctx context.Context, kv [][2]string) error {
	var values [][]any
	for _, r := range kv {
		values = append(values, []any{r[0], r[1]})
	}
	_, err := p.svc.Spreadsheets.Values.Update(p.SpreadsheetID, fmt.Sprintf("%s!A1:B%d", StatusTab, len(values)), &gsheets.ValueRange{Values: values}).
		ValueInputOption("RAW").Context(ctx).Do()
	return err
}

func clip(s string) string {
	if len(s) > maxCell {
		return s[:maxCell] + " …[truncated]"
	}
	return strings.ReplaceAll(s, "\x00", "")
}
