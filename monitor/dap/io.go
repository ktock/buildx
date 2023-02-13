package dap

// Ported from https://github.com/ktock/buildg/blob/v0.4.1/pkg/dap/dap.go
// Copyright The buildg Authors.
// Licensed under the Apache License, Version 2.0

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/containerd/console"
	"github.com/containerd/fifo"
	mobysignal "github.com/moby/sys/signal"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
)

func AttachContainerIO(root string, setTtyRaw bool) error {
	if root == "" {
		return errors.Errorf("root needs to be specified")
	}

	type ioSet struct {
		stdin  io.WriteCloser
		stdout io.ReadCloser
		stderr io.ReadCloser
	}
	ioSetCh := make(chan ioSet)
	errCh := make(chan error)
	go func() {
		stdin, stdout, stderr, err := openFifosClient(context.TODO(), root)
		if err != nil {
			errCh <- err
			return
		}
		ioSetCh <- ioSet{stdin, stdout, stderr}
	}()
	var (
		stdin  io.WriteCloser
		stdout io.ReadCloser
		stderr io.ReadCloser
	)
	select {
	case ioSet := <-ioSetCh:
		stdin, stdout, stderr = ioSet.stdin, ioSet.stdout, ioSet.stderr
	case err := <-errCh:
		return err
	case <-time.After(3 * time.Second):
		return errors.Errorf("i/o timeout; check server is up and running")
	}
	defer func() { stdin.Close(); stdout.Close(); stderr.Close() }()

	sigToName := map[syscall.Signal]string{}
	for name, value := range mobysignal.SignalMap {
		sigToName[value] = name
	}
	ch := make(chan os.Signal, 1)
	signals := []os.Signal{syscall.SIGWINCH, syscall.SIGINT, syscall.SIGTERM}
	signal.Notify(ch, signals...)
	go func() {
		sockPath := filepath.Join(root, "signal.sock")
		c := http.Client{
			Transport: &http.Transport{
				DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", sockPath)
				},
			},
		}
		defer signal.Stop(ch)
		for ss := range ch {
			sigName, ok := sigToName[ss.(syscall.Signal)]
			if !ok {
				continue
			}
			v := url.Values{}
			v.Set("signal", sigName)
			if _, err := c.PostForm("http://localhost", v); err != nil {
				fmt.Fprintf(os.Stderr, "failed to send signal: %v\n", err)
			}
		}
	}()

	if setTtyRaw {
		con := console.Current()
		if err := con.SetRaw(); err != nil {
			return errors.Errorf("failed to configure terminal: %v", err)
		}
		defer con.Reset()
	}

	go io.Copy(stdin, os.Stdin)
	eg, _ := errgroup.WithContext(context.TODO())
	eg.Go(func() error { _, err := io.Copy(os.Stdout, stdout); return err })
	eg.Go(func() error { _, err := io.Copy(os.Stderr, stderr); return err })
	if err := eg.Wait(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "exec finished\n")
	return nil
}

func serveContainerIO(ctx context.Context, root string) (io.ReadCloser, io.WriteCloser, io.WriteCloser, <-chan syscall.Signal, func(), error) {
	stdin, stdout, stderr, err := openFifosServer(ctx, root)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	var srv http.Server
	sf := newSignalForwarder()
	srv.Handler = sf
	go func() {
		ln, err := net.Listen("unix", filepath.Join(root, "signal.sock"))
		if err != nil {
			logrus.WithError(err).Warnf("failed to listen contaienr IO")
			return
		}
		if err := srv.Serve(ln); err != nil {
			logrus.WithError(err).Warnf("failed to serve contaienr IO")
		}
	}()
	return stdin, stdout, stderr, sf.signalCh, func() {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			logrus.WithError(err).Warnf("failed to gracefully shutdown container IO")
		}
		srv.Close()
		stdin.Close()
		stdout.Close()
		stderr.Close()
	}, nil
}

func openFifosClient(ctx context.Context, fifosDir string) (stdin io.WriteCloser, stdout, stderr io.ReadCloser, retErr error) {
	if stdin, retErr = fifo.OpenFifo(ctx, filepath.Join(fifosDir, "stdin"), syscall.O_WRONLY, 0700); retErr != nil {
		return nil, nil, nil, errors.Errorf("failed to open stdin fifo: %v", retErr)
	}
	defer func() {
		if retErr != nil && stdin != nil {
			stdin.Close()
		}
	}()
	if stdout, retErr = fifo.OpenFifo(ctx, filepath.Join(fifosDir, "stdout"), syscall.O_RDONLY, 0700); retErr != nil {
		return nil, nil, nil, errors.Errorf("failed to open stdout fifo: %v", retErr)
	}
	defer func() {
		if retErr != nil && stdout != nil {
			stdout.Close()
		}
	}()
	if stderr, retErr = fifo.OpenFifo(ctx, filepath.Join(fifosDir, "stderr"), syscall.O_RDONLY, 0700); retErr != nil {
		return nil, nil, nil, errors.Errorf("failed to open stderr fifo: %v", retErr)
	}
	defer func() {
		if retErr != nil && stderr != nil {
			stderr.Close()
		}
	}()
	return stdin, stdout, stderr, nil
}

func openFifosServer(ctx context.Context, fifosDir string) (stdin io.ReadCloser, stdout, stderr io.WriteCloser, retErr error) {
	if stdin, retErr = fifo.OpenFifo(ctx, filepath.Join(fifosDir, "stdin"), syscall.O_RDONLY|syscall.O_CREAT|syscall.O_NONBLOCK, 0700); retErr != nil {
		return nil, nil, nil, errors.Errorf("failed to open stdin fifo: %v", retErr)
	}
	defer func() {
		if retErr != nil && stdin != nil {
			stdin.Close()
		}
	}()
	if stdout, retErr = fifo.OpenFifo(ctx, filepath.Join(fifosDir, "stdout"), syscall.O_WRONLY|syscall.O_CREAT|syscall.O_NONBLOCK, 0700); retErr != nil {
		return nil, nil, nil, errors.Errorf("failed to open stdout fifo: %v", retErr)
	}
	defer func() {
		if retErr != nil && stdout != nil {
			stdout.Close()
		}
	}()
	if stderr, retErr = fifo.OpenFifo(ctx, filepath.Join(fifosDir, "stderr"), syscall.O_WRONLY|syscall.O_CREAT|syscall.O_NONBLOCK, 0700); retErr != nil {
		return nil, nil, nil, errors.Errorf("failed to open stderr fifo: %v", retErr)
	}
	defer func() {
		if retErr != nil && stderr != nil {
			stderr.Close()
		}
	}()
	return stdin, stdout, stderr, nil
}

func newSignalForwarder() *signalForwarder {
	return &signalForwarder{
		signalCh: make(chan syscall.Signal),
	}
}

type signalForwarder struct {
	signalCh chan syscall.Signal
}

func (s *signalForwarder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sig := r.PostFormValue("signal")
	syscallSignal, ok := mobysignal.SignalMap[sig]
	if !ok {
		logrus.Warnf("unknown signal: %q", sig)
		return
	}
	s.signalCh <- syscallSignal
}
