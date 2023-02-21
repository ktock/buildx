package local

import (
	"context"
	"io"

	"github.com/containerd/console"
	"github.com/docker/buildx/build"
	cbuild "github.com/docker/buildx/controller/build"
	"github.com/docker/buildx/controller/control"
	controllererrors "github.com/docker/buildx/controller/errdefs"
	controllerapi "github.com/docker/buildx/controller/pb"
	"github.com/docker/cli/cli/command"
	"github.com/pkg/errors"
)

func NewLocalBuildxController(ctx context.Context, dockerCli command.Cli) control.BuildxController {
	return &localController{
		dockerCli: dockerCli,
		ref:       "local",
	}
}

type localController struct {
	dockerCli    command.Cli
	ref          string
	resultCtx    *build.ResultContext
	buildOptions *controllerapi.BuildOptions
}

func (b *localController) Invoke(ctx context.Context, ref string, cfg controllerapi.ContainerConfig, ioIn io.ReadCloser, ioOut io.WriteCloser, ioErr io.WriteCloser) error {
	if ref != b.ref {
		return errors.Errorf("unknown ref %q", ref)
	}
	if b.resultCtx == nil {
		return errors.New("no build result is registered")
	}
	ccfg := build.ContainerConfig{
		ResultCtx:  b.resultCtx,
		Entrypoint: cfg.Entrypoint,
		Cmd:        cfg.Cmd,
		Env:        cfg.Env,
		Tty:        cfg.Tty,
		Stdin:      ioIn,
		Stdout:     ioOut,
		Stderr:     ioErr,
		Initial:    cfg.Initial,
	}
	if !cfg.NoUser {
		ccfg.User = &cfg.User
	}
	if !cfg.NoCwd {
		ccfg.Cwd = &cfg.Cwd
	}
	return build.Invoke(ctx, ccfg)
}

func (b *localController) Build(ctx context.Context, options controllerapi.BuildOptions, in io.ReadCloser, w io.Writer, out console.File, progressMode string) (string, error) {
	res, buildErr := cbuild.RunBuild(ctx, b.dockerCli, options, in, progressMode, nil)
	if buildErr != nil {
		var re *cbuild.ResultContextError
		if errors.As(buildErr, &re) && re.ResultContext != nil {
			res = re.ResultContext
		}
	}
	if res != nil {
		b.resultCtx = res
		b.buildOptions = &options
		if buildErr != nil {
			buildErr = controllererrors.WrapBuild(buildErr, b.ref)
		}
	}
	if buildErr != nil {
		return "", buildErr
	}
	return b.ref, nil
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
