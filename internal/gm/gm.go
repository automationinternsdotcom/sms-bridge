// Package gm wraps libgm: pairing, connecting, and syncing message history.
package gm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/exhttp"
	"google.golang.org/protobuf/proto"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/automationinternsdotcom/sms-bridge/internal/store"
)

var ErrLoggedOut = errors.New("google messages session is logged out; run `smsbridge pair` again")

type Client struct {
	SessionPath string
	Log         zerolog.Logger
	Store       *store.Store
	// NewMessages receives a signal (non-blocking) whenever a live message is stored.
	NewMessages chan struct{}

	sess   *libgm.AuthData
	cli    *libgm.Client
	mu     sync.Mutex
	active chan struct{}
	fatal  chan error
	live   int
	saveMu sync.Mutex
}

func (c *Client) loadSession() error {
	b, err := os.ReadFile(c.SessionPath)
	if err != nil {
		return err
	}
	c.sess = libgm.NewAuthData()
	return json.Unmarshal(b, c.sess)
}

// SaveSession writes the session atomically with 0600 permissions.
func (c *Client) SaveSession() error {
	c.saveMu.Lock()
	defer c.saveMu.Unlock()
	b, err := json.Marshal(c.sess)
	if err != nil {
		return err
	}
	tmp := c.SessionPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.SessionPath)
}

func (c *Client) newClient() {
	c.active = make(chan struct{}, 1)
	c.fatal = make(chan error, 1)
	c.cli = libgm.NewClient(c.sess, nil, c.Log.Level(zerolog.ErrorLevel), exhttp.SensibleClientSettings)
	c.cli.SetEventHandler(c.handleEvent)
}

// Pair runs Google-account pairing. onEmoji is called with the emoji the user
// must tap on their phone.
func (c *Client) Pair(ctx context.Context, cookies map[string]string, onEmoji func(string)) error {
	c.sess = libgm.NewAuthData()
	c.sess.SetCookies(cookies)
	c.newClient()
	if err := c.cli.DoGaiaPairing(ctx, onEmoji); err != nil {
		return err
	}
	return c.SaveSession()
}

// Connect restores the session and starts the long poll.
func (c *Client) Connect(ctx context.Context) error {
	if err := c.loadSession(); err != nil {
		return fmt.Errorf("load session (pair first): %w", err)
	}
	if !c.sess.HasCookies() || c.sess.TachyonAuthToken == nil {
		return ErrLoggedOut
	}
	c.newClient()
	if err := c.cli.FetchConfig(ctx); err != nil {
		c.Log.Warn().Err(err).Msg("fetch config failed; continuing")
	} else if c.cli.Config.GetDeviceInfo().GetEmail() == "" {
		return ErrLoggedOut
	}
	if err := c.cli.Connect(ctx); err != nil {
		if isCredErr(err) {
			return fmt.Errorf("%w (%v)", ErrLoggedOut, err)
		}
		return err
	}
	_ = c.SaveSession()
	// Wait briefly for the phone to confirm this session is active.
	select {
	case <-c.active:
	case err := <-c.fatal:
		return err
	case <-time.After(20 * time.Second):
		c.Log.Warn().Msg("no BROWSER_ACTIVE alert within 20s; continuing")
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (c *Client) Disconnect() {
	if c.cli != nil {
		c.cli.Disconnect()
	}
	if c.sess != nil {
		_ = c.SaveSession()
	}
}

func (c *Client) PairingID() string {
	if c.sess == nil {
		return ""
	}
	return c.sess.PairingID.String()
}

func (c *Client) Email() string {
	if c.cli == nil || c.cli.Config == nil {
		return ""
	}
	return c.cli.Config.GetDeviceInfo().GetEmail()
}

func isCredErr(err error) bool {
	var re events.RequestError
	if errors.As(err, &re) && re.Data != nil {
		return re.Data.Type == events.ErrInvalidCredentials.Data.Type || re.Data.Type == events.ErrRequestedEntityNotFound.Data.Type
	}
	var he events.HTTPError
	if errors.As(err, &he) && he.Resp != nil {
		return he.Resp.StatusCode == 401
	}
	return false
}

func (c *Client) handleEvent(raw any) {
	switch evt := raw.(type) {
	case *libgm.WrappedMessage:
		c.mu.Lock()
		c.live++
		c.mu.Unlock()
		if conv, _ := c.Store.GetConversation(evt.GetConversationID()); conv != nil {
			if _, err := c.storeMessage(evt.Message, conv); err != nil {
				c.Log.Err(err).Msg("store live message")
			}
			c.notifyNew()
		} else {
			// Unknown conversation: fetch its details off the event goroutine, then store.
			msg := evt.Message
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				conv, err := c.cli.GetConversation(ctx, msg.GetConversationID())
				if err != nil {
					c.Log.Warn().Err(err).Msg("fetch conversation for live message")
					return
				}
				if err := c.saveConversation(conv, "inbox"); err != nil {
					return
				}
				if sc, _ := c.Store.GetConversation(msg.GetConversationID()); sc != nil {
					_, _ = c.storeMessage(msg, sc)
					c.notifyNew()
				}
			}()
		}
	case *gmproto.Conversation:
		if sc, _ := c.Store.GetConversation(evt.GetConversationID()); sc != nil {
			_ = c.saveConversation(evt, sc.Folder)
		}
	case *gmproto.UserAlertEvent:
		switch evt.GetAlertType() {
		case gmproto.AlertType_BROWSER_ACTIVE:
			select {
			case c.active <- struct{}{}:
			default:
			}
		case gmproto.AlertType_BROWSER_INACTIVE, gmproto.AlertType_BROWSER_INACTIVE_FROM_TIMEOUT, gmproto.AlertType_BROWSER_INACTIVE_FROM_INACTIVITY:
			c.Log.Warn().Str("alert", evt.GetAlertType().String()).Msg("this session became inactive (another Messages for Web session took over)")
		}
	case *events.ListenFatalError:
		if isCredErr(evt.Error) {
			c.signalFatal(fmt.Errorf("%w (%v)", ErrLoggedOut, evt.Error))
		} else {
			c.signalFatal(fmt.Errorf("listen failed: %w", evt.Error))
		}
	case *events.GaiaLoggedOut, events.GaiaLoggedOut, *gmproto.RevokePairData:
		c.signalFatal(ErrLoggedOut)
	case *events.AuthTokenRefreshed, events.AuthTokenRefreshed:
		_ = c.SaveSession()
	case *events.PhoneNotResponding, events.PhoneNotResponding:
		c.Log.Warn().Msg("phone not responding (is it online?)")
	}
}

func (c *Client) notifyNew() {
	if c.NewMessages == nil {
		return
	}
	select {
	case c.NewMessages <- struct{}{}:
	default:
	}
}

// FatalChan exposes logout/fatal errors for long-running callers.
func (c *Client) FatalChan() <-chan error { return c.fatal }

func (c *Client) signalFatal(err error) {
	select {
	case c.fatal <- err:
	default:
	}
}

// Fatal returns a logout error if one was signalled.
func (c *Client) Fatal() error {
	select {
	case err := <-c.fatal:
		c.fatal <- err
		return err
	default:
		return nil
	}
}

type participant struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Number string `json:"number"`
	IsMe   bool   `json:"me,omitempty"`
}

func folderName(f gmproto.ListConversationsRequest_Folder) string {
	return strings.ToLower(f.String())
}

// SyncStats summarises one sync run.
type SyncStats struct {
	Conversations, Checked, NewMessages, Pages, LiveEvents, Failed int
	BackfillPending                                                int
}

// Sync enumerates every conversation and fetches messages newer than what is
// stored, then continues each conversation's history backfill.
// maxPagesBackfill bounds backfill work per run (0 = unlimited).
func (c *Client) Sync(ctx context.Context, maxPagesBackfill int) (*SyncStats, error) {
	st := &SyncStats{}
	pairing := c.PairingID()
	var convs []*gmproto.Conversation
	seen := map[string]bool{}
	for _, folder := range []gmproto.ListConversationsRequest_Folder{
		gmproto.ListConversationsRequest_INBOX, gmproto.ListConversationsRequest_ARCHIVE, gmproto.ListConversationsRequest_SPAM_BLOCKED,
	} {
		var cursor *gmproto.Cursor
		for page := 0; page < 200; page++ {
			resp, err := c.cli.ListConversations(ctx, &gmproto.ListConversationsRequest{Count: 100, Folder: folder, Cursor: cursor})
			if err != nil {
				if isCredErr(err) {
					return st, fmt.Errorf("%w (%v)", ErrLoggedOut, err)
				}
				return st, fmt.Errorf("list %s: %w", folderName(folder), err)
			}
			added := 0
			for _, conv := range resp.GetConversations() {
				if seen[conv.GetConversationID()] {
					continue
				}
				seen[conv.GetConversationID()] = true
				added++
				convs = append(convs, conv)
				if err := c.saveConversation(conv, folderName(folder)); err != nil {
					return st, err
				}
			}
			c.Log.Info().Str("folder", folderName(folder)).Int("page", page).Int("new", added).Msg("listed conversations")
			if resp.Cursor == nil || added == 0 || len(resp.GetConversations()) < 100 {
				break
			}
			cursor = resp.Cursor
		}
	}
	st.Conversations = len(convs)

	backfillBudget := maxPagesBackfill
	headFailed := map[string]bool{}
	for _, conv := range convs {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		if err := c.Fatal(); err != nil {
			return st, err
		}
		sc, err := c.Store.GetConversation(conv.GetConversationID())
		if err != nil {
			return st, err
		}
		needHead := conv.GetLastMessageTimestamp() > sc.SyncedToUS
		if needHead {
			st.Checked++
			n, pages, oldest, err := c.syncHead(ctx, pairing, sc)
			st.NewMessages += n
			st.Pages += pages
			if err != nil {
				if errors.Is(err, ErrLoggedOut) || ctx.Err() != nil {
					return st, err
				}
				st.Failed++
				headFailed[sc.ConvID] = true
				c.Log.Warn().Err(err).Str("conv", sc.ConvID).Msg("conversation head sync failed; skipping")
				continue
			}
			if err := c.Store.SetSyncedTo(sc.ConvID, conv.GetLastMessageTimestamp()); err != nil {
				return st, err
			}
			// First time we see this conversation: backfill continues from where head stopped.
			if !sc.BackfillDone && sc.BackfillCursorID == "" && oldest != nil {
				_ = c.Store.SetBackfill(sc.ConvID, false, oldest.LastItemID, oldest.LastItemTimestamp, 0)
				sc, _ = c.Store.GetConversation(sc.ConvID)
			}
		}
	}
	// Pass 2: history backfill, newest conversations first, within the budget.
	for _, conv := range convs {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		if err := c.Fatal(); err != nil {
			return st, err
		}
		if maxPagesBackfill != 0 && backfillBudget <= 0 {
			break
		}
		sc, err := c.Store.GetConversation(conv.GetConversationID())
		if err != nil {
			return st, err
		}
		if sc.BackfillDone || headFailed[sc.ConvID] {
			continue // a failed head must be retried before backfill, or the gap could be sealed
		}
		n, pages, err := c.backfill(ctx, pairing, sc, &backfillBudget, maxPagesBackfill == 0)
		st.NewMessages += n
		st.Pages += pages
		if err != nil {
			if errors.Is(err, ErrLoggedOut) || ctx.Err() != nil {
				return st, err
			}
			st.Failed++
			c.Log.Warn().Err(err).Str("conv", sc.ConvID).Msg("conversation backfill failed; skipping")
		}
	}
	c.mu.Lock()
	st.LiveEvents = c.live
	c.mu.Unlock()
	s, _ := c.Store.Stats()
	if s != nil {
		st.BackfillPending = s.BackfillPending
	}
	if st.Failed > 0 {
		return st, fmt.Errorf("%d conversation(s) failed to sync; will retry", st.Failed)
	}
	return st, nil
}

func (c *Client) saveConversation(conv *gmproto.Conversation, folder string) error {
	var parts []participant
	for _, p := range conv.GetParticipants() {
		parts = append(parts, participant{
			ID: p.GetID().GetParticipantID(), Name: firstNonEmpty(p.GetFullName(), p.GetFirstName()),
			Number: firstNonEmpty(p.GetID().GetNumber(), p.GetFormattedNumber()), IsMe: p.GetIsMe(),
		})
	}
	pj, _ := json.Marshal(parts)
	ctype := "SMS"
	if conv.GetType() == gmproto.ConversationType_RCS {
		ctype = "RCS"
	}
	return c.Store.UpsertConversationInfo(&store.Conversation{
		ConvID: conv.GetConversationID(), Name: conv.GetName(), ConvType: ctype, IsGroup: conv.GetIsGroupChat(),
		Folder: folder, Participants: string(pj), LastMsgUS: conv.GetLastMessageTimestamp(),
	})
}

const pageSize = 50

// syncHead pages from newest backwards until it reaches already-synced history
// (a fully known page at or below the conversation's watermark).
func (c *Client) syncHead(ctx context.Context, pairing string, sc *store.Conversation) (newMsgs, pages int, oldest *gmproto.Cursor, err error) {
	var cursor *gmproto.Cursor
	for pages < 1000 {
		resp, err := c.fetch(ctx, sc.ConvID, cursor)
		if err != nil {
			return newMsgs, pages, oldest, err
		}
		pages++
		msgs := resp.GetMessages()
		if len(msgs) == 0 {
			if cursor == nil || sc.BackfillCursorID == "" {
				_ = c.Store.SetBackfill(sc.ConvID, true, "", 0, resp.GetTotalMessages())
				sc.BackfillDone = true
			}
			return newMsgs, pages, nil, nil
		}
		known := 0
		for _, m := range msgs {
			if c.Store.Known(sc.ConvID, m.GetMessageID()) {
				known++
			}
			if isNew, err := c.storeMessage(m, sc); err != nil {
				return newMsgs, pages, oldest, err
			} else if isNew {
				newMsgs++
			}
		}
		oldest = nextCursor(resp, msgs)
		minTS := msgs[0].GetTimestamp()
		for _, m := range msgs {
			if m.GetTimestamp() < minTS {
				minTS = m.GetTimestamp()
			}
		}
		// A fully known page only ends the pass once it reaches what was already
		// synced; live-stored messages above a gap must not hide the gap.
		if oldest == nil || (known == len(msgs) && minTS <= sc.SyncedToUS) {
			return newMsgs, pages, oldest, nil
		}
		// New conversation: take only the newest page here; older history is
		// fetched by the budgeted backfill so every conversation gets recent rows first.
		if sc.SyncedToUS == 0 && sc.BackfillCursorID == "" {
			return newMsgs, pages, oldest, nil
		}
		// Reached the region the backfill already covers.
		if sc.BackfillCursorID != "" && oldest.LastItemTimestamp <= sc.BackfillCursorMS {
			return newMsgs, pages, oldest, nil
		}
		cursor = oldest
		sleep(ctx)
	}
	return newMsgs, pages, oldest, nil
}

func (c *Client) backfill(ctx context.Context, pairing string, sc *store.Conversation, budget *int, unlimited bool) (newMsgs, pages int, err error) {
	var cursor *gmproto.Cursor
	if sc.BackfillCursorID != "" {
		cursor = &gmproto.Cursor{LastItemID: sc.BackfillCursorID, LastItemTimestamp: sc.BackfillCursorMS}
	}
	for unlimited || *budget > 0 {
		resp, err := c.fetch(ctx, sc.ConvID, cursor)
		if err != nil {
			return newMsgs, pages, err
		}
		pages++
		*budget--
		msgs := resp.GetMessages()
		if len(msgs) == 0 {
			return newMsgs, pages, c.Store.SetBackfill(sc.ConvID, true, "", 0, resp.GetTotalMessages())
		}
		for _, m := range msgs {
			if isNew, err := c.storeMessage(m, sc); err != nil {
				return newMsgs, pages, err
			} else if isNew {
				newMsgs++
			}
		}
		next := nextCursor(resp, msgs)
		if next == nil || (cursor != nil && next.LastItemID == cursor.LastItemID) {
			return newMsgs, pages, c.Store.SetBackfill(sc.ConvID, true, "", 0, resp.GetTotalMessages())
		}
		if err := c.Store.SetBackfill(sc.ConvID, false, next.LastItemID, next.LastItemTimestamp, resp.GetTotalMessages()); err != nil {
			return newMsgs, pages, err
		}
		cursor = next
		sleep(ctx)
	}
	return newMsgs, pages, nil
}

func (c *Client) fetch(ctx context.Context, convID string, cursor *gmproto.Cursor) (*gmproto.ListMessagesResponse, error) {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		fctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		resp, err := c.cli.FetchMessages(fctx, convID, pageSize, cursor)
		cancel()
		if err == nil {
			return resp, nil
		}
		if isCredErr(err) {
			return nil, fmt.Errorf("%w (%v)", ErrLoggedOut, err)
		}
		lastErr = err
		c.Log.Warn().Err(err).Str("conv", convID).Int("attempt", attempt).Msg("fetch failed, retrying")
		select {
		case <-time.After(time.Duration(2<<attempt) * time.Second):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("fetch %s: %w", convID, lastErr)
}

// nextCursor mirrors the bridge: use the response cursor, else the oldest message.
func nextCursor(resp *gmproto.ListMessagesResponse, msgs []*gmproto.Message) *gmproto.Cursor {
	if resp.Cursor != nil && resp.Cursor.LastItemID != "" {
		return resp.Cursor
	}
	var oldest *gmproto.Message
	for _, m := range msgs {
		if oldest == nil || m.GetTimestamp() < oldest.GetTimestamp() {
			oldest = m
		}
	}
	if oldest == nil {
		return nil
	}
	return &gmproto.Cursor{LastItemID: oldest.GetMessageID(), LastItemTimestamp: oldest.GetTimestamp() / 1000}
}

func sleep(ctx context.Context) {
	select {
	case <-time.After(400 * time.Millisecond):
	case <-ctx.Done():
	}
}

func (c *Client) storeMessage(m *gmproto.Message, sc *store.Conversation) (bool, error) {
	var parts []participant
	_ = json.Unmarshal([]byte(sc.Participants), &parts)
	status := m.GetMessageStatus().GetStatus()
	code := int(status)
	dir := "in"
	switch {
	case code >= 1 && code < 100:
		dir = "out"
	case code >= 100 && code < 200:
		dir = "in"
	case code >= 200:
		dir = "system"
	default:
		if m.GetParticipantID() == "1" {
			dir = "out"
		}
	}
	senderName, senderNumber := "", ""
	for _, p := range parts {
		if p.ID == m.GetParticipantID() {
			senderName, senderNumber = p.Name, p.Number
			if p.IsMe && dir == "in" && code == 0 {
				dir = "out"
			}
		}
	}
	if dir == "out" {
		// For outgoing, "contact" is who it was sent to.
		var names, nums []string
		for _, p := range parts {
			if !p.IsMe && p.ID != "1" {
				names = append(names, p.Name)
				nums = append(nums, p.Number)
			}
		}
		senderName, senderNumber = strings.Join(names, ", "), strings.Join(nums, ", ")
	}
	var texts, atts []string
	if s := m.GetSubject(); s != "" {
		texts = append(texts, s)
	}
	for _, info := range m.GetMessageInfo() {
		if mc := info.GetMessageContent(); mc != nil && mc.GetContent() != "" {
			texts = append(texts, mc.GetContent())
		}
		if md := info.GetMediaContent(); md != nil {
			atts = append(atts, strings.TrimSpace(firstNonEmpty(md.GetMimeType(), md.GetFormat().String())+" "+md.GetMediaName()))
		}
		if lc := info.GetLocationContent(); lc != nil {
			atts = append(atts, "location")
		}
	}
	kind := sc.ConvType
	if kind == "SMS" && (len(atts) > 0 || sc.IsGroup || m.GetSubject() != "") {
		kind = "MMS"
	}
	raw, _ := proto.Marshal(m)
	return c.Store.Upsert(&store.Message{
		PairingID: c.PairingID(), MessageID: m.GetMessageID(), ConvID: m.GetConversationID(), TsUS: m.GetTimestamp(),
		Direction: dir, SenderID: m.GetParticipantID(), SenderName: senderName, SenderNumber: senderNumber,
		Body: strings.Join(texts, "\n"), Attachments: strings.Join(atts, "; "), Kind: kind,
		Status: code, StatusName: strings.ToLower(strings.TrimPrefix(status.String(), "MESSAGE_STATUS_")), Raw: raw,
	})
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
