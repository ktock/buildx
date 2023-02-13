package dap

import (
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

func AddDAPCommands(cmd *cobra.Command, dockerCli command.Cli) {
	cmd.AddCommand(
		attachContainerCmd(dockerCli),
		dapCmd(dockerCli),
	)
}

func attachContainerCmd(dockerCli command.Cli) *cobra.Command {
	var setTtyRaw bool
	cmd := &cobra.Command{
		Use:    fmt.Sprintf("%s [OPTIONS] rootdir", AttachContainerCommand),
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) < 1 || args[0] == "" {
				return errors.Errorf("specify root dir: %+v", args)
			}
			return AttachContainerIO(args[0], setTtyRaw)
		},
	}
	flags := cmd.Flags()
	flags.BoolVar(&setTtyRaw, "set-tty-raw", false, "set tty raw")
	return cmd
}

func dapCmd(dockerCli command.Cli) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "dap",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			logrus.SetOutput(os.Stderr)
			s, err := NewServer(dockerCli, &stdioConn{os.Stdin, os.Stdout})
			if err != nil {
				return err
			}
			if err := s.Serve(); err != nil {
				logrus.WithError(err).Warnf("failed to serve") // TODO: should return error
			}
			logrus.Info("finishing server")
			return nil
		},
	}
	return cmd
}

type stdioConn struct {
	io.Reader
	io.Writer
}

func (c *stdioConn) Read(b []byte) (n int, err error) {
	return c.Reader.Read(b)
}
func (c *stdioConn) Write(b []byte) (n int, err error) {
	return c.Writer.Write(b)
}
func (c *stdioConn) Close() error                       { return nil }
func (c *stdioConn) LocalAddr() net.Addr                { return dummyAddr{} }
func (c *stdioConn) RemoteAddr() net.Addr               { return dummyAddr{} }
func (c *stdioConn) SetDeadline(t time.Time) error      { return nil }
func (c *stdioConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *stdioConn) SetWriteDeadline(t time.Time) error { return nil }

type dummyAddr struct{}

func (a dummyAddr) Network() string { return "dummy" }
func (a dummyAddr) String() string  { return "dummy" }
