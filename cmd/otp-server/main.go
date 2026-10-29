package main

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	otpv1 "otp-grpc/gen/otp/v1"
	"otp-grpc/internal/otp"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "health" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		conn, err := grpc.NewClient("127.0.0.1:50051", grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			os.Exit(1)
		}
		defer conn.Close()
		response, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{Service: "otp.v1.OtpService"})
		if err != nil || response.Status != healthpb.HealthCheckResponse_SERVING {
			os.Exit(1)
		}
		return
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	config, e := otp.LoadConfig()
	if e != nil {
		log.Error("configuration_invalid", "error", e)
		os.Exit(1)
	}
	redisOptions := &redis.Options{Addr: config.RedisAddress, Password: config.RedisPassword, DB: config.RedisDB}
	if config.RedisTLS {
		redisOptions.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	db := redis.NewClient(redisOptions)
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if e = db.Ping(ctx).Err(); e != nil {
		log.Error("redis_unavailable", "error", e)
		os.Exit(1)
	}
	var sms otp.SMSProvider
	if config.Provider == "mock" {
		sms = otp.MockProvider{DB: db}
	} else {
		sms = otp.MobishastraProvider{User: config.MobiUser, Password: config.MobiPassword, Sender: config.MobiSender, CountryCode: config.CountryCode}
	}
	serviceToken := os.Getenv("OTP_SERVICE_TOKEN")
	if os.Getenv("APP_ENV") == "production" && len(serviceToken) < 32 {
		log.Error("OTP_SERVICE_TOKEN must be at least 32 characters in production")
		os.Exit(1)
	}
	interceptor := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if serviceToken != "" && info.FullMethod != "/grpc.health.v1.Health/Check" {
			md, _ := metadata.FromIncomingContext(ctx)
			values := md.Get("x-otp-service-token")
			if len(values) != 1 || subtle.ConstantTimeCompare([]byte(values[0]), []byte(serviceToken)) != 1 {
				return nil, status.Error(codes.Unauthenticated, "service authentication failed")
			}
		}
		return handler(ctx, req)
	}
	server := grpc.NewServer(grpc.UnaryInterceptor(interceptor))
	otpv1.RegisterOtpServiceServer(server, otp.NewService(db, sms, config, log))
	h := health.NewServer()
	h.SetServingStatus("otp.v1.OtpService", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(server, h)
	healthCtx, stopHealth := context.WithCancel(context.Background())
	defer stopHealth()
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-healthCtx.Done():
				return
			case <-ticker.C:
				pingCtx, cancel := context.WithTimeout(healthCtx, 2*time.Second)
				err := db.Ping(pingCtx).Err()
				cancel()
				if err != nil {
					h.SetServingStatus("otp.v1.OtpService", healthpb.HealthCheckResponse_NOT_SERVING)
				} else {
					h.SetServingStatus("otp.v1.OtpService", healthpb.HealthCheckResponse_SERVING)
				}
			}
		}
	}()
	listener, e := net.Listen("tcp", config.ListenAddress)
	if e != nil {
		log.Error("listen_failed", "error", e)
		os.Exit(1)
	}
	done := make(chan os.Signal, 1)
	signal.Notify(done, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-done
		stopHealth()
		h.Shutdown()
		ch := make(chan struct{})
		go func() { server.GracefulStop(); close(ch) }()
		select {
		case <-ch:
		case <-time.After(10 * time.Second):
			server.Stop()
		}
	}()
	log.Info("otp_server_started", "address", config.ListenAddress, "provider", config.Provider)
	if e = server.Serve(listener); e != nil {
		log.Error("grpc_server_stopped", "error", e)
		os.Exit(1)
	}
}
