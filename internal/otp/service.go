package otp

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"regexp"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	otpv1 "otp-grpc/gen/otp/v1"
)

var phonePattern = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)
var purposePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{1,63}$`)
var codePattern = regexp.MustCompile(`^[0-9]{6}$`)

type Service struct {
	otpv1.UnimplementedOtpServiceServer
	DB     *redis.Client
	SMS    SMSProvider
	Config Config
	Log    *slog.Logger
}

func NewService(db *redis.Client, sms SMSProvider, c Config, logger *slog.Logger) *Service {
	return &Service{DB: db, SMS: sms, Config: c, Log: logger}
}
func (s *Service) hash(value string) string { return s.digest("key", value) }
func (s *Service) digest(parts ...string) string {
	mac := hmac.New(sha256.New, []byte(s.Config.HashKey))
	for _, p := range parts {
		mac.Write([]byte(p))
		mac.Write([]byte{0})
	}
	return hex.EncodeToString(mac.Sum(nil))
}
func token(n int) (string, error) {
	b := make([]byte, n)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	return hex.EncodeToString(b), nil
}
func generateCode() (string, error) {
	n, e := rand.Int(rand.Reader, big.NewInt(1000000))
	if e != nil {
		return "", e
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}
func validate(phone, purpose string) error {
	if !phonePattern.MatchString(phone) || !purposePattern.MatchString(purpose) {
		return status.Error(codes.InvalidArgument, "invalid phone or purpose")
	}
	return nil
}
func (s *Service) key(phone string) string { return "otp:challenge:v1:" + s.hash(phone) }
func (s *Service) lock(ctx context.Context, phone string) (func(), error) {
	key := "otp:lock:v1:" + s.hash(phone)
	id, e := token(16)
	if e != nil {
		return nil, e
	}
	ok, e := s.DB.SetNX(ctx, key, id, 15*time.Second).Result()
	if e != nil {
		return nil, e
	}
	if !ok {
		return nil, status.Error(codes.Aborted, "OTP operation in progress")
	}
	return func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s.DB.Eval(releaseCtx, `if redis.call('GET',KEYS[1]) == ARGV[1] then return redis.call('DEL',KEYS[1]) end return 0`, []string{key}, id)
	}, nil
}

const reserveScript = `
for i=1,#KEYS do
 local count=tonumber(redis.call('GET',KEYS[i]) or '0')
 if count>=tonumber(ARGV[i]) then return redis.call('PTTL',KEYS[i]) end
end
for i=1,#KEYS do
 local count=redis.call('INCR',KEYS[i])
 if count==1 then redis.call('PEXPIRE',KEYS[i],ARGV[i+#KEYS]) end
end
return 0`

func (s *Service) reserve(ctx context.Context, phone, ip, identity string) (time.Duration, error) {
	h := s.hash(phone)
	keys := []string{"otp:quota:cool:" + h, "otp:quota:hour:" + h, "otp:quota:day:" + h, "otp:quota:global:day"}
	limits := []int{1, s.Config.PhoneHourly, s.Config.PhoneDaily, 100000}
	ttls := []time.Duration{s.Config.Cooldown, time.Hour, 24 * time.Hour, 24 * time.Hour}
	if ip != "" {
		keys = append(keys, "otp:quota:ip:"+s.hash(ip))
		limits = append(limits, s.Config.IPHourly)
		ttls = append(ttls, time.Hour)
	}
	if identity != "" {
		keys = append(keys, "otp:quota:identity:"+s.hash(identity))
		limits = append(limits, s.Config.IdentityHourly)
		ttls = append(ttls, time.Hour)
	}
	args := make([]any, 0, len(keys)*2)
	for _, v := range limits {
		args = append(args, v)
	}
	for _, v := range ttls {
		args = append(args, v.Milliseconds())
	}
	result, e := s.DB.Eval(ctx, reserveScript, keys, args...).Int64()
	if e != nil {
		return 0, e
	}
	if result > 0 {
		return time.Duration(result) * time.Millisecond, nil
	}
	return 0, nil
}

func (s *Service) SendOtp(ctx context.Context, r *otpv1.SendOtpRequest) (*otpv1.SendOtpResponse, error) {
	return s.send(ctx, r.Phone, r.Purpose, r.IdempotencyKey, r.RequesterIp, r.IdentityId, "")
}
func (s *Service) ResendOtp(ctx context.Context, r *otpv1.ResendOtpRequest) (*otpv1.SendOtpResponse, error) {
	if len(r.ChallengeId) != 32 {
		return nil, status.Error(codes.InvalidArgument, "challenge id required")
	}
	return s.send(ctx, r.Phone, r.Purpose, r.IdempotencyKey, r.RequesterIp, r.IdentityId, r.ChallengeId)
}

func (s *Service) send(ctx context.Context, phone, purpose, idempotency, ip, identity, previousID string) (*otpv1.SendOtpResponse, error) {
	if e := validate(phone, purpose); e != nil {
		return nil, e
	}
	s.Log.Info("otp_request", "purpose", purpose)
	if len(idempotency) < 8 || len(idempotency) > 128 {
		return nil, status.Error(codes.InvalidArgument, "idempotency key required")
	}
	unlock, e := s.lock(ctx, phone)
	if e != nil {
		return nil, asInternal(e)
	}
	defer unlock()
	idKey := "otp:idempotency:v1:" + s.digest(phone, purpose, idempotency)
	prior, e := s.DB.HGetAll(ctx, idKey).Result()
	if e != nil {
		return nil, asInternal(e)
	}
	if len(prior) > 0 {
		if prior["state"] == "SENT" {
			current, _ := s.DB.HGet(ctx, s.key(phone), "id").Result()
			if current != prior["id"] {
				return nil, status.Error(codes.FailedPrecondition, "challenge superseded")
			}
			ttl, _ := s.DB.TTL(ctx, s.key(phone)).Result()
			return &otpv1.SendOtpResponse{ChallengeId: prior["id"], ExpiresInSeconds: int32(max(0, int(ttl.Seconds()))), Status: "SENT"}, nil
		}
		return nil, status.Error(codes.FailedPrecondition, "request already processed or pending")
	}
	if previousID != "" {
		current, e := s.DB.HGet(ctx, s.key(phone), "id").Result()
		if e != nil || current != previousID {
			return nil, status.Error(codes.FailedPrecondition, "challenge unavailable")
		}
	}
	if wait, e := s.reserve(ctx, phone, ip, identity); e != nil {
		return nil, asInternal(e)
	} else if wait > 0 {
		s.Log.Info("otp_quota_exceeded", "purpose", purpose)
		return nil, status.Errorf(codes.ResourceExhausted, "OTP quota; retry after %d seconds", int(wait.Seconds())+1)
	}
	challengeID, e := token(16)
	if e != nil {
		return nil, asInternal(e)
	}
	code, e := generateCode()
	if e != nil {
		return nil, asInternal(e)
	}
	// Pending state is stored before the external call. A lost SMS response cannot trigger an automatic resend.
	if e = s.DB.HSet(ctx, idKey, "state", "PENDING", "id", challengeID).Err(); e != nil {
		return nil, asInternal(e)
	}
	if e = s.DB.Expire(ctx, idKey, s.Config.TTL).Err(); e != nil {
		return nil, asInternal(e)
	}
	challengeKey := s.key(phone)
	fields := map[string]any{"id": challengeID, "purpose": purpose, "digest": s.digest(phone, purpose, challengeID, code), "attempts": 0, "state": "PENDING", "expires": time.Now().Add(s.Config.TTL).Unix()}
	if e = s.DB.HSet(ctx, challengeKey, fields).Err(); e != nil {
		return nil, asInternal(e)
	}
	if e = s.DB.Expire(ctx, challengeKey, s.Config.TTL).Err(); e != nil {
		return nil, asInternal(e)
	}
	if e = s.DB.Set(ctx, "otp:challenge-index:v1:"+challengeID, challengeKey, s.Config.TTL).Err(); e != nil {
		return nil, asInternal(e)
	}
	message := fmt.Sprintf("Votre code Zwanga est %s. Il expire dans %d secondes.", code, int(s.Config.TTL.Seconds()))
	sendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if e = s.SMS.Send(sendCtx, phone, message); e != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s.DB.Del(cleanupCtx, challengeKey)
		s.DB.HSet(cleanupCtx, idKey, "state", "FAILED")
		s.Log.Error("sms_send_failed", "provider", s.Config.Provider, "error", e.Error())
		return nil, status.Error(codes.Unavailable, "SMS delivery unavailable")
	}
	// Finish with a bounded context even when the caller disconnected after SMS submission.
	finishCtx, finishCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer finishCancel()
	if e = s.DB.HSet(finishCtx, idKey, "state", "SENT").Err(); e != nil {
		return nil, asInternal(e)
	}
	if e = s.DB.HSet(finishCtx, challengeKey, "state", "SENT").Err(); e != nil {
		return nil, asInternal(e)
	}
	s.Log.Info("otp_sent", "provider", s.Config.Provider, "purpose", purpose)
	return &otpv1.SendOtpResponse{ChallengeId: challengeID, ExpiresInSeconds: int32(s.Config.TTL.Seconds()), Status: "SENT"}, nil
}

const verifyScript = `
local id=redis.call('HGET',KEYS[1],'id')
if not id then return 'EXPIRED' end
if redis.call('HGET',KEYS[1],'purpose') ~= ARGV[1] then return 'INVALID' end
local state=redis.call('HGET',KEYS[1],'state')
if state=='LOCKED' then return 'LOCKED' end
if state~='SENT' then return 'INVALID' end
local attempts=tonumber(redis.call('HGET',KEYS[1],'attempts') or '0')
if attempts>=tonumber(ARGV[4]) then redis.call('HSET',KEYS[1],'state','LOCKED');return 'LOCKED' end
if redis.call('HGET',KEYS[1],'digest')~=ARGV[2] then
 attempts=redis.call('HINCRBY',KEYS[1],'attempts',1)
 if attempts>=tonumber(ARGV[4]) then redis.call('HSET',KEYS[1],'state','LOCKED');return 'LOCKED' end
 return 'INVALID'
end
redis.call('HSET',KEYS[1],'state','VERIFIED')
redis.call('HDEL',KEYS[1],'digest')
redis.call('SET',KEYS[2],ARGV[3],'EX',ARGV[5])
return 'VALID'`

func (s *Service) VerifyOtp(ctx context.Context, r *otpv1.VerifyOtpRequest) (*otpv1.VerifyOtpResponse, error) {
	if e := validate(r.Phone, r.Purpose); e != nil {
		return nil, e
	}
	if !codePattern.MatchString(r.Code) {
		return nil, status.Error(codes.InvalidArgument, "invalid code format")
	}
	unlock, e := s.lock(ctx, r.Phone)
	if e != nil {
		return nil, asInternal(e)
	}
	defer unlock()
	challengeID, e := s.DB.HGet(ctx, s.key(r.Phone), "id").Result()
	if errors.Is(e, redis.Nil) {
		s.Log.Info("otp_expired", "purpose", r.Purpose)
		return &otpv1.VerifyOtpResponse{Status: "EXPIRED"}, nil
	}
	if e != nil {
		return nil, asInternal(e)
	}
	proof, e := token(32)
	if e != nil {
		return nil, asInternal(e)
	}
	proofKey := "otp:proof:v1:" + s.hash(proof)
	proofValue := s.digest(r.Phone, r.Purpose)
	result, e := s.DB.Eval(ctx, verifyScript, []string{s.key(r.Phone), proofKey}, r.Purpose, s.digest(r.Phone, r.Purpose, challengeID, r.Code), proofValue, s.Config.MaxAttempts, int(s.Config.ProofTTL.Seconds())).Text()
	if e != nil {
		return nil, asInternal(e)
	}
	s.Log.Info("otp_verification", "status", result, "purpose", r.Purpose)
	if result == "VALID" {
		return &otpv1.VerifyOtpResponse{Status: result, Proof: proof, ProofExpiresInSeconds: int32(s.Config.ProofTTL.Seconds())}, nil
	}
	return &otpv1.VerifyOtpResponse{Status: result}, nil
}

func (s *Service) ConsumeProof(ctx context.Context, r *otpv1.ConsumeProofRequest) (*otpv1.ConsumeProofResponse, error) {
	if e := validate(r.Phone, r.Purpose); e != nil {
		return nil, e
	}
	if len(r.Proof) != 64 {
		return &otpv1.ConsumeProofResponse{}, nil
	}
	valid, e := s.DB.Eval(ctx, `local value=redis.call('GET',KEYS[1]); if value~=ARGV[1] then return 0 end; redis.call('DEL',KEYS[1]); return 1`, []string{"otp:proof:v1:" + s.hash(r.Proof)}, s.digest(r.Phone, r.Purpose)).Int()
	if e != nil {
		return nil, asInternal(e)
	}
	return &otpv1.ConsumeProofResponse{Valid: valid == 1}, nil
}

func (s *Service) CheckOtpStatus(ctx context.Context, r *otpv1.CheckOtpStatusRequest) (*otpv1.CheckOtpStatusResponse, error) {
	if len(r.ChallengeId) != 32 {
		return nil, status.Error(codes.InvalidArgument, "invalid challenge id")
	}
	// The index contains only a key reference and never a phone number.
	ref, e := s.DB.Get(ctx, "otp:challenge-index:v1:"+r.ChallengeId).Result()
	if errors.Is(e, redis.Nil) {
		return &otpv1.CheckOtpStatusResponse{Status: "EXPIRED"}, nil
	}
	if e != nil {
		return nil, asInternal(e)
	}
	data, e := s.DB.HGetAll(ctx, ref).Result()
	if e != nil {
		return nil, asInternal(e)
	}
	if data["id"] != r.ChallengeId {
		return &otpv1.CheckOtpStatusResponse{Status: "EXPIRED"}, nil
	}
	ttl, e := s.DB.TTL(ctx, ref).Result()
	if e != nil {
		return nil, asInternal(e)
	}
	return &otpv1.CheckOtpStatusResponse{Status: data["state"], ExpiresInSeconds: int32(max(0, int(ttl.Seconds())))}, nil
}

func asInternal(e error) error {
	if _, ok := status.FromError(e); ok {
		return e
	}
	return status.Error(codes.Unavailable, "OTP storage unavailable")
}
