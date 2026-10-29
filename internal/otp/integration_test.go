package otp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	otpv1 "otp-grpc/gen/otp/v1"
)

func TestIntegrationRedisGRPC(t *testing.T) {
	addr := os.Getenv("OTP_INTEGRATION_REDIS_ADDRESS")
	if addr == "" {
		t.Skip("set OTP_INTEGRATION_REDIS_ADDRESS to run against an existing Redis")
	}
	dbIndex := 15
	if raw := os.Getenv("OTP_INTEGRATION_REDIS_DB"); raw != "" {
		n, e := strconv.Atoi(raw)
		if e != nil || n < 0 || n > 15 {
			t.Fatal("invalid integration Redis DB")
		}
		dbIndex = n
	}
	db := redis.NewClient(&redis.Options{Addr: addr, Password: os.Getenv("OTP_INTEGRATION_REDIS_PASSWORD"), DB: dbIndex})
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if e := db.Ping(ctx).Err(); e != nil {
		t.Fatalf("Redis unavailable: %v", e)
	}
	config := Config{HashKey: strings.Repeat("i", 32), TTL: 5 * time.Minute, ProofTTL: 2 * time.Minute, Cooldown: time.Minute, MaxAttempts: 5, PhoneHourly: 5, PhoneDaily: 10, IPHourly: 20, IdentityHourly: 10, Provider: "mock"}
	phone := "+243899" + strconv.FormatInt(time.Now().UnixNano()%100000000+100000000, 10)
	start := func() (*grpc.Server, *grpc.ClientConn, otpv1.OtpServiceClient) {
		t.Helper()
		listener, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		server := grpc.NewServer()
		otpv1.RegisterOtpServiceServer(server, NewService(db, MockProvider{DB: db}, config, slog.New(slog.NewTextHandler(io.Discard, nil))))
		h := health.NewServer()
		h.SetServingStatus("otp.v1.OtpService", healthpb.HealthCheckResponse_SERVING)
		healthpb.RegisterHealthServer(server, h)
		go server.Serve(listener)
		conn, e := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
		if e != nil {
			t.Fatal(e)
		}
		return server, conn, otpv1.NewOtpServiceClient(conn)
	}
	server, conn, client := start()
	healthClient := healthpb.NewHealthClient(conn)
	healthResponse, e := healthClient.Check(ctx, &healthpb.HealthCheckRequest{Service: "otp.v1.OtpService"})
	if e != nil || healthResponse.Status != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("health: %v %v", healthResponse, e)
	}
	sent, e := client.SendOtp(ctx, &otpv1.SendOtpRequest{Phone: phone, Purpose: "phone_verification", IdempotencyKey: "integration-" + strconv.FormatInt(time.Now().UnixNano(), 10)})
	if e != nil {
		t.Fatal(e)
	}
	hash := sha256.Sum256([]byte(phone))
	code, e := db.Get(ctx, "otp:mock-outbox:"+hex.EncodeToString(hash[:])).Result()
	if e != nil || !codePattern.MatchString(code) {
		t.Fatalf("mock delivery: %v", e)
	}
	conn.Close()
	server.Stop()
	server, conn, client = start()
	defer conn.Close()
	defer server.Stop()
	verified, e := client.VerifyOtp(ctx, &otpv1.VerifyOtpRequest{Phone: phone, Purpose: "phone_verification", Code: code})
	if e != nil || verified.Status != "VALID" {
		t.Fatalf("verify after restart: %v %v", verified, e)
	}
	proof, e := client.ConsumeProof(ctx, &otpv1.ConsumeProofRequest{Phone: phone, Purpose: "phone_verification", Proof: verified.Proof})
	if e != nil || !proof.Valid {
		t.Fatalf("proof: %v %v", proof, e)
	}
	replay, e := client.ConsumeProof(ctx, &otpv1.ConsumeProofRequest{Phone: phone, Purpose: "phone_verification", Proof: verified.Proof})
	if e != nil || replay.Valid {
		t.Fatalf("replayed proof: %v %v", replay, e)
	}
	if sent.ChallengeId == "" {
		t.Fatal("missing challenge id")
	}
}
