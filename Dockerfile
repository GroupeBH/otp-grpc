FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /otp-server ./cmd/otp-server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /otp-server /otp-server
EXPOSE 50051
USER nonroot:nonroot
ENTRYPOINT ["/otp-server"]
