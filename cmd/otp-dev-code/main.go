// otp-dev-code reads the local mock SMS outbox. It is not included in the production image.
package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

func main() {
	if os.Getenv("APP_ENV") == "production" || len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: go run ./cmd/otp-dev-code +243...")
		os.Exit(2)
	}
	dbIndex, _ := strconv.Atoi(os.Getenv("OTP_REDIS_DB"))
	address := os.Getenv("OTP_REDIS_ADDRESS")
	if address == "" {
		address = "localhost:6379"
	}
	options := &redis.Options{Addr: address, Password: os.Getenv("OTP_REDIS_PASSWORD"), DB: dbIndex}
	if os.Getenv("OTP_REDIS_TLS") == "true" {
		options.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	client := redis.NewClient(options)
	defer client.Close()
	sum := sha256.Sum256([]byte(os.Args[1]))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	code, e := client.Get(ctx, "otp:mock-outbox:"+hex.EncodeToString(sum[:])).Result()
	if e != nil {
		fmt.Fprintln(os.Stderr, "mock code unavailable")
		os.Exit(1)
	}
	fmt.Println(code)
}
