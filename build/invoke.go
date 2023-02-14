package build

import (
	"context"
	_ "crypto/sha256" // ensure digests can be computed
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/exporter/containerimage/exptypes"
	gateway "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/solver/pb"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// InvokeConfig is configuration for a process to run in the container.
type InvokeConfig struct {
	Stdin  io.ReadCloser
	Stdout io.WriteCloser
	Stderr io.WriteCloser
	Tty    bool

	Entrypoint []string
	Cmd        []string
	Env        []string
	User       *string
	Cwd        *string
}

// ResultContext is a build result with the client that built it.
type ResultContext struct {
	Client *client.Client
	Res    *gateway.Result
}

type Container struct {
	cancelOnce      sync.Once
	containerCancel func()

	isUnavailable atomic.Bool

	initStarted atomic.Bool

	container gateway.Container
	image     *specs.Image

	releaseCh chan struct{}
}

func NewContainer(ctx context.Context, resultCtx *ResultContext) (*Container, error) {
	c, res := resultCtx.Client, resultCtx.Res

	mainCtx := ctx

	ctrCh := make(chan *Container)
	errCh := make(chan error)
	go func() {
		_, err := c.Build(context.TODO(), client.SolveOpt{}, "buildx", func(ctx context.Context, c gateway.Client) (*gateway.Result, error) {
			ctx, cancel := context.WithCancel(ctx)
			go func() {
				<-mainCtx.Done()
				cancel()
			}()

			if res.Ref == nil {
				return nil, errors.Errorf("no reference is registered")
			}
			st, err := res.Ref.ToState()
			if err != nil {
				return nil, err
			}
			def, err := st.Marshal(ctx)
			if err != nil {
				return nil, err
			}
			imgRef, err := c.Solve(ctx, gateway.SolveRequest{
				Definition: def.ToPB(),
			})
			if err != nil {
				return nil, err
			}
			containerCtx, containerCancel := context.WithCancel(ctx)
			defer containerCancel()
			bkContainer, err := c.NewContainer(containerCtx, gateway.NewContainerRequest{
				Mounts: []gateway.Mount{
					{
						Dest:      "/",
						MountType: pb.MountType_BIND,
						Ref:       imgRef.Ref,
					},
				},
			})
			if err != nil {
				return nil, err
			}
			imgData := res.Metadata[exptypes.ExporterImageConfigKey]
			var img *specs.Image
			if len(imgData) > 0 {
				img = &specs.Image{}
				if err := json.Unmarshal(imgData, img); err != nil {
					fmt.Println(err)
					return nil, err
				}
			}
			releaseCh := make(chan struct{})
			container := &Container{
				containerCancel: containerCancel,
				container:       bkContainer,
				image:           img,
				releaseCh:       releaseCh,
			}
			ctrCh <- container
			<-container.releaseCh

			return nil, bkContainer.Release(ctx)
		}, nil)
		if err != nil {
			errCh <- err
		}
	}()
	select {
	case ctr := <-ctrCh:
		return ctr, nil
	case err := <-errCh:
		return nil, err
	case <-mainCtx.Done():
		return nil, mainCtx.Err()
	}
}

func (c *Container) Cancel() {
	c.markUnavailable()
	c.cancelOnce.Do(func() {
		if c.containerCancel != nil {
			c.containerCancel()
		}
		close(c.releaseCh)
	})
}

func (c *Container) IsUnavailable() bool {
	return c.isUnavailable.Load()
}

func (c *Container) markUnavailable() {
	c.isUnavailable.Store(true)
}

func (c *Container) Exec(ctx context.Context, cfg *InvokeConfig) error {
	if isInit := c.initStarted.CompareAndSwap(false, true); isInit {
		defer func() {
			// container can't be used after init exits
			c.markUnavailable()
		}()
	}
	err := exec(ctx, cfg, c.container, c.image)
	if err != nil {
		// Container becomes unavailable if one of the processes fails in it.
		c.markUnavailable()
	}
	return err
}

func exec(ctx context.Context, cfg *InvokeConfig, ctr gateway.Container, img *specs.Image) error {
	user := ""
	if cfg.User != nil {
		user = *cfg.User
	} else if img != nil {
		user = img.Config.User
	}

	cwd := ""
	if cfg.Cwd != nil {
		cwd = *cfg.Cwd
	} else if img != nil {
		cwd = img.Config.WorkingDir
	}

	env := []string{}
	if img != nil {
		env = append(env, img.Config.Env...)
	}
	env = append(env, cfg.Env...)

	args := []string{}
	if cfg.Entrypoint != nil {
		args = append(args, cfg.Entrypoint...)
	}
	if cfg.Cmd != nil {
		args = append(args, cfg.Cmd...)
	}
	// caller should always set args
	if len(args) == 0 {
		return errors.Errorf("specify args to execute")
	}
	proc, err := ctr.Start(ctx, gateway.StartRequest{
		Args:   args,
		Env:    env,
		User:   user,
		Cwd:    cwd,
		Tty:    cfg.Tty,
		Stdin:  cfg.Stdin,
		Stdout: cfg.Stdout,
		Stderr: cfg.Stderr,
	})
	if err != nil {
		return errors.Errorf("failed to start container: %v", err)
	}

	doneCh := make(chan struct{})
	defer close(doneCh)
	go func() {
		select {
		case <-ctx.Done():
			if err := proc.Signal(ctx, syscall.SIGKILL); err != nil {
				logrus.Warnf("failed to kill process: %v", err)
			}
		case <-doneCh:
		}
	}()

	return proc.Wait()
}
