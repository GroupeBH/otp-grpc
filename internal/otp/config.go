package otp

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddress  string
	RedisAddress   string
	RedisPassword  string
	RedisDB        int
	RedisTLS       bool
	HashKey        string
	Provider       string
	MobiUser       string
	MobiPassword   string
	MobiSender     string
	CountryCode    string
	TTL            time.Duration
	ProofTTL       time.Duration
	Cooldown       time.Duration
	MaxAttempts    int
	PhoneHourly    int
	PhoneDaily     int
	IPHourly       int
	IdentityHourly int
}

func env(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}
func number(name string, fallback int) (int, error) {
	v := env(name, strconv.Itoa(fallback))
	n, e := strconv.Atoi(v)
	if e != nil || n < 1 {
		return 0, errors.New("invalid " + name)
	}
	return n, nil
}

func LoadConfig() (Config, error) {
	c := Config{ListenAddress: env("OTP_LISTEN_ADDRESS", ":50051"), RedisAddress: env("OTP_REDIS_ADDRESS", "localhost:6379"), RedisPassword: os.Getenv("OTP_REDIS_PASSWORD"), HashKey: os.Getenv("OTP_HASH_KEY"), Provider: env("OTP_SMS_PROVIDER", "mock"), MobiUser: os.Getenv("MOBISHASTRA_USER"), MobiPassword: os.Getenv("MOBISHASTRA_PASSWORD"), MobiSender: os.Getenv("MOBISHASTRA_SENDER_ID"), CountryCode: env("MOBISHASTRA_COUNTRY_CODE", "ALL")}
	var e error
	if c.RedisDB, e = strconv.Atoi(env("OTP_REDIS_DB", "0")); e != nil || c.RedisDB < 0 || c.RedisDB > 15 {
		return c, errors.New("invalid OTP_REDIS_DB")
	}
	if c.RedisTLS, e = strconv.ParseBool(env("OTP_REDIS_TLS", "false")); e != nil {
		return c, errors.New("invalid OTP_REDIS_TLS")
	}
	if c.TTL, e = duration("OTP_TTL_SECONDS", 300); e != nil {
		return c, e
	}
	if c.ProofTTL, e = duration("OTP_PROOF_TTL_SECONDS", 120); e != nil {
		return c, e
	}
	if c.Cooldown, e = duration("OTP_RESEND_COOLDOWN_SECONDS", 60); e != nil {
		return c, e
	}
	if c.MaxAttempts, e = number("OTP_MAX_ATTEMPTS", 5); e != nil {
		return c, e
	}
	if c.PhoneHourly, e = number("OTP_PHONE_HOURLY_LIMIT", 5); e != nil {
		return c, e
	}
	if c.PhoneDaily, e = number("OTP_PHONE_DAILY_LIMIT", 10); e != nil {
		return c, e
	}
	if c.IPHourly, e = number("OTP_IP_HOURLY_LIMIT", 20); e != nil {
		return c, e
	}
	if c.IdentityHourly, e = number("OTP_IDENTITY_HOURLY_LIMIT", 10); e != nil {
		return c, e
	}
	if len(c.HashKey) < 32 {
		return c, errors.New("OTP_HASH_KEY must contain at least 32 characters")
	}
	if c.Provider != "mock" && c.Provider != "mobishastra" {
		return c, errors.New("invalid OTP_SMS_PROVIDER")
	}
	if c.Provider == "mobishastra" && (c.MobiUser == "" || c.MobiPassword == "" || c.MobiSender == "") {
		return c, errors.New("Mobishastra credentials and sender ID are required")
	}
	if env("APP_ENV", "") == "production" && c.Provider == "mock" {
		return c, errors.New("mock SMS provider is forbidden in production")
	}
	return c, nil
}

func duration(name string, fallback int) (time.Duration, error) {
	n, e := number(name, fallback)
	if e != nil {
		return 0, e
	}
	if n > 86400 {
		return 0, errors.New("invalid " + name)
	}
	return time.Duration(n) * time.Second, nil
}
