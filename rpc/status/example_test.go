package status_test

import (
	"errors"
	"fmt"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/status"
)

func ExampleError() {
	// A handler reports a missing entity.
	err := status.Error(codes.NotFound, "chat 42 not found")

	// The caller checks the code.
	if status.Code(err) == codes.NotFound {
		fmt.Println("not found:", status.Convert(err).Message())
	}
	// Output: not found: chat 42 not found
}

func ExampleFromError() {
	err := fmt.Errorf("load chat: %w", status.Error(codes.Unavailable, "database is down"))

	s, ok := status.FromError(err)
	fmt.Println(ok, s.Code(), s.Message())

	s, ok = status.FromError(errors.New("plain error"))
	fmt.Println(ok, s.Code(), s.Message())
	// Output:
	// true Unavailable load chat: rpc error: code = Unavailable desc = database is down
	// false Unknown plain error
}

func ExampleStatus_WithDetails() {
	s, err := status.New(codes.ResourceExhausted, "rate limit").WithDetails(wrapperspb.String("retry in 3s"))
	if err != nil {
		panic(err)
	}
	for _, d := range s.Details() {
		if hint, ok := d.(*wrapperspb.StringValue); ok {
			fmt.Println(s.Code(), "-", hint.GetValue())
		}
	}
	// Output: ResourceExhausted - retry in 3s
}
