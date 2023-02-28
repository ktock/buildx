package controller

import (
	"context"
	"io"

	"github.com/containerd/console"
	"github.com/docker/buildx/controller/control"
	"github.com/docker/buildx/controller/local"
	controllerapi "github.com/docker/buildx/controller/pb"
	"github.com/docker/buildx/controller/remote"
	"github.com/docker/buildx/util/progress"
	"github.com/docker/cli/cli/command"
	"github.com/moby/buildkit/client"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
)

func NewController(ctx context.Context, opts control.ControlOptions, dockerCli command.Cli) (c control.BuildxController, err error) {
	if !opts.Detach {
		logrus.Infof("launching local buildx controller")
		c = local.NewLocalBuildxController(ctx, dockerCli)
		return c, nil
	}

	logrus.Infof("connecting to buildx server")
	c, err = remote.NewRemoteBuildxController(ctx, dockerCli, opts)
	if err != nil {
		return nil, errors.Wrap(err, "failed to use buildx server; use --detach=false")
	}
	return c, nil
}

// Build is a helper function that builds the build and prints the status using the controller.
func Build(ctx context.Context, c control.BuildxController, options controllerapi.BuildOptions, in io.ReadCloser, w io.Writer, out console.File, progressMode string) (ref string, resp *client.SolveResponse, err error) {
	pw, err := progress.NewPrinter(context.TODO(), w, out, progressMode)
	if err != nil {
		return "", nil, err
	}
	statusChan := make(chan *client.SolveStatus)
	statusDone := make(chan struct{})
	eg, egCtx := errgroup.WithContext(ctx)
	eg.Go(func() error {
		defer close(statusChan)
		var err error
		ref, resp, err = c.Build(egCtx, options, in, statusChan)
		return err
	})
	eg.Go(func() error {
		defer close(statusDone)
		for s := range statusChan {
			st := s
			pw.Write(st)
		}
		return nil
	})
	eg.Go(func() error {
		<-statusDone
		return pw.Wait()
	})
	return ref, resp, eg.Wait()
}
