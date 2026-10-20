// Command bench runs the parts of a benchmark (GEN-1).
//
// Usage:
//
//	bench certs -dir DIR [-hosts HOST,...]
//	bench server -transport quic|grpc-h2 -addr HOST:PORT -certs DIR
//
// certs issues a throwaway CA and a server certificate into DIR, for the
// server and its clients to share. server answers the calls of the
// benchmark service on addr with the certificate from DIR; it prints
// "listening on ADDR" once it accepts connections and stops on SIGINT or
// SIGTERM.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/VladimirKhmelev/quic-rpc-go/bench/common"
	"github.com/VladimirKhmelev/quic-rpc-go/internal/certs"
	"github.com/VladimirKhmelev/quic-rpc-go/testbed/netns"
)

const usage = `usage:
  bench certs -dir DIR [-hosts HOST,...]
  bench server -transport quic|grpc-h2 -addr HOST:PORT -certs DIR
`

func main() {
	code, err := run(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

var errUsage = errors.New("bad arguments")

func run(args []string) (int, error) {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2, nil
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	switch args[0] {
	case "certs":
		dir := fs.String("dir", "", "write the certificates to `DIR`")
		hosts := fs.String("hosts", "localhost,"+netns.ServerAddr.String(), "names and addresses the server certificate is valid for")
		if err := fs.Parse(args[1:]); err != nil {
			return 2, nil // flag has printed the error
		}
		if *dir == "" || fs.NArg() != 0 {
			return 2, fmt.Errorf("%w: certs needs -dir", errUsage)
		}
		b, err := certs.Issue(strings.Split(*hosts, ",")...)
		if err != nil {
			return 1, err
		}
		return 0, b.Write(*dir)
	case "server":
		transport := fs.String("transport", "", "serve with `TRANSPORT`: quic or grpc-h2")
		addr := fs.String("addr", "", "listen on `HOST:PORT`")
		certsDir := fs.String("certs", "", "read the certificates from `DIR`")
		if err := fs.Parse(args[1:]); err != nil {
			return 2, nil // flag has printed the error
		}
		if *addr == "" || *certsDir == "" || fs.NArg() != 0 {
			return 2, fmt.Errorf("%w: server needs -transport, -addr and -certs", errUsage)
		}
		t, err := common.ParseTransport(*transport)
		if err != nil {
			return 2, fmt.Errorf("%w: %w", errUsage, err)
		}
		return serve(t, *addr, *certsDir)
	default:
		fmt.Fprint(os.Stderr, usage)
		return 2, nil
	}
}

func serve(t common.Transport, addr, certsDir string) (int, error) {
	b, err := certs.Load(certsDir)
	if err != nil {
		return 1, err
	}
	p, err := b.Pair()
	if err != nil {
		return 1, err
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	srv, err := common.Listen(t, addr, p.Server)
	if err != nil {
		return 1, err
	}
	fmt.Println("listening on", srv.Addr())
	<-stop
	srv.Stop()
	return 0, nil
}
