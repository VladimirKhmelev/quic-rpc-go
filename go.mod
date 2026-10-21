module github.com/VladimirKhmelev/quic-rpc-go

go 1.26.8

replace (
	github.com/VladimirKhmelev/quic-rpc-go/grpcbridge => ./grpcbridge
	github.com/VladimirKhmelev/quic-rpc-go/rpc => ./rpc
)

tool google.golang.org/grpc/cmd/protoc-gen-go-grpc

require (
	github.com/VladimirKhmelev/quic-rpc-go/grpcbridge v0.0.0-00010101000000-000000000000
	github.com/VladimirKhmelev/quic-rpc-go/rpc v0.0.0-00010101000000-000000000000
	go.yaml.in/yaml/v3 v3.0.5
	golang.org/x/sys v0.47.0
	google.golang.org/grpc v1.84.0
	google.golang.org/protobuf v1.36.12
)

require (
	github.com/quic-go/quic-go v0.63.0 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260706201446-f0a921348800 // indirect
	google.golang.org/grpc/cmd/protoc-gen-go-grpc v1.6.2 // indirect
)
