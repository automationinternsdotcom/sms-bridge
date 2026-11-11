package gm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/util"
)

// SendOutcome is the result of a send, as reported to the caller.
type SendOutcome string

const (
	// SendSent: the phone reported the message sent (or delivered).
	SendSent SendOutcome = "sent"
	// SendFailed: the phone definitely did not send it. Safe to retry.
	SendFailed SendOutcome = "failed"
	// SendUnknown: accepted but no final status yet. It may still go out, so
	// retrying could send it twice.
	SendUnknown SendOutcome = "unknown"
)

// SendResult describes one send attempt.
type SendResult struct {
	Outcome        SendOutcome `json:"outcome"`
	Detail         string      `json:"detail,omitempty"`
	ConversationID string      `json:"conversation_id,omitempty"`
	Recipient      string      `json:"recipient,omitempty"`
	MessageID      string      `json:"message_id,omitempty"`
}

var sendRetryBackoff = []time.Duration{3 * time.Second, 8 * time.Second, 20 * time.Second}

func (c *Client) handleSettings(s *gmproto.Settings) {
	if s.GetSIMCards() == nil {
		return
	}
	m := map[string]*gmproto.SIMCard{}
	for _, sim := range s.GetSIMCards() {
		m[sim.GetSIMParticipant().GetID()] = sim
	}
	c.mu.Lock()
	c.sims = m
	c.mu.Unlock()
}

// handleEcho forwards our own sent message's status updates to a waiting Send.
func (c *Client) handleEcho(m *gmproto.Message) {
	tmp := m.GetTmpID()
	if tmp == "" {
		return
	}
	c.mu.Lock()
	ch := c.pending[tmp]
	c.mu.Unlock()
	if ch != nil {
		select {
		case ch <- m:
		default:
		}
	}
}

// ResolvePhone returns the conversation for a phone number, creating it if
// needed (as Google Messages does when you start a new chat).
func (c *Client) ResolvePhone(ctx context.Context, phone string) (*gmproto.Conversation, error) {
	resp, err := c.cli.GetOrCreateConversation(ctx, &gmproto.GetOrCreateConversationRequest{
		Numbers: []*gmproto.ContactNumber{{MysteriousInt: 2, Number: phone, Number2: phone}},
	})
	if err != nil {
		return nil, err
	}
	if resp.GetConversation().GetConversationID() == "" {
		return nil, fmt.Errorf("no conversation for that number (status %s)", resp.GetStatus())
	}
	return resp.GetConversation(), nil
}

// Conversation fetches a conversation's current details from the phone.
func (c *Client) Conversation(ctx context.Context, convID string) (*gmproto.Conversation, error) {
	return c.cli.GetConversation(ctx, convID)
}

// Recipients lists the other participants of a conversation as "Name (number)".
func Recipients(conv *gmproto.Conversation) string {
	var out []string
	for _, p := range conv.GetParticipants() {
		if p.GetIsMe() || !p.GetIsVisible() {
			continue
		}
		name := firstNonEmpty(p.GetFullName(), p.GetFirstName())
		num := firstNonEmpty(p.GetFormattedNumber(), p.GetID().GetNumber())
		switch {
		case name != "" && num != "" && name != num:
			out = append(out, name+" ("+num+")")
		default:
			out = append(out, firstNonEmpty(name, num))
		}
	}
	return strings.Join(out, ", ")
}

// Attachment is a file to send with a message.
type Attachment struct {
	Data       []byte
	Name, Mime string
}

// SendText sends text (and an optional attachment) to a conversation and waits
// up to wait for the phone to report a final status. RCS chats reject a media
// message with a caption (the mautrix bridge marks captions unsupported there),
// so for RCS the attachment goes first and the text follows as its own message.
func (c *Client) SendText(ctx context.Context, conv *gmproto.Conversation, text string, att *Attachment, wait time.Duration) *SendResult {
	textInfo := func() *gmproto.MessageInfo {
		return &gmproto.MessageInfo{Data: &gmproto.MessageInfo_MessageContent{MessageContent: &gmproto.MessageContent{Content: text}}}
	}
	if att == nil {
		return c.sendInfos(ctx, conv, []*gmproto.MessageInfo{textInfo()}, wait)
	}
	media, err := c.cli.UploadMedia(ctx, att.Data, att.Name, att.Mime)
	if err != nil {
		return &SendResult{Outcome: SendFailed, Detail: "attachment upload failed: " + err.Error(),
			ConversationID: conv.GetConversationID(), Recipient: Recipients(conv)}
	}
	mediaInfo := &gmproto.MessageInfo{Data: &gmproto.MessageInfo_MediaContent{MediaContent: media}}
	if text == "" {
		return c.sendInfos(ctx, conv, []*gmproto.MessageInfo{mediaInfo}, wait)
	}
	if conv.GetType() != gmproto.ConversationType_RCS {
		return c.sendInfos(ctx, conv, []*gmproto.MessageInfo{mediaInfo, textInfo()}, wait)
	}
	res := c.sendInfos(ctx, conv, []*gmproto.MessageInfo{mediaInfo}, wait)
	if res.Outcome != SendSent {
		res.Detail = "attachment: " + res.Detail + " (caption not sent)"
		return res
	}
	tres := c.sendInfos(ctx, conv, []*gmproto.MessageInfo{textInfo()}, wait)
	tres.Detail = "attachment sent; caption: " + tres.Detail
	return tres
}

func (c *Client) sendInfos(ctx context.Context, conv *gmproto.Conversation, infos []*gmproto.MessageInfo, wait time.Duration) *SendResult {
	res := &SendResult{ConversationID: conv.GetConversationID(), Recipient: Recipients(conv)}
	outgoingID := conv.GetDefaultOutgoingID()
	c.mu.Lock()
	sim := c.sims[outgoingID]
	c.mu.Unlock()

	tmpID := util.GenerateTmpID()
	echo := make(chan *gmproto.Message, 16)
	c.mu.Lock()
	if c.pending == nil {
		c.pending = map[string]chan *gmproto.Message{}
	}
	c.pending[tmpID] = echo
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, tmpID)
		c.mu.Unlock()
	}()

	req := &gmproto.SendMessageRequest{
		ConversationID: conv.GetConversationID(),
		MessagePayload: &gmproto.MessagePayload{
			TmpID:          tmpID,
			ConversationID: conv.GetConversationID(),
			ParticipantID:  outgoingID,
			TmpID2:         tmpID,
			MessageInfo:    infos,
		},
		SIMPayload: sim.GetSIMData().GetSIMPayload(),
		TmpID:      tmpID,
	}
	resp, err := c.cli.SendMessage(ctx, req)
	// Same tmp ID on retries, as the bridge does, so the phone can dedupe.
	for attempt := 0; err == nil && isTransient(resp.GetStatus()) && attempt < len(sendRetryBackoff); attempt++ {
		select {
		case <-ctx.Done():
			res.Outcome, res.Detail = SendUnknown, "cancelled while retrying"
			return res
		case <-time.After(sendRetryBackoff[attempt]):
		}
		resp, err = c.cli.SendMessage(ctx, req)
	}
	switch {
	case errors.Is(err, libgm.ErrPhoneNotResponding):
		res.Outcome, res.Detail = SendUnknown, "phone not responding; it may still send when it comes back online"
		return res
	case err != nil:
		res.Outcome, res.Detail = SendUnknown, "request error: "+err.Error()
		return res
	case resp.GetStatus() != gmproto.SendMessageResponse_SUCCESS:
		res.Outcome, res.Detail = SendFailed, "phone rejected the send: "+resp.GetStatus().String()
		return res
	}

	// Accepted. Wait for the echo to reach a final status.
	timer := time.NewTimer(wait)
	defer timer.Stop()
	last := "accepted by phone, no status yet"
	for {
		select {
		case m := <-echo:
			res.MessageID = m.GetMessageID()
			st := m.GetMessageStatus().GetStatus()
			last = strings.ToLower(strings.TrimPrefix(st.String(), "OUTGOING_"))
			switch {
			case isSentStatus(st):
				res.Outcome, res.Detail = SendSent, last
				return res
			case isFailedStatus(st):
				res.Outcome, res.Detail = SendFailed, last
				return res
			}
		case <-timer.C:
			res.Outcome, res.Detail = SendUnknown, "timed out waiting for final status ("+last+"); it may still send"
			return res
		case <-ctx.Done():
			res.Outcome, res.Detail = SendUnknown, "cancelled while waiting ("+last+"); it may still send"
			return res
		}
	}
}

func isTransient(s gmproto.SendMessageResponse_Status) bool {
	return s == gmproto.SendMessageResponse_FAILURE_2 || s == gmproto.SendMessageResponse_FAILURE_3
}

func isSentStatus(s gmproto.MessageStatusType) bool {
	switch s {
	case gmproto.MessageStatusType_OUTGOING_COMPLETE, gmproto.MessageStatusType_OUTGOING_DELIVERED, gmproto.MessageStatusType_OUTGOING_DISPLAYED:
		return true
	}
	return false
}

func isFailedStatus(s gmproto.MessageStatusType) bool {
	name := s.String()
	return strings.HasPrefix(name, "OUTGOING_FAILED") || s == gmproto.MessageStatusType_OUTGOING_CANCELED
}
