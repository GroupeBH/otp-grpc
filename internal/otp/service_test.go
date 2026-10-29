package otp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	otpv1 "otp-grpc/gen/otp/v1"
)

const testPhone = "+243899123456"

type failingSMS struct{}

func (failingSMS) Send(context.Context, string, string) error { return errors.New("provider failed") }

func fixture(t *testing.T) (*Service, *miniredis.Miniredis, *string) {
	t.Helper()
	m := miniredis.RunT(t)
	db := redis.NewClient(&redis.Options{Addr: m.Addr()})
	t.Cleanup(func() { db.Close() })
	code := new(string)
	c := Config{HashKey: strings.Repeat("x", 32), TTL: 5 * time.Minute, ProofTTL: 2 * time.Minute, Cooldown: time.Minute, MaxAttempts: 5, PhoneHourly: 5, PhoneDaily: 10, IPHourly: 20, IdentityHourly: 10, Provider: "mock"}
	s := NewService(db, MockProvider{Delivered: func(_, message string) { *code = mockCode.FindString(message) }}, c, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return s, m, code
}
func request(id string) *otpv1.SendOtpRequest {
	return &otpv1.SendOtpRequest{Phone: testPhone, Purpose: "phone_verification", IdempotencyKey: id}
}

func TestCodeGeneration(t *testing.T) {
	for i := 0; i < 1000; i++ {
		code, e := generateCode()
		if e != nil || !codePattern.MatchString(code) {
			t.Fatalf("invalid code: %q %v", code, e)
		}
	}
}

func TestSendVerifyConsumeAndStatus(t *testing.T) {
	s, m, code := fixture(t)
	ctx := context.Background()
	sent, e := s.SendOtp(ctx, request("request-0001"))
	if e != nil || sent.Status != "SENT" {
		t.Fatalf("send: %v %v", sent, e)
	}
	again, e := s.SendOtp(ctx, request("request-0001"))
	if e != nil || again.ChallengeId != sent.ChallengeId {
		t.Fatalf("idempotency: %v %v", again, e)
	}
	st, e := s.CheckOtpStatus(ctx, &otpv1.CheckOtpStatusRequest{ChallengeId: sent.ChallengeId})
	if e != nil || st.Status != "SENT" {
		t.Fatalf("status: %v %v", st, e)
	}
	bad, e := s.VerifyOtp(ctx, &otpv1.VerifyOtpRequest{Phone: testPhone, Purpose: "phone_verification", Code: "000000"})
	if *code == "000000" {
		t.Skip("random code matched test wrong code")
	}
	if e != nil || bad.Status != "INVALID" {
		t.Fatalf("bad verify: %v %v", bad, e)
	}
	good, e := s.VerifyOtp(ctx, &otpv1.VerifyOtpRequest{Phone: testPhone, Purpose: "phone_verification", Code: *code})
	if e != nil || good.Status != "VALID" || good.Proof == "" {
		t.Fatalf("valid: %v %v", good, e)
	}
	replay, e := s.VerifyOtp(ctx, &otpv1.VerifyOtpRequest{Phone: testPhone, Purpose: "phone_verification", Code: *code})
	if e != nil || replay.Status == "VALID" {
		t.Fatalf("replayed: %v %v", replay, e)
	}
	proof := &otpv1.ConsumeProofRequest{Phone: testPhone, Purpose: "phone_verification", Proof: good.Proof}
	wrongProof, e := s.ConsumeProof(ctx, &otpv1.ConsumeProofRequest{Phone: "+243899000000", Purpose: "phone_verification", Proof: good.Proof})
	if e != nil || wrongProof.Valid {
		t.Fatalf("wrong phone consumed proof: %v %v", wrongProof, e)
	}
	consumed, e := s.ConsumeProof(ctx, proof)
	if e != nil || !consumed.Valid {
		t.Fatalf("consume: %v %v", consumed, e)
	}
	consumed, e = s.ConsumeProof(ctx, proof)
	if e != nil || consumed.Valid {
		t.Fatalf("proof replay: %v %v", consumed, e)
	}
	m.FastForward(5 * time.Minute)
	st, e = s.CheckOtpStatus(ctx, &otpv1.CheckOtpStatusRequest{ChallengeId: sent.ChallengeId})
	if e != nil || st.Status != "EXPIRED" {
		t.Fatalf("expiry: %v %v", st, e)
	}
}

func TestAttemptsCooldownResendAndFailure(t *testing.T) {
	s, m, code := fixture(t)
	ctx := context.Background()
	first, e := s.SendOtp(ctx, request("request-0001"))
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.ResendOtp(ctx, &otpv1.ResendOtpRequest{Phone: testPhone, Purpose: "phone_verification", ChallengeId: first.ChallengeId, IdempotencyKey: "request-0002"})
	if status.Code(e) != codes.ResourceExhausted {
		t.Fatalf("cooldown: %v", e)
	}
	for i := 0; i < 5; i++ {
		wrong := "000000"
		if *code == wrong {
			wrong = "111111"
		}
		result, e := s.VerifyOtp(ctx, &otpv1.VerifyOtpRequest{Phone: testPhone, Purpose: "phone_verification", Code: wrong})
		if e != nil {
			t.Fatal(e)
		}
		if i == 4 && result.Status != "LOCKED" {
			t.Fatalf("lockout: %v", result)
		}
	}
	m.FastForward(time.Minute)
	second, e := s.ResendOtp(ctx, &otpv1.ResendOtpRequest{Phone: testPhone, Purpose: "phone_verification", ChallengeId: first.ChallengeId, IdempotencyKey: "request-0003"})
	if e != nil || second.ChallengeId == first.ChallengeId {
		t.Fatalf("resend: %v %v", second, e)
	}
	s.SMS = failingSMS{}
	m.FastForward(time.Minute)
	_, e = s.SendOtp(ctx, request("request-0004"))
	if status.Code(e) != codes.Unavailable {
		t.Fatalf("provider failure: %v", e)
	}
	st, e := s.CheckOtpStatus(ctx, &otpv1.CheckOtpStatusRequest{ChallengeId: second.ChallengeId})
	if e != nil || st.Status != "EXPIRED" {
		t.Fatalf("failed send must invalidate: %v %v", st, e)
	}
}

func TestConcurrentVerification(t *testing.T) {
	s, _, code := fixture(t)
	ctx := context.Background()
	_, e := s.SendOtp(ctx, request("request-0001"))
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	results := make(chan string, 2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			r, e := s.VerifyOtp(ctx, &otpv1.VerifyOtpRequest{Phone: testPhone, Purpose: "phone_verification", Code: *code})
			if e != nil {
				results <- status.Code(e).String()
			} else {
				results <- r.Status
			}
		}()
	}
	wg.Wait()
	close(results)
	valid := 0
	for r := range results {
		if r == "VALID" {
			valid++
		}
	}
	if valid != 1 {
		t.Fatalf("expected one successful verification, got %d", valid)
	}
}

func TestMobishastraSubmission(t *testing.T) {
	var query string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		w.Write([]byte("123 Send Successful"))
	}))
	defer server.Close()
	p := MobishastraProvider{Client: server.Client(), Endpoint: server.URL, User: "user", Password: "secret", Sender: "ZWANGA", CountryCode: "ALL"}
	if e := p.Send(context.Background(), testPhone, "Test message"); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(query, "mobileno=243899123456") || !strings.Contains(query, "msgtext=Test+message") || !strings.Contains(query, "pwd=secret") {
		t.Fatalf("query malformed: %s", strings.ReplaceAll(query, "secret", "[redacted]"))
	}
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("Invalid Password")) })
	if e := p.Send(context.Background(), testPhone, "Test"); e == nil || strings.Contains(e.Error(), "secret") {
		t.Fatalf("unsafe provider error: %v", e)
	}
}
