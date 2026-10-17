// Command protoc-gen-rpcquic is a protoc plugin that generates quic-rpc-go
// clients and servers for the services of .proto files:
//
//	protoc --go_out=. --go_opt=paths=source_relative \
//	       --rpcquic_out=. --rpcquic_opt=paths=source_relative \
//	       chat/v1/chat.proto
//
// The code goes to a sub-package named after the Go package of the messages
// plus the suffix set by package_suffix, "quic" by default: chatv1quic for
// chatv1. Its names are those of protoc-gen-go-grpc, so both can be
// generated from one .proto and switching transports is a change of import.
// An empty package_suffix generates into the package of the messages.
//
// For now only unary and bidirectional streaming methods are supported.
package main

import (
	"flag"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

// version is printed into generated files; bump it when their content changes.
const version = "v0.1.0"

type options struct {
	packageSuffix string
}

func newFlags() (*flag.FlagSet, *options) {
	o := &options{}
	var flags flag.FlagSet
	flags.StringVar(&o.packageSuffix, "package_suffix", "quic",
		"suffix of the sub-package the code is generated into; empty means the package of the messages")
	return &flags, o
}

func run(o *options) func(*protogen.Plugin) error {
	return func(gen *protogen.Plugin) error {
		gen.SupportedFeatures = uint64(pluginpb.CodeGeneratorResponse_FEATURE_PROTO3_OPTIONAL) |
			uint64(pluginpb.CodeGeneratorResponse_FEATURE_SUPPORTS_EDITIONS)
		gen.SupportedEditionsMinimum = descriptorpb.Edition_EDITION_PROTO2
		gen.SupportedEditionsMaximum = descriptorpb.Edition_EDITION_2024
		for _, f := range gen.Files {
			if !f.Generate {
				continue
			}
			if err := generateFile(gen, f, o.packageSuffix); err != nil {
				return err
			}
		}
		return nil
	}
}

func main() {
	flags, o := newFlags()
	protogen.Options{ParamFunc: flags.Set}.Run(run(o))
}
