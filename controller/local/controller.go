package local

import (
	"context"
	"io"
	"syscall"

	"github.com/containerd/console"
	"github.com/docker/buildx/build"
	cbuild "github.com/docker/buildx/controller/build"
	"github.com/docker/buildx/controller/control"
	controllererrors "github.com/docker/buildx/controller/errdefs"
	controllerapi "github.com/docker/buildx/controller/pb"
	"github.com/docker/cli/cli/command"
	gateway "github.com/moby/buildkit/frontend/gateway/client"
	solverpb "github.com/moby/buildkit/solver/pb"
	"github.com/pkg/errors"
)

func NewLocalBuildxController(ctx context.Context, dockerCli command.Cli) control.BuildxController {
	return &localController{
		dockerCli: dockerCli,
		ref:       "local",
	}
}

type localController struct {
	dockerCli      command.Cli
	ref            string
	result         *build.ResultContext
	originalResult *build.ResultContext
	buildOptions   *controllerapi.BuildOptions
}

// func (b *localController) Invoke(ctx context.Context, ref string, cfg controllerapi.ContainerConfig, ioIn io.ReadCloser, ioOut io.WriteCloser, ioErr io.WriteCloser) error {
func (b *localController) Invoke(ctx context.Context, ref string, cfg controllerapi.ContainerConfig, ioIn io.ReadCloser, ioOut io.WriteCloser, ioErr io.WriteCloser, signalCh <-chan syscall.Signal, resizeCh <-chan control.WinSize) error {
	if ref != b.ref {
		return errors.Errorf("unknown ref %q", ref)
	}
	if b.result == nil {
		return errors.New("no build result is registered")
	}
	ccfg := build.ContainerConfig{
		ResultCtx:       b.result,
		Entrypoint:      cfg.Entrypoint,
		Cmd:             cfg.Cmd,
		Env:             cfg.Env,
		Tty:             cfg.Tty,
		Stdin:           ioIn,
		Stdout:          ioOut,
		Stderr:          ioErr,
		Initial:         cfg.Initial,
		SignalCh:        signalCh,
		Image:           cfg.Image,
		ResultMountPath: cfg.ResultMountPath,
	}
	if resizeCh != nil {
		gwResizeCh := make(chan gateway.WinSize)
		doneCh := make(chan struct{})
		defer close(doneCh)
		go func() {
			for {
				select {
				case w := <-resizeCh:
					select {
					case gwResizeCh <- gateway.WinSize{Rows: w.Rows, Cols: w.Cols}:
					case <-doneCh:
						return
					}
				case <-doneCh:
					return
				}
			}
		}()
		ccfg.ResizeCh = gwResizeCh
	}
	if !cfg.NoUser {
		ccfg.User = &cfg.User
	}
	if !cfg.NoCwd {
		ccfg.Cwd = &cfg.Cwd
	}
	return build.Invoke(ctx, ccfg)
}

func (b *localController) Build(ctx context.Context, options controllerapi.BuildOptions, in io.ReadCloser, w io.Writer, out console.File, progressMode string) (ref string, def *solverpb.Definition, err error) {
	res, buildErr := cbuild.RunBuild(ctx, b.dockerCli, options, in, progressMode, nil)
	if buildErr != nil {
		var re *cbuild.ResultContextError
		if errors.As(buildErr, &re) && re.ResultContext != nil {
			res = re.ResultContext
			def = re.Definition
		}
	} else {
		var err error
		def, err = cbuild.DefinitionFromResultContext(ctx, res)
		if err != nil {
			return "", nil, errors.Errorf("failed to get definition from result: %v", err)
		}
	}
	if res != nil {
		b.result = res
		b.originalResult = res
		b.buildOptions = &options
		if buildErr != nil && def != nil {
			buildErr = controllererrors.WrapBuild(buildErr, b.ref, def)
		}
	}
	if buildErr != nil {
		return "", nil, buildErr
	}
	return b.ref, def, nil
}

func (b *localController) Kill(context.Context) error {
	return nil // nop
}

func (b *localController) Close() error {
	// TODO: cancel current build and invoke
	return nil
}

func (b *localController) List(ctx context.Context) (res []string, _ error) {
	return []string{b.ref}, nil
}

func (b *localController) Disconnect(ctx context.Context, key string) error {
	return nil // nop
}

func (b *localController) Inspect(ctx context.Context, ref string) (*controllerapi.InspectResponse, error) {
	if ref != b.ref {
		return nil, errors.Errorf("unknown ref %q", ref)
	}
	return &controllerapi.InspectResponse{Options: b.buildOptions}, nil
}

func (b *localController) Continue(ctx context.Context, ref string, target *solverpb.Definition, w io.Writer, out console.File, progressMode string) error {
	if ref != b.ref {
		return errors.Errorf("unknown ref %q", ref)
	}
	res, err := build.GetResultAt(ctx, b.originalResult, target, nil)
	if err == nil {
		b.result = res
		if res.Err != nil {
			err = errors.Errorf("failed continue: %v", res.Err)
		}
	}
	return err
}
