// Command sandbox-helper is the controller-owned static helper image-mounted into
// attempts (ADR 0015). It holds no credentials and trusts no attempt input beyond
// the bytes it forwards.
package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

// attemptUser owns workdir volumes; it matches the attempt --user.
const attemptUser = 65532

func main() {
	var err error
	switch {
	case len(os.Args) == 3 && os.Args[1] == "init":
		err = initialize(os.Args[2])
	case len(os.Args) >= 4 && len(os.Args)%2 == 0 && os.Args[1] == "forward":
		errs := make(chan error, len(os.Args))
		for i := 2; i < len(os.Args); i += 2 {
			go func(listen, target string) { errs <- forward(listen, target) }(os.Args[i], os.Args[i+1])
		}
		err = <-errs
	default:
		err = fmt.Errorf("usage: sandbox-helper init DIR | forward LISTEN TARGET [LISTEN TARGET]...")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// initialize hands a fresh volume root to the attempt user. With only CAP_CHOWN the
// mode changes first, while root still owns the directory.
func initialize(dir string) error {
	if err := os.Chmod(dir, 0700); err != nil {
		return err
	}
	return os.Chown(dir, attemptUser, attemptUser)
}

// forward relays loopback TCP connections to the controller endpoint on the attempt network.
func forward(listen, target string) error {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	slots := make(chan struct{}, 16)
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		select {
		case slots <- struct{}{}:
		default:
			_ = c.Close()
			continue
		}
		go func() {
			defer func() { <-slots }()
			defer c.Close()
			u, err := net.DialTimeout("tcp", target, 10*time.Second)
			if err != nil {
				return
			}
			defer u.Close()
			done := make(chan struct{})
			go func() {
				_, _ = io.Copy(u, c)
				_ = u.(*net.TCPConn).CloseWrite()
				close(done)
			}()
			_, _ = io.Copy(c, u)
			_ = c.(*net.TCPConn).CloseWrite()
			<-done
		}()
	}
}
