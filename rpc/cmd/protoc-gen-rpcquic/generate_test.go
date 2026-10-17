package main

import (
	"flag"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata/golden")

const echoProto = "echo/v1/echo.proto"

// request builds a CodeGeneratorRequest for testdata/echo/v1/echo.proto as
// protoc 3.21.12 would send it. edit, if not nil, changes the file first.
func request(t *testing.T, param string, edit func(*descriptorpb.FileDescriptorProto)) *pluginpb.CodeGeneratorRequest {
	t.Helper()
	b, err := os.ReadFile("testdata/echo.binpb")
	if err != nil {
		t.Fatalf("read descriptor set: %v", err)
	}
	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(b, &set); err != nil {
		t.Fatalf("unmarshal descriptor set: %v", err)
	}
	if edit != nil {
		edit(set.File[len(set.File)-1])
	}
	return &pluginpb.CodeGeneratorRequest{
		FileToGenerate:  []string{echoProto},
		Parameter:       proto.String(param),
		ProtoFile:       set.File,
		CompilerVersion: &pluginpb.Version{Major: proto.Int32(3), Minor: proto.Int32(21), Patch: proto.Int32(12)},
	}
}

func generate(t *testing.T, req *pluginpb.CodeGeneratorRequest) *pluginpb.CodeGeneratorResponse {
	t.Helper()
	flags, o := newFlags()
	gen, err := protogen.Options{ParamFunc: flags.Set}.New(req)
	if err != nil {
		t.Fatalf("protogen: %v", err)
	}
	if err := run(o)(gen); err != nil {
		gen.Error(err)
	}
	return gen.Response()
}

// generateOne runs the plugin and returns its only file, checking its name
// and package clause.
func generateOne(t *testing.T, param, wantFile, wantPkg string) string {
	t.Helper()
	resp := generate(t, request(t, param, nil))
	if resp.Error != nil {
		t.Fatalf("plugin error: %s", resp.GetError())
	}
	if len(resp.File) != 1 || resp.File[0].GetName() != wantFile {
		t.Fatalf("generated %v, want only %s", names(resp), wantFile)
	}
	content := resp.File[0].GetContent()
	f, err := parser.ParseFile(token.NewFileSet(), wantFile, content, parser.PackageClauseOnly)
	if err != nil {
		t.Fatalf("generated code does not parse: %v", err)
	}
	if f.Name.Name != wantPkg {
		t.Fatalf("package %s, want %s", f.Name.Name, wantPkg)
	}
	return content
}

// TestGolden compares the output for the default options with a reviewed copy.
func TestGolden(t *testing.T) {
	const file = "echo/v1/echov1quic/echo.rpcquic.go"
	content := generateOne(t, "paths=source_relative", file, "echov1quic")
	golden := filepath.Join("testdata", "golden", filepath.FromSlash(file))
	if *update {
		if err := os.MkdirAll(filepath.Dir(golden), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden file (run with -update to create it): %v", err)
	}
	if content != string(want) {
		t.Fatalf("generated code differs from %s; run go test -update and review the diff", golden)
	}
}

func TestPackageSuffixOption(t *testing.T) {
	content := generateOne(t, "paths=source_relative,package_suffix=rpc", "echo/v1/echov1rpc/echo.rpcquic.go", "echov1rpc")
	if !strings.Contains(content, `"example.com/gen/echo/v1"`) {
		t.Error("the code in a sub-package does not import the package of the messages")
	}

	// Without a suffix the code joins the package of the messages.
	content = generateOne(t, "paths=source_relative,package_suffix=", "echo/v1/echo.rpcquic.go", "echov1")
	if strings.Contains(content, `"example.com/gen/echo/v1"`) || !strings.Contains(content, "in *Msg,") {
		t.Error("the code in the package of the messages qualifies or imports them")
	}
}

func TestUnsupportedStreaming(t *testing.T) {
	tests := []struct {
		kind           string
		client, server bool
	}{
		{"server-streaming", false, true},
		{"client-streaming", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			resp := generate(t, request(t, "paths=source_relative", func(f *descriptorpb.FileDescriptorProto) {
				m := f.Service[0].Method[1] // Echo.Bidi
				m.ClientStreaming, m.ServerStreaming = proto.Bool(tt.client), proto.Bool(tt.server)
			}))
			msg := resp.GetError()
			if !strings.Contains(msg, "test.echo.v1.Echo.Bidi is a "+tt.kind+" method") {
				t.Fatalf("error %q, want one naming Echo.Bidi as %s", msg, tt.kind)
			}
			if len(resp.File) != 0 {
				t.Fatalf("generated %v despite the error", names(resp))
			}
		})
	}
}

func TestFileWithoutServices(t *testing.T) {
	resp := generate(t, request(t, "paths=source_relative", func(f *descriptorpb.FileDescriptorProto) {
		f.Service = nil
	}))
	if resp.Error != nil || len(resp.File) != 0 {
		t.Fatalf("got error %q and files %v, want nothing", resp.GetError(), names(resp))
	}
}

func names(resp *pluginpb.CodeGeneratorResponse) []string {
	var out []string
	for _, f := range resp.File {
		out = append(out, f.GetName())
	}
	return out
}
