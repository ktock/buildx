package control

import (
	"context"
	"io"
	"syscall"

	"github.com/containerd/console"
	controllerapi "github.com/docker/buildx/controller/pb"
	solverpb "github.com/moby/buildkit/solver/pb"
)

type BuildxController interface {
	Invoke(ctx context.Context, ref string, options controllerapi.ContainerConfig, ioIn io.ReadCloser, ioOut io.WriteCloser, ioErr io.WriteCloser, signalCh <-chan syscall.Signal, resizeCh <-chan WinSize) error
	Build(ctx context.Context, options controllerapi.BuildOptions, in io.ReadCloser, w io.Writer, out console.File, progressMode string) (ref string, def *solverpb.Definition, err error)
	Kill(ctx context.Context) error
	Close() error
	List(ctx context.Context) (res []string, _ error)
	Disconnect(ctx context.Context, ref string) error
	Inspect(ctx context.Context, ref string) (*controllerapi.InspectResponse, error)
	Continue(ctx context.Context, ref string, def *solverpb.Definition, w io.Writer, out console.File, progressMode string) error
}

type ControlOptions struct {
	ServerConfig string
	Root         string
	Detach       bool
}

type WinSize struct {
	Rows uint32
	Cols uint32
}
