package otp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

type SMSProvider interface {
	Send(context.Context, string, string) error
}

type MockProvider struct {
	DB        *redis.Client
	Delivered func(string, string)
}

var mockCode = regexp.MustCompile(`[0-9]{6}`)

func (m MockProvider) Send(ctx context.Context, phone, message string) error {
	if m.Delivered != nil {
		m.Delivered(phone, message)
	}
	if m.DB != nil {
		code := mockCode.FindString(message)
		if code == "" {
			return errors.New("mock SMS missing code")
		}
		sum := sha256.Sum256([]byte(phone))
		return m.DB.Set(ctx, "otp:mock-outbox:"+hex.EncodeToString(sum[:]), code, 5*time.Minute).Err()
	}
	return nil
}

type MobishastraProvider struct {
	Client                              *http.Client
	Endpoint                            string
	User, Password, Sender, CountryCode string
}

var successful = regexp.MustCompile(`^(?:[0-9]+\s+)?Send Successful$`)

func (m MobishastraProvider) Send(ctx context.Context, phone, message string) error {
	endpoint := m.Endpoint
	if endpoint == "" {
		endpoint = "https://mshastra.com/sendurl.aspx"
	}
	u, e := url.Parse(endpoint)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" {
		return errors.New("invalid Mobishastra endpoint")
	}
	q := u.Query()
	q.Set("user", m.User)
	q.Set("pwd", m.Password)
	q.Set("senderid", m.Sender)
	q.Set("mobileno", strings.TrimPrefix(phone, "+"))
	q.Set("msgtext", message)
	q.Set("priority", "High")
	q.Set("CountryCode", m.CountryCode)
	u.RawQuery = q.Encode()
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if e != nil {
		return e
	}
	client := m.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	resp, e := client.Do(req)
	if e != nil {
		return errors.New("SMS transport failure")
	}
	defer resp.Body.Close()
	body, e := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if e != nil {
		return errors.New("SMS response unreadable")
	}
	if resp.StatusCode != http.StatusOK || !successful.MatchString(strings.TrimSpace(string(body))) {
		return errors.New("SMS provider rejected submission")
	}
	return nil
}
