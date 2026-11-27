package platform

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/smtp"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// ---- senders -------------------------------------------------------------------------------

// OutMessage is one message to one person.
type OutMessage struct {
	Channel, To, Subject, Text string
}

// Sender delivers messages over one channel. A nil Sender in Senders means
// "development mode" for that channel: messages are recorded (visible in the
// operator console) but NOT delivered.
type Sender interface {
	Name() string
	Send(ctx context.Context, m OutMessage) error
}

type Senders struct {
	Email, SMS, WhatsApp Sender
}

func (s Senders) For(channel string) Sender {
	switch channel {
	case "email":
		return s.Email
	case "sms":
		return s.SMS
	case "whatsapp":
		return s.WhatsApp
	}
	return nil
}

var httpClient = &http.Client{Timeout: 20 * time.Second}

// SMTPSender sends e-mail through any SMTP server (STARTTLS when offered).
type SMTPSender struct {
	Addr, User, Pass, From string // Addr "smtp.example.com:587"
}

func (s *SMTPSender) Name() string { return "smtp" }
func (s *SMTPSender) Send(ctx context.Context, m OutMessage) error {
	host := s.Addr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	var auth smtp.Auth
	if s.User != "" {
		auth = smtp.PlainAuth("", s.User, s.Pass, host)
	}
	from := s.From
	addr := from
	if i := strings.Index(from, "<"); i >= 0 && strings.HasSuffix(from, ">") {
		addr = from[i+1 : len(from)-1]
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n",
		from, m.To, mime.QEncoding.Encode("utf-8", m.Subject), time.Now().Format(time.RFC1123Z))
	b.WriteString(strings.ReplaceAll(m.Text, "\n", "\r\n"))
	return smtp.SendMail(s.Addr, auth, addr, []string{m.To}, b.Bytes())
}

// AfricasTalkingSender sends SMS through Africa's Talking (widely used in East Africa).
type AfricasTalkingSender struct {
	Username, APIKey, From string
	BaseURL                string // default https://api.africastalking.com (sandbox: https://api.sandbox.africastalking.com)
}

func (s *AfricasTalkingSender) Name() string { return "africastalking" }
func (s *AfricasTalkingSender) Send(ctx context.Context, m OutMessage) error {
	base := s.BaseURL
	if base == "" {
		base = "https://api.africastalking.com"
	}
	f := url.Values{"username": {s.Username}, "to": {m.To}, "message": {m.Text}}
	if s.From != "" {
		f.Set("from", s.From)
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(base, "/")+"/version1/messaging", strings.NewReader(f.Encode()))
	req.Header.Set("apiKey", s.APIKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if res.StatusCode >= 300 {
		return fmt.Errorf("africastalking HTTP %d: %.200s", res.StatusCode, body)
	}
	var out struct {
		SMSMessageData struct {
			Recipients []struct {
				Status     string `json:"status"`
				StatusCode int    `json:"statusCode"`
			} `json:"Recipients"`
		} `json:"SMSMessageData"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return fmt.Errorf("africastalking: unexpected reply %.200s", body)
	}
	for _, r := range out.SMSMessageData.Recipients {
		if r.StatusCode >= 100 && r.StatusCode <= 102 {
			return nil
		}
		return fmt.Errorf("africastalking: %s (%d)", r.Status, r.StatusCode)
	}
	return fmt.Errorf("africastalking: no recipient in reply %.200s", body)
}

// WhatsAppSender sends through the WhatsApp Business Cloud API (Meta).
// Messages a business starts must use an APPROVED TEMPLATE; set Template to a
// template whose body has one parameter ({{1}}), which receives the text.
type WhatsAppSender struct {
	Token, PhoneNumberID, Template, Lang string
	BaseURL                              string // default https://graph.facebook.com/v20.0
}

func (s *WhatsAppSender) Name() string { return "whatsapp-cloud" }
func (s *WhatsAppSender) Send(ctx context.Context, m OutMessage) error {
	base := s.BaseURL
	if base == "" {
		base = "https://graph.facebook.com/v20.0"
	}
	to := strings.TrimPrefix(m.To, "+")
	var payload map[string]any
	if s.Template != "" {
		lang := s.Lang
		if lang == "" {
			lang = "en"
		}
		// template parameters may not contain new lines or tabs
		text := strings.Join(strings.Fields(m.Text), " ")
		payload = map[string]any{"messaging_product": "whatsapp", "to": to, "type": "template",
			"template": map[string]any{"name": s.Template, "language": map[string]string{"code": lang},
				"components": []any{map[string]any{"type": "body", "parameters": []any{map[string]string{"type": "text", "text": text}}}}}}
	} else {
		payload = map[string]any{"messaging_product": "whatsapp", "to": to, "type": "text", "text": map[string]string{"body": m.Text}}
	}
	js, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(base, "/")+"/"+s.PhoneNumberID+"/messages", bytes.NewReader(js))
	req.Header.Set("Authorization", "Bearer "+s.Token)
	req.Header.Set("Content-Type", "application/json")
	res, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if res.StatusCode >= 300 {
		return fmt.Errorf("whatsapp HTTP %d: %.200s", res.StatusCode, body)
	}
	return nil
}

// ---- development outbox ------------------------------------------------------------------------

// DevMessage is a message the development sender recorded instead of sending.
type DevMessage struct {
	At      int64  `json:"at"`
	Slug    string `json:"slug"`
	Kind    string `json:"kind"`
	Channel string `json:"channel"`
	To      string `json:"to"`
	Subject string `json:"subject,omitempty"`
	Text    string `json:"text"`
}

func (p *Platform) DevMessages() []DevMessage {
	p.devMu.Lock()
	defer p.devMu.Unlock()
	out := make([]DevMessage, len(p.devLog))
	for i := range p.devLog {
		out[len(p.devLog)-1-i] = p.devLog[i]
	}
	return out
}

// Delivering reports per channel whether messages are really delivered (a
// provider is configured) or only recorded for development.
func (p *Platform) Delivering() map[string]string {
	out := map[string]string{}
	for _, ch := range []string{"sms", "whatsapp", "email"} {
		if s := p.cfg.Senders.For(ch); s != nil {
			out[ch] = s.Name()
		} else {
			out[ch] = "development (not delivered)"
		}
	}
	return out
}

// ---- usage, caps and the outbox ----------------------------------------------------------------

// SMSSegments is how many SMS a text costs (160 GSM characters, or 70 when it
// needs Unicode; long messages are 153 / 67 per part).
func SMSSegments(text string) int {
	gsm := true
	for _, r := range text {
		if r > 127 || r == '`' {
			gsm = false
			break
		}
	}
	n := utf8.RuneCountInString(text)
	one, part := 160, 153
	if !gsm {
		one, part = 70, 67
	}
	if n <= one {
		return 1
	}
	return (n + part - 1) / part
}

func units(channel, text string) int {
	if channel == "sms" {
		return SMSSegments(text)
	}
	return 1
}

func (p *Platform) month() string { return p.now().UTC().Format("2006-01") }

type Limits struct {
	SMSMonthCap int `json:"sms_month_cap"`
	WAMonthCap  int `json:"wa_month_cap"`
}

type Usage struct {
	Month       string `json:"month"`
	SMSUsed     int    `json:"sms_used"`
	SMSQueued   int    `json:"sms_queued"`
	SMSCap      int    `json:"sms_cap"`
	WAUsed      int    `json:"wa_used"`
	WAQueued    int    `json:"wa_queued"`
	WACap       int    `json:"wa_cap"`
	EmailSent   int    `json:"email_sent"`
	EmailQueued int    `json:"email_queued"`
}

type querier interface {
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

func (p *Platform) usage(ctx context.Context, q querier, slug string) (Usage, error) {
	u := Usage{Month: p.month()}
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(MAX(sms_month_cap),0), COALESCE(MAX(wa_month_cap),0) FROM shop_limits WHERE slug=?`, slug).Scan(&u.SMSCap, &u.WACap); err != nil {
		return u, err
	}
	get := func(ch string, used, queued *int) error {
		if err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(units),0) FROM usage WHERE slug=? AND month=? AND channel=?`, slug, u.Month, ch).Scan(used); err != nil {
			return err
		}
		return q.QueryRowContext(ctx, `SELECT COALESCE(SUM(units),0) FROM outbox WHERE slug=? AND channel=? AND status='queued'`, slug, ch).Scan(queued)
	}
	if err := get("sms", &u.SMSUsed, &u.SMSQueued); err != nil {
		return u, err
	}
	if err := get("whatsapp", &u.WAUsed, &u.WAQueued); err != nil {
		return u, err
	}
	err := get("email", &u.EmailSent, &u.EmailQueued)
	return u, err
}

func (p *Platform) Usage(ctx context.Context, slug string) (Usage, error) {
	return p.usage(ctx, p.r, slug)
}

func (u Usage) remaining(ch string) int {
	switch ch {
	case "sms":
		return max(0, u.SMSCap-u.SMSUsed-u.SMSQueued)
	case "whatsapp":
		return max(0, u.WACap-u.WAUsed-u.WAQueued)
	}
	return 1 << 30
}

// Channels says which channels a shop can use right now (paid ones need a cap).
func (p *Platform) Channels(ctx context.Context, slug string) (map[string]bool, error) {
	u, err := p.Usage(ctx, slug)
	if err != nil {
		return nil, err
	}
	return map[string]bool{"email": true, "sms": u.remaining("sms") > 0, "whatsapp": u.remaining("whatsapp") > 0}, nil
}

func (p *Platform) SetLimits(ctx context.Context, slug string, l Limits) error {
	if l.SMSMonthCap < 0 || l.WAMonthCap < 0 || l.SMSMonthCap > 1_000_000 || l.WAMonthCap > 1_000_000 {
		return bad("limits", "caps must be between 0 and 1,000,000 per month")
	}
	_, err := p.w.ExecContext(ctx, `INSERT INTO shop_limits(slug,sms_month_cap,wa_month_cap) VALUES(?,?,?)
		ON CONFLICT(slug) DO UPDATE SET sms_month_cap=excluded.sms_month_cap, wa_month_cap=excluded.wa_month_cap`, slug, l.SMSMonthCap, l.WAMonthCap)
	return err
}

// queueTx adds one message to the outbox inside tx, refusing if a paid
// channel would go over the shop's monthly cap (the shop pays for SMS/WhatsApp).
func (p *Platform) queueTx(ctx context.Context, tx *sql.Tx, slug, kind, channel, to, subject, text string, broadcastID *int64) error {
	n := units(channel, text)
	if channel != "email" {
		u, err := p.usage(ctx, tx, slug)
		if err != nil {
			return err
		}
		if u.remaining(channel) < n {
			return ErrChannelUnavailable
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO outbox(slug,kind,channel,to_addr,subject,body,units,broadcast_id,next_try_at,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		slug, kind, channel, to, subject, text, n, broadcastID, p.now().Unix(), p.now().Unix())
	return err
}

// QueueEmail sends a system e-mail (shop welcome, password reset). Free; no cap.
func (p *Platform) QueueEmail(ctx context.Context, slug, kind, to, subject, text string) error {
	tx, err := p.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := p.queueTx(ctx, tx, slug, kind, "email", to, subject, text, nil); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	p.Kick()
	return nil
}

// Kick wakes the delivery worker.
func (p *Platform) Kick() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

// RunWorker delivers queued messages until ctx ends, retrying failures with
// back-off (up to 6 attempts). Successful SMS/WhatsApp count against the
// shop's monthly usage.
func (p *Platform) RunWorker(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		p.deliverDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-p.kick:
		}
	}
}

type outRow struct {
	id                                     int64
	slug, kind, channel, to, subject, body string
	units, attempts                        int
}

func (p *Platform) deliverDue(ctx context.Context) {
	rows, err := p.r.QueryContext(ctx, `SELECT id,slug,kind,channel,to_addr,subject,body,units,attempts FROM outbox WHERE status='queued' AND next_try_at<=? ORDER BY id LIMIT 50`, p.now().Unix())
	if err != nil {
		return
	}
	var due []outRow
	for rows.Next() {
		var o outRow
		if rows.Scan(&o.id, &o.slug, &o.kind, &o.channel, &o.to, &o.subject, &o.body, &o.units, &o.attempts) == nil {
			due = append(due, o)
		}
	}
	rows.Close()
	for _, o := range due {
		if ctx.Err() != nil {
			return
		}
		status, errText := "sent", ""
		if s := p.cfg.Senders.For(o.channel); s != nil {
			sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := s.Send(sctx, OutMessage{Channel: o.channel, To: o.to, Subject: o.subject, Text: o.body})
			cancel()
			if err != nil {
				errText = err.Error()
				if len(errText) > 300 {
					errText = errText[:300]
				}
				status = "queued"
				if o.attempts+1 >= 6 {
					status = "failed"
				}
			}
		} else {
			status = "logged"
			p.devMu.Lock()
			p.devLog = append(p.devLog, DevMessage{At: p.now().Unix(), Slug: o.slug, Kind: o.kind, Channel: o.channel, To: o.to, Subject: o.subject, Text: o.body})
			if len(p.devLog) > 200 {
				p.devLog = p.devLog[len(p.devLog)-200:]
			}
			p.devMu.Unlock()
			p.log.Info("message recorded (development mode, not delivered)", "channel", o.channel, "to", Mask(o.to), "kind", o.kind)
		}
		p.mu.Lock()
		tx, err := p.w.BeginTx(ctx, nil)
		if err == nil {
			next := p.now().Add(time.Duration(30<<o.attempts) * time.Second).Unix()
			tx.ExecContext(ctx, `UPDATE outbox SET status=?, attempts=attempts+1, last_error=?, next_try_at=?, sent_at=CASE WHEN ? IN ('sent','logged') THEN ? ELSE sent_at END WHERE id=?`,
				status, errText, next, status, p.now().Unix(), o.id)
			if (status == "sent" || status == "logged") && o.slug != "" {
				tx.ExecContext(ctx, `INSERT INTO usage(slug,month,channel,units) VALUES(?,?,?,?) ON CONFLICT(slug,month,channel) DO UPDATE SET units=units+excluded.units`,
					o.slug, p.month(), o.channel, o.units)
			}
			tx.Commit()
		}
		p.mu.Unlock()
		if errText != "" {
			p.log.Warn("message delivery failed", "channel", o.channel, "to", Mask(o.to), "attempt", o.attempts+1, "err", errText)
		}
	}
}

// OutboxEntry is one message as the operator sees it. Sign-in codes are
// redacted unless they were only recorded (development mode).
type OutboxEntry struct {
	ID        int64  `json:"id"`
	Slug      string `json:"slug"`
	Kind      string `json:"kind"`
	Channel   string `json:"channel"`
	To        string `json:"to"`
	Text      string `json:"text"`
	Status    string `json:"status"`
	Attempts  int    `json:"attempts"`
	LastError string `json:"last_error,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

func (p *Platform) Outbox(ctx context.Context, limit int) ([]OutboxEntry, error) {
	rows, err := p.r.QueryContext(ctx, `SELECT id,slug,kind,channel,to_addr,body,status,attempts,last_error,created_at FROM outbox ORDER BY id DESC LIMIT ?`, max(1, min(limit, 500)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OutboxEntry{}
	for rows.Next() {
		var e OutboxEntry
		if err := rows.Scan(&e.ID, &e.Slug, &e.Kind, &e.Channel, &e.To, &e.Text, &e.Status, &e.Attempts, &e.LastError, &e.CreatedAt); err != nil {
			return nil, err
		}
		if e.Kind == "otp" && e.Status != "logged" {
			e.Text = "(sign-in code hidden)"
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- broadcasts (a shop messaging its members) -----------------------------------------------

const (
	MaxBroadcastLen     = 459 // three SMS
	MaxBroadcastsPerDay = 3
)

type BroadcastPlan struct {
	Audience     int            `json:"audience"`      // members who opted in to offers
	ByChannel    map[string]int `json:"by_channel"`    // recipients per channel
	Skipped      int            `json:"skipped"`       // opted in, but their channel is unavailable
	SMSUnits     int            `json:"sms_units"`     // SMS segments needed
	SMSRemaining int            `json:"sms_remaining"` // left this month
	WAUnits      int            `json:"wa_units"`
	WARemaining  int            `json:"wa_remaining"`
	Fits         bool           `json:"fits"`
	SentToday    int            `json:"sent_today"`
	ID           int64          `json:"id,omitempty"`
	Queued       int            `json:"queued,omitempty"`
}

// Broadcast plans (dry) or sends a message from a shop to every member who
// opted in, each on their chosen channel, with an unsubscribe link. Paid
// channels must fit within the shop's remaining monthly cap or nothing is sent.
func (p *Platform) Broadcast(ctx context.Context, slug, shopName, actor, text, baseURL string, dry bool) (*BroadcastPlan, error) {
	text = strings.TrimSpace(text)
	if text == "" || utf8.RuneCountInString(text) > MaxBroadcastLen {
		return nil, bad("text", "write a message of 1-%d characters", MaxBroadcastLen)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	tx, err := p.w.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	u, err := p.usage(ctx, tx, slug)
	if err != nil {
		return nil, err
	}
	plan := &BroadcastPlan{ByChannel: map[string]int{}, SMSRemaining: u.remaining("sms"), WARemaining: u.remaining("whatsapp")}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM broadcasts WHERE slug=? AND created_at>?`, slug, p.now().Add(-24*time.Hour).Unix()).Scan(&plan.SentToday); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT s.id,s.name,COALESCE(s.email,''),COALESCE(s.phone,''),s.channel,s.created_at
		FROM memberships m JOIN shoppers s ON s.id=m.shopper_id WHERE m.slug=? AND m.marketing=1 ORDER BY m.joined_at`, slug)
	if err != nil {
		return nil, err
	}
	type msg struct{ ch, to, subject, body string }
	var msgs []msg
	for rows.Next() {
		s, err := scanShopper(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		plan.Audience++
		to := contactFor(s)
		if (s.Channel == "email" && s.Email == "") || (s.Channel != "email" && s.Phone == "") || to == "" {
			plan.Skipped++
			continue
		}
		link := strings.TrimRight(baseURL, "/") + "/u/" + p.UnsubscribeToken(s.ID, slug)
		var body, subject string
		if s.Channel == "email" {
			subject = shopName + ": news for you"
			body = text + "\n\n-- " + shopName + "\nYou get this because you asked " + shopName + " for offers on Price Tag. Stop these messages: " + link
		} else {
			body = text + "\n- " + shopName + ". Stop: " + link
		}
		msgs = append(msgs, msg{s.Channel, to, subject, body})
		plan.ByChannel[s.Channel]++
		switch s.Channel {
		case "sms":
			plan.SMSUnits += SMSSegments(body)
		case "whatsapp":
			plan.WAUnits++
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	plan.Fits = plan.SMSUnits <= plan.SMSRemaining && plan.WAUnits <= plan.WARemaining && plan.SentToday < MaxBroadcastsPerDay
	if dry {
		return plan, nil
	}
	if plan.SentToday >= MaxBroadcastsPerDay {
		return plan, bad("too_often", "a shop can send at most %d messages to its customers per day", MaxBroadcastsPerDay)
	}
	if !plan.Fits {
		return plan, bad("over_cap", "this message needs %d SMS and %d WhatsApp messages but only %d and %d are left this month; ask Price Tag to raise your limit",
			plan.SMSUnits, plan.WAUnits, plan.SMSRemaining, plan.WARemaining)
	}
	if len(msgs) == 0 {
		return plan, bad("no_audience", "no customers have asked for offers yet")
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO broadcasts(slug,created_at,created_by,body,audience,queued,skipped) VALUES(?,?,?,?,?,?,?)`,
		slug, p.now().Unix(), actor, text, plan.Audience, len(msgs), plan.Skipped)
	if err != nil {
		return nil, err
	}
	plan.ID, _ = res.LastInsertId()
	for _, m := range msgs {
		if err := p.queueTx(ctx, tx, slug, "broadcast", m.ch, m.to, m.subject, m.body, &plan.ID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	plan.Queued = len(msgs)
	p.Kick()
	return plan, nil
}

type BroadcastSummary struct {
	ID        int64  `json:"id"`
	CreatedAt int64  `json:"created_at"`
	CreatedBy string `json:"created_by"`
	Text      string `json:"text"`
	Queued    int    `json:"queued"`
	Delivered int    `json:"delivered"`
	Failed    int    `json:"failed"`
	Pending   int    `json:"pending"`
}

func (p *Platform) Broadcasts(ctx context.Context, slug string) ([]BroadcastSummary, error) {
	rows, err := p.r.QueryContext(ctx, `SELECT b.id,b.created_at,b.created_by,b.body,b.queued,
		(SELECT COUNT(*) FROM outbox o WHERE o.broadcast_id=b.id AND o.status IN ('sent','logged')),
		(SELECT COUNT(*) FROM outbox o WHERE o.broadcast_id=b.id AND o.status='failed'),
		(SELECT COUNT(*) FROM outbox o WHERE o.broadcast_id=b.id AND o.status='queued')
		FROM broadcasts b WHERE b.slug=? ORDER BY b.id DESC LIMIT 50`, slug)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BroadcastSummary{}
	for rows.Next() {
		var b BroadcastSummary
		if err := rows.Scan(&b.ID, &b.CreatedAt, &b.CreatedBy, &b.Text, &b.Queued, &b.Delivered, &b.Failed, &b.Pending); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
