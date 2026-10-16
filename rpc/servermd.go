package rpc

import (
	"context"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/metadata"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/status"
)

func serverStreamFrom(ctx context.Context) (*serverStream, error) {
	if ss, ok := ctx.Value(serverStreamKey{}).(*serverStream); ok {
		return ss, nil
	}
	return nil, status.Error(codes.Internal, "rpc: the context does not belong to a server call")
}

// SetHeader adds header metadata to the call whose handler got ctx. It is
// sent with the response or with SendHeader.
func SetHeader(ctx context.Context, md metadata.MD) error {
	ss, err := serverStreamFrom(ctx)
	if err != nil {
		return err
	}
	return ss.SetHeader(md)
}

// SendHeader sends the header metadata of the call whose handler got ctx,
// adding md to it.
func SendHeader(ctx context.Context, md metadata.MD) error {
	ss, err := serverStreamFrom(ctx)
	if err != nil {
		return err
	}
	return ss.SendHeader(md)
}

// SetTrailer adds trailer metadata to the call whose handler got ctx. It is
// sent when the handler returns.
func SetTrailer(ctx context.Context, md metadata.MD) error {
	ss, err := serverStreamFrom(ctx)
	if err != nil {
		return err
	}
	ss.SetTrailer(md)
	return nil
}
