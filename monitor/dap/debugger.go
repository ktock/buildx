package dap

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"

	"github.com/docker/buildx/controller"
	"github.com/docker/buildx/controller/control"
	controllererror "github.com/docker/buildx/controller/errdefs"
	controllerapi "github.com/docker/buildx/controller/pb"
	"github.com/docker/buildx/monitor/walker"
	"github.com/docker/cli/cli/command"
	"github.com/moby/buildkit/client/llb"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type debugger struct {
	mesCh     chan string
	bps       *walker.Breakpoints
	stoppedCh chan []int

	lConfig     *LaunchConfig
	debugCancel func() error

	bCtx       *walker.BreakContext
	controller control.BuildxController
	curRef     string
	onBreak    int64

	output io.Writer

	dockerCli command.Cli
}

func newDebugger(dockerCli command.Cli) (*debugger, error) {
	return &debugger{
		mesCh:     make(chan string),
		bps:       walker.NewBreakpoints(),
		stoppedCh: make(chan []int),
		output:    io.Discard,
		dockerCli: dockerCli,
	}, nil
}

func (d *debugger) launchConfig() *LaunchConfig {
	return d.lConfig
}

func (d *debugger) launch(cfg LaunchConfig, onStartHook, onFinishHook func(), stopOnEntry bool, pw io.Writer) error {
	if d.lConfig != nil {
		// debugger has already been lauched
		return errors.Errorf("multi session unsupported")
	}

	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()
	doneCh := make(chan struct{})
	defer close(doneCh)
	d.debugCancel = func() error {
		cancel()
		<-doneCh
		logrus.Debugf("debug finished")
		return nil
	}

	buildOpt, err := parseDAPBuildOpt(cfg)
	if err != nil {
		return err
	}

	d.lConfig = &cfg
	onStartHook()

	d.output = pw
	detach := true
	if cfg.ControllerMode == "local" {
		detach = false
	}
	c, err := controller.NewController(ctx, control.ControlOptions{
		Detach:       detach,
		ServerConfig: cfg.ServerConfig,
		Root:         cfg.Root,
	}, d.dockerCli)
	if err != nil {
		return err
	}
	d.controller = c
	buildOpt.Debug = true // we don't get the result but get only the build definition via error.
	ref, def, err := d.controller.Build(ctx, buildOpt, nil, pw, nil, "plain")
	if err != nil {
		var be *controllererror.BuildError
		if errors.As(err, &be) {
			ref = be.Ref
			def = be.Definition
			// We can proceed to dap session
		} else {
			return err
		}
	}
	d.curRef = ref
	defOp, err := llb.NewDefinitionOp(def)
	if err != nil {
		return err
	}
	if stopOnEntry {
		d.bps.Add("stopOnEntry", walker.NewStopOnEntryBreakpoint())
	}
	d.bps.Add("onError", walker.NewOnErrorBreakpoint()) // always break on error
	err = walker.NewWalker(d.bps, d.breakHandler,
		func(st llb.State) error {
			def, err := st.Marshal(ctx)
			if err != nil {
				return errors.Errorf("continue: failed to marshal definition: %v", err)
			}
			return d.controller.Continue(ctx, d.curRef, def.ToPB(), pw, nil, "plain")
		},
	).Walk(ctx, llb.NewState(defOp))

	onFinishHook()

	return err
}

func parseDAPBuildOpt(cfg LaunchConfig) (bo controllerapi.BuildOptions, _ error) {
	if cfg.Program == "" {
		return bo, errors.Errorf("program must be specified")
	}
	bo.Opts = &controllerapi.CommonOptions{}

	contextPath, dockerfile := filepath.Split(cfg.Program)
	bo.ContextPath = contextPath
	bo.DockerfileName = filepath.Join(contextPath, dockerfile)
	if target := cfg.Target; target != "" {
		bo.Target = target
	}
	for _, ba := range cfg.BuildArgs {
		bo.BuildArgs = append(bo.BuildArgs, ba)
	}
	bo.Outputs = append(bo.Outputs, "type=image")
	bo.SSH = cfg.SSH
	bo.Secrets = cfg.Secrets
	// TODO
	// - CacheFrom, CacheTo
	// - Contexts
	// - ExtraHosts
	// ...
	return bo, nil
}

func (d *debugger) cancel() error {
	close(d.stoppedCh)
	if d.debugCancel == nil {
		return nil
	}
	if err := d.debugCancel(); err != nil {
		logrus.WithError(err).Warnf("failed to close")
	}
	return nil
}

func (d *debugger) breakpoints() *walker.Breakpoints {
	return d.bps
}

func (d *debugger) doContinue() {
	if atomic.LoadInt64(&d.onBreak) == 1 {
		d.bps.BreakAllNode(false)
		d.mesCh <- "continue"
	} else {
		logrus.Warnf("continue is reqested but no break happens")
	}
}

func (d *debugger) doNext() {
	if atomic.LoadInt64(&d.onBreak) == 1 {
		d.bps.BreakAllNode(true)
		d.mesCh <- "next"
	} else {
		logrus.Warnf("next is reqested but no break happens")
	}
}

func (d *debugger) breakContext() *walker.BreakContext {
	return d.bCtx
}

func (d *debugger) stopped() <-chan []int {
	return d.stoppedCh
}

func (d *debugger) breakHandler(ctx context.Context, bCtx *walker.BreakContext) error {
	for key, r := range bCtx.Hits {
		logrus.Debugf("Breakpoint[%s]: %v", key, r)
	}
	bpIDs := make([]int, 0)
	for si := range bCtx.Hits {
		keyI, err := strconv.ParseInt(si, 10, 64)
		if err != nil {
			logrus.WithError(err).Warnf("failed to parse breakpoint key")
			continue
		}
		bpIDs = append(bpIDs, int(keyI))
	}
	def, err := bCtx.State.Marshal(ctx)
	if err != nil {
		return err
	}
	if err := d.controller.Continue(ctx, d.curRef, def.ToPB(), d.output, nil, "plain"); err != nil { // TODO: support stdin
		fmt.Fprintf(os.Stderr, "failed to continue: %v\n", err) // still debuggable
	}
	d.stoppedCh <- bpIDs
	d.bCtx = bCtx
	atomic.StoreInt64(&d.onBreak, 1)
	<-d.mesCh
	atomic.StoreInt64(&d.onBreak, 0)
	return nil
}
