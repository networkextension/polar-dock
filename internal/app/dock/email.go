package dock

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const emailVerificationTTL = 30 * time.Minute

type MailSender interface {
	Enabled() bool
	Send(ctx context.Context, message MailMessage) error
}

type MailMessage struct {
	To        string
	Subject   string
	PlainText string
}

type smtpMailer struct {
	host      string
	port      int
	username  string
	password  string
	fromEmail string
	fromName  string
}

func newSMTPMailer(cfg Config) MailSender {
	return &smtpMailer{
		host:      strings.TrimSpace(cfg.SMTPHost),
		port:      cfg.SMTPPort,
		username:  strings.TrimSpace(cfg.SMTPUsername),
		password:  cfg.SMTPPassword,
		fromEmail: strings.TrimSpace(cfg.SMTPFromEmail),
		fromName:  strings.TrimSpace(cfg.SMTPFromName),
	}
}

func (m *smtpMailer) Enabled() bool {
	return m != nil &&
		m.host != "" &&
		m.port > 0 &&
		m.username != "" &&
		m.password != "" &&
		m.fromEmail != ""
}

func (m *smtpMailer) Send(ctx context.Context, message MailMessage) error {
	if !m.Enabled() {
		return errors.New("smtp mailer is not configured")
	}
	if strings.TrimSpace(message.To) == "" {
		return errors.New("missing recipient email")
	}

	toAddr := strings.TrimSpace(message.To)
	fromHeader := m.fromEmail
	if m.fromName != "" {
		fromHeader = (&mail.Address{
			Name:    m.fromName,
			Address: m.fromEmail,
		}).String()
	}

	subject := mime.QEncoding.Encode("utf-8", message.Subject)
	body := strings.ReplaceAll(message.PlainText, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\n", "\r\n")

	msg := strings.Join([]string{
		"From: " + fromHeader,
		"To: " + toAddr,
		"Subject: " + subject,
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=UTF-8",
		"Content-Transfer-Encoding: 8bit",
		"",
		body,
	}, "\r\n")

	addr := fmt.Sprintf("%s:%d", m.host, m.port)
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))

	client, err := smtp.NewClient(conn, m.host)
	if err != nil {
		return err
	}
	defer client.Close()

	if ok, _ := client.Extension("STARTTLS"); !ok {
		return errors.New("smtp server does not support STARTTLS")
	}
	if err := client.StartTLS(&tls.Config{ServerName: m.host, MinVersion: tls.VersionTLS12}); err != nil {
		return err
	}
	if err := client.Auth(smtp.PlainAuth("", m.username, m.password, m.host)); err != nil {
		return err
	}
	if err := client.Mail(m.fromEmail); err != nil {
		return err
	}
	if err := client.Rcpt(toAddr); err != nil {
		return err
	}

	writer, err := client.Data()
	if err != nil {
		return err
	}
	buffered := bufio.NewWriter(writer)
	if _, err := buffered.WriteString(msg); err != nil {
		_ = writer.Close()
		return err
	}
	if err := buffered.Flush(); err != nil {
		_ = writer.Close()
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return client.Quit()
}

func (s *Server) buildEmailVerificationLink(token string) string {
	baseURL := strings.TrimRight(s.publicBaseURL, "/")
	if baseURL == "" {
		baseURL = strings.TrimRight(DefaultPasskeyOrigin, "/")
	}
	return fmt.Sprintf("%s/api/email-verification/verify?token=%s", baseURL, url.QueryEscape(token))
}

func buildEmailVerificationMessage(lang string, user *User, verifyURL string) MailMessage {
	subject := "Verify your email address"
	body := strings.Join([]string{
		fmt.Sprintf("Hi %s,", user.Username),
		"",
		"Please confirm your email address by opening the link below:",
		verifyURL,
		"",
		fmt.Sprintf("This link will expire in %d minutes.", int(emailVerificationTTL/time.Minute)),
		"If you did not request this, you can safely ignore this email.",
	}, "\n")
	if normalizeLang(lang) == langZhCN {
		subject = "请验证你的邮箱地址"
		body = strings.Join([]string{
			fmt.Sprintf("%s，你好：", user.Username),
			"",
			"请打开下面的链接完成邮箱验证：",
			verifyURL,
			"",
			fmt.Sprintf("该链接将在 %d 分钟后失效。", int(emailVerificationTTL/time.Minute)),
			"如果这不是你本人发起的操作，可以直接忽略这封邮件。",
		}, "\n")
	}
	return MailMessage{
		To:        user.Email,
		Subject:   subject,
		PlainText: body,
	}
}

// buildTeamInvitationLink composes the public preview URL the
// recipient clicks to view + accept the invitation. Uses the static
// HTML page with the token in the query string, which works in both
// the dev express layer and any prod static-serving frontend without
// path-parameter routing.
func (s *Server) buildTeamInvitationLink(token string) string {
	baseURL := strings.TrimRight(s.publicBaseURL, "/")
	if baseURL == "" {
		baseURL = strings.TrimRight(DefaultPasskeyOrigin, "/")
	}
	return fmt.Sprintf("%s/invite.html?token=%s", baseURL, url.QueryEscape(token))
}

// buildTeamInvitationMessage formats the invitation email. zh-CN +
// English variants — matches the existing email-verification i18n
// pattern so we don't fork tone or signature.
func buildTeamInvitationMessage(lang string, teamName, inviterName, inviterEmail, role, recipient, inviteURL string, ttl time.Duration) MailMessage {
	ttlHours := int(ttl / time.Hour)
	roleLabelEn := role
	roleLabelZh := role
	switch role {
	case "owner":
		roleLabelEn = "Owner"
		roleLabelZh = "所有者"
	case "admin":
		roleLabelEn = "Admin"
		roleLabelZh = "管理员"
	case "member":
		roleLabelEn = "Member"
		roleLabelZh = "成员"
	case "viewer":
		roleLabelEn = "Viewer"
		roleLabelZh = "只读"
	}

	subject := fmt.Sprintf("%s invited you to join %s", inviterName, teamName)
	body := strings.Join([]string{
		fmt.Sprintf("Hi,"),
		"",
		fmt.Sprintf("%s (%s) has invited you to join the workspace \"%s\" as %s.", inviterName, inviterEmail, teamName, roleLabelEn),
		"",
		"Click the link below to view and accept the invitation:",
		inviteURL,
		"",
		fmt.Sprintf("This invitation expires in %d hours.", ttlHours),
		"If you didn't expect this email, you can safely ignore it.",
		"",
		"— Polar",
	}, "\n")
	if normalizeLang(lang) == langZhCN {
		subject = fmt.Sprintf("%s 邀请你加入 %s", inviterName, teamName)
		body = strings.Join([]string{
			"你好，",
			"",
			fmt.Sprintf("%s（%s）邀请你以「%s」身份加入工作区「%s」。", inviterName, inviterEmail, roleLabelZh, teamName),
			"",
			"点击下方链接查看并接受邀请：",
			inviteURL,
			"",
			fmt.Sprintf("该邀请将在 %d 小时后过期。", ttlHours),
			"如果你不认识此发件人，可以直接忽略这封邮件。",
			"",
			"—— Polar",
		}, "\n")
	}
	return MailMessage{
		To:        recipient,
		Subject:   subject,
		PlainText: body,
	}
}

// sendTeamInvitation dispatches the email if a mailer is configured.
// Returns nil + ok=false if the mailer is disabled — the handler then
// surfaces a "邮件未发送，请手动复制链接" hint to the inviter; the
// invitation row itself was already created and the link works.
func (s *Server) sendTeamInvitation(ctx context.Context, lang, teamName, inviterName, inviterEmail string, inv *TeamInvitation, inviteURL string) (bool, error) {
	if s == nil || s.mailer == nil || !s.mailer.Enabled() {
		return false, nil
	}
	ttl := time.Until(inv.ExpiresAt)
	msg := buildTeamInvitationMessage(lang, teamName, inviterName, inviterEmail, inv.Role, inv.Email, inviteURL, ttl)
	if err := s.mailer.Send(ctx, msg); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Server) sendEmailVerification(ctx context.Context, lang string, user *User) error {
	if s == nil || s.mailer == nil || !s.mailer.Enabled() {
		return errors.New("email service unavailable")
	}
	token, err := s.createEmailVerificationToken(user.ID, user.Email, time.Now(), emailVerificationTTL)
	if err != nil {
		return err
	}
	verifyURL := s.buildEmailVerificationLink(token)
	if err := s.mailer.Send(ctx, buildEmailVerificationMessage(lang, user, verifyURL)); err != nil {
		_ = s.deletePendingEmailVerificationTokens(user.ID)
		return err
	}
	return nil
}

func (s *Server) handleEmailVerificationSend(c *gin.Context) {
	userID, _ := c.Get("user_id")
	userIDStr, _ := userID.(string)
	user, err := s.getUserByID(userIDStr)
	if err != nil || user == nil {
		jsonError(c, http.StatusInternalServerError, "common.server_error")
		return
	}
	if user.EmailVerified {
		jsonMessage(c, http.StatusOK, "email.already_verified", gin.H{
			"email_verified": true,
		})
		return
	}
	if s.mailer == nil || !s.mailer.Enabled() {
		jsonError(c, http.StatusServiceUnavailable, "email.service_unavailable")
		return
	}
	if err := s.sendEmailVerification(c.Request.Context(), requestLang(c), user); err != nil {
		jsonError(c, http.StatusInternalServerError, "email.send_failed")
		return
	}
	jsonMessage(c, http.StatusOK, "email.verification_sent", gin.H{
		"email":          user.Email,
		"email_verified": false,
	})
}

func (s *Server) handleEmailVerificationConfirm(c *gin.Context) {
	token := strings.TrimSpace(c.Query("token"))
	if token == "" {
		jsonError(c, http.StatusBadRequest, "email.invalid_token")
		return
	}
	user, err := s.consumeEmailVerificationToken(token, time.Now())
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "common.server_error")
		return
	}
	if user == nil {
		jsonError(c, http.StatusBadRequest, "email.invalid_token")
		return
	}
	jsonMessage(c, http.StatusOK, "email.verify_success", gin.H{
		"user_id":        user.ID,
		"email":          user.Email,
		"email_verified": user.EmailVerified,
	})
}
