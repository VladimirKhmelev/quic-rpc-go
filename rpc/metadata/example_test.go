package metadata_test

import (
	"context"
	"fmt"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/metadata"
)

func ExampleAppendToOutgoingContext() {
	// The client attaches a token to every call made with ctx.
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer abc")

	md, _ := metadata.FromOutgoingContext(ctx)
	fmt.Println(md.Get("authorization"))
	// Output: [Bearer abc]
}

func ExampleFromIncomingContext() {
	// On the server the call layer puts the received metadata into the
	// handler's context; NewIncomingContext stands in for it here.
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-request-id", "r-1"))

	if md, ok := metadata.FromIncomingContext(ctx); ok {
		fmt.Println(md.Get("x-request-id"))
	}
	// Output: [r-1]
}

func ExampleMD_String() {
	md := metadata.Pairs("authorization", "Bearer secret")
	fmt.Println(md)                      // safe to log
	fmt.Println(map[string][]string(md)) // everything, on purpose
	// Output:
	// map[<1 redacted>]
	// map[authorization:[Bearer secret]]
}
