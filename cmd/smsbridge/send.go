package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/automationinternsdotcom/sms-bridge/internal/gm"
	"github.com/automationinternsdotcom/sms-bridge/internal/store"
)

// The daemon owns the only Google Messages connection, so sends go through it
// over a Unix socket in the (0700) data dir.
func sockPath() string { return filepath.Join(dataDir, "send.sock") }

const (
	daemonSendWait = 90 * time.Second  // daemon waits this long for a final status
	clientTimeout  = 150 * time.Second // must exceed the daemon's total work
)

type sendRequest struct {
	ConversationID string `json:"conversation_id,omitempty"`
	Phone          string `json:"phone,omitempty"`
	Text           string `json:"text"`
	AttachPath     string `json:"attach_path,omitempty"`
	DryRun         bool   `json:"dry_run,omitempty"`
	AllowGroup     bool   `json:"allow_group,omitempty"`
}

type sendReply struct {
	gm.SendResult
	Error string `json:"error,omitempty"`
	Group bool   `json:"group,omitempty"`
}

// serveSend listens for send requests until ctx ends. Requests are handled one
// at a time.
func serveSend(ctx context.Context, c *gm.Client) error {
	_ = os.Remove(sockPath())
	ln, err := net.Listen("unix", sockPath())
	if err != nil {
		return err
	}
	_ = os.Chmod(sockPath(), 0o600)
	go func() { <-ctx.Done(); ln.Close() }()
	go func() {
		defer os.Remove(sockPath())
		for {
			conn, err := ln.Accept()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				c.Log.Warn().Err(err).Msg("send socket accept")
				time.Sleep(time.Second)
				continue
			}
			handleSendConn(ctx, c, conn)
		}
	}()
	return nil
}

func handleSendConn(ctx context.Context, c *gm.Client, conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(clientTimeout))
	var req sendRequest
	reply := func(r *sendReply) { _ = json.NewEncoder(conn).Encode(r) }
	if err := json.NewDecoder(io.LimitReader(conn, 1<<20)).Decode(&req); err != nil {
		reply(&sendReply{Error: "bad request: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.Text) == "" && req.AttachPath == "" && !req.DryRun {
		reply(&sendReply{Error: "empty text"})
		return
	}
	var att *gm.Attachment
	if req.AttachPath != "" {
		a, err := loadAttachment(req.AttachPath)
		if err != nil {
			reply(&sendReply{Error: err.Error()})
			return
		}
		att = a
	}
	rctx, cancel := context.WithTimeout(ctx, clientTimeout-10*time.Second)
	defer cancel()
	var cv *gmproto.Conversation
	var err error
	if req.ConversationID != "" {
		cv, err = c.Conversation(rctx, req.ConversationID)
	} else {
		cv, err = c.ResolvePhone(rctx, req.Phone)
	}
	if err != nil {
		reply(&sendReply{Error: "resolve recipient: " + err.Error()})
		return
	}
	out := &sendReply{Group: cv.GetIsGroupChat()}
	out.ConversationID, out.Recipient = cv.GetConversationID(), gm.Recipients(cv)
	if cv.GetIsGroupChat() && !req.AllowGroup {
		out.Error = "that is a group conversation; pass -allow-group to send to it"
		reply(out)
		return
	}
	if req.DryRun {
		out.Outcome = "dry-run"
		reply(out)
		return
	}
	c.Log.Info().Str("conv", cv.GetConversationID()).Msg("sending text") // no body or number in logs
	res := c.SendText(rctx, cv, req.Text, att, daemonSendWait)
	c.Log.Info().Str("conv", cv.GetConversationID()).Str("outcome", string(res.Outcome)).Str("detail", res.Detail).Msg("send finished")
	out.SendResult = *res
	reply(out)
}

const maxAttachment = 25 << 20

func loadAttachment(path string) (*gm.Attachment, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("attachment path must be absolute")
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("attachment: %w", err)
	}
	if !fi.Mode().IsRegular() || fi.Size() > maxAttachment {
		return nil, fmt.Errorf("attachment must be a regular file under %d MB", maxAttachment>>20)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("attachment: %w", err)
	}
	mime := http.DetectContentType(data)
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = mime[:i]
	}
	return &gm.Attachment{Data: data, Name: filepath.Base(path), Mime: mime}, nil
}

var phoneRe = regexp.MustCompile(`^\+?[0-9]{7,15}$`)

// normalizePhone strips formatting and assumes US (+1) for bare 10-digit numbers.
func normalizePhone(s string) (string, error) {
	s = strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' || r == '(' || r == ')' || r == '.' {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if !phoneRe.MatchString(s) {
		return "", fmt.Errorf("%q is not a phone number", s)
	}
	if !strings.HasPrefix(s, "+") {
		switch {
		case len(s) == 10:
			s = "+1" + s
		case len(s) == 11 && s[0] == '1':
			s = "+" + s
		default:
			return "", fmt.Errorf("add a country code (+...) to %q", s)
		}
	}
	return s, nil
}

// Exit codes for `send`, so callers can tell outcomes apart.
const (
	exitSent    = 0
	exitFailed  = 1 // definitely not sent; safe to fix and retry
	exitUnknown = 3 // may still be sent; do NOT retry automatically
)

func cmdSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	conv := fs.String("conv", "", "exact conversation ID (from `smsbridge lookup`)")
	phone := fs.String("to", "", "phone number, e.g. +14805551234")
	textArg := fs.String("text", "", "message text (default: read from stdin)")
	attach := fs.String("attach", "", "image or file to send (with optional text as a caption)")
	dry := fs.Bool("dry-run", false, "resolve the recipient and print it without sending")
	group := fs.Bool("allow-group", false, "allow sending to a group conversation")
	_ = fs.Parse(args)
	if (*conv == "") == (*phone == "") {
		return errors.New("give exactly one of -conv ID or -to PHONE")
	}
	req := sendRequest{ConversationID: *conv, DryRun: *dry, AllowGroup: *group}
	if *phone != "" {
		p, err := normalizePhone(*phone)
		if err != nil {
			return err
		}
		req.Phone = p
	}
	req.Text = *textArg
	if *attach != "" {
		abs, err := filepath.Abs(*attach)
		if err != nil {
			return err
		}
		if _, err := loadAttachment(abs); err != nil {
			return err
		}
		req.AttachPath = abs
	}
	if req.Text == "" && !*dry && req.AttachPath == "" {
		b, err := io.ReadAll(io.LimitReader(os.Stdin, 64<<10))
		if err != nil {
			return err
		}
		req.Text = strings.TrimRight(string(b), "\n")
	}
	if strings.TrimSpace(req.Text) == "" && !*dry && req.AttachPath == "" {
		return errors.New("no message text (pass -text, pipe it on stdin, or -attach a file)")
	}
	conn, err := net.DialTimeout("unix", sockPath(), 5*time.Second)
	if err != nil {
		return fmt.Errorf("the smsbridge daemon isn't reachable (%v); check `launchctl print gui/$(id -u)/%s`", err, launchLabel())
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(clientTimeout))
	if err := json.NewEncoder(conn).Encode(&req); err != nil {
		return err
	}
	var rep sendReply
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&rep); err != nil {
		if *dry {
			return err
		}
		fmt.Println(`{"outcome":"unknown","detail":"lost contact with the daemon; the text may still send"}`)
		os.Exit(exitUnknown)
	}
	b, _ := json.Marshal(rep)
	fmt.Println(string(b))
	switch {
	case rep.Error != "":
		os.Exit(exitFailed)
	case rep.Outcome == gm.SendUnknown:
		os.Exit(exitUnknown)
	case rep.Outcome == gm.SendFailed:
		os.Exit(exitFailed)
	}
	return nil
}

func cmdLookup(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: smsbridge lookup NAME-OR-NUMBER")
	}
	q := strings.TrimSpace(strings.Join(args, " "))
	st, err := store.Open(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	term := q
	if digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, q); len(digits) >= 7 {
		term = digits[max(0, len(digits)-10):]
	}
	convs, err := st.FindConversations(term, 15)
	if err != nil {
		return err
	}
	type part struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Number string `json:"number"`
		Me     bool   `json:"me,omitempty"`
	}
	type row struct {
		ConversationID string   `json:"conversation_id"`
		Name           string   `json:"name"`
		Group          bool     `json:"group"`
		Type           string   `json:"type"`
		Participants   []string `json:"participants"`
		LastActive     string   `json:"last_active,omitempty"`
	}
	var rows []row
	for _, c := range convs {
		var ps []part
		_ = json.Unmarshal([]byte(c.Participants), &ps)
		// Hide Jon's own entries: the "me" participant, the local self ID "1",
		// and other self/SIM entries that carry the same name.
		self := map[string]bool{}
		for _, p := range ps {
			if p.Me && p.Name != "" {
				self[p.Name] = true
			}
		}
		var names []string
		for _, p := range ps {
			if p.Me || p.ID == "1" || self[p.Name] {
				continue
			}
			names = append(names, strings.TrimSpace(p.Name+" "+p.Number))
		}
		r := row{ConversationID: c.ConvID, Name: c.Name, Group: c.IsGroup, Type: c.ConvType, Participants: names}
		if c.LastMsgUS > 0 {
			r.LastActive = time.UnixMicro(c.LastMsgUS).Format("2006-01-02")
		}
		rows = append(rows, r)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
}
