package monitor

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"

	"github.com/containerd/console"
	"github.com/docker/buildx/controller/control"
	controllererrors "github.com/docker/buildx/controller/errdefs"
	controllerapi "github.com/docker/buildx/controller/pb"
	"github.com/docker/buildx/util/ioset"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/term"
)

const helpMessage = `
Available commands are:
  reload     reloads the context and build it.
  rollback   re-runs the interactive container with initial rootfs contents.
  list       list buildx sessions.
  attach     attach to a buildx server.
  disconnect disconnect a client from a buildx server. Specific session ID can be specified an arg.
  kill       kill buildx server.
  exit       exits monitor.
  help       shows this message.
`

// RunMonitor provides an interactive session for running and managing containers via specified IO.
func RunMonitor(ctx context.Context, curRef string, options *controllerapi.BuildOptions, invokeConfig controllerapi.ContainerConfig, c control.BuildxController, progressMode string, stdin io.ReadCloser, stdout io.WriteCloser, stderr console.File) error {
	defer func() {
		if err := c.Disconnect(ctx, curRef); err != nil {
			logrus.Warnf("disconnect error: %v", err)
		}
	}()
	monitorIn, monitorOut := ioset.Pipe()
	defer func() {
		monitorIn.Close()
	}()
	monitorEnableCh := make(chan struct{})
	monitorDisableCh := make(chan struct{})
	monitorOutCtx := ioset.MuxOut{
		Out:         monitorOut,
		EnableHook:  func() { monitorEnableCh <- struct{}{} },
		DisableHook: func() { monitorDisableCh <- struct{}{} },
	}

	containerIn, containerOut := ioset.Pipe()
	defer func() {
		containerIn.Close()
	}()
	containerOutCtx := ioset.MuxOut{
		Out: containerOut,
		// send newline to hopefully get the prompt; TODO: better UI (e.g. reprinting the last line)
		EnableHook:  func() { containerOut.Stdin.Write([]byte("\n")) },
		DisableHook: func() {},
	}

	invokeForwarder := ioset.NewForwarder()
	invokeForwarder.SetIn(&containerIn)
	m := &monitor{
		invokeIO: invokeForwarder,
		muxIO: ioset.NewMuxIO(ioset.In{
			Stdin:  io.NopCloser(stdin),
			Stdout: nopCloser{stdout},
			Stderr: nopCloser{stderr},
		}, []ioset.MuxOut{monitorOutCtx, containerOutCtx}, 1, func(prev int, res int) string {
			if prev == 0 && res == 0 {
				// No toggle happened because container I/O isn't enabled.
				return "No running interactive containers. You can start one by issuing rollback command\n"
			}
			return "Switched IO\n"
		}),
		invokeFunc: func(ctx context.Context, ref string, init bool, in io.ReadCloser, out io.WriteCloser, err io.WriteCloser) error {
			invokeConfig.Initial = init
			return c.Invoke(ctx, ref, invokeConfig, in, out, err)
		},
	}

	// Start container automatically
	fmt.Fprintf(stdout, "Launching interactive container. Press Ctrl-a-c to switch to monitor console\n")
	m.rollback(ctx, curRef, false)

	// Serve monitor commands
	monitorForwarder := ioset.NewForwarder()
	monitorForwarder.SetIn(&monitorIn)
	for {
		<-monitorEnableCh
		in, out := ioset.Pipe()
		monitorForwarder.SetOut(&out)
		doneCh, errCh := make(chan struct{}), make(chan error)
		go func() {
			defer close(doneCh)
			defer in.Close()
			go func() {
				<-ctx.Done()
				in.Close()
			}()
			t := term.NewTerminal(readWriter{in.Stdin, in.Stdout}, "(buildx) ")
			for {
				l, err := t.ReadLine()
				if err != nil {
					if err != io.EOF {
						errCh <- err
						return
					}
					return
				}
				args := strings.Fields(l) // TODO: use shlex
				if len(args) == 0 {
					continue
				}
				switch args[0] {
				case "":
					// nop
				case "reload":
					var bo *controllerapi.BuildOptions
					if curRef != "" {
						// Rebuilding an existing session; Restore the build option used for building this session.
						res, err := c.Inspect(ctx, curRef)
						if err != nil {
							fmt.Printf("failed to inspect the current build session: %v\n", err)
						} else {
							bo = res.Options
						}
					} else {
						bo = options
					}
					if bo == nil {
						fmt.Println("reload: no build option is provided")
						continue
					}
					if curRef != "" {
						if err := c.Disconnect(ctx, curRef); err != nil {
							fmt.Println("disconnect error", err)
						}
					}
					var resultUpdated bool
					ref, err := c.Build(ctx, *bo, nil, stdout, stderr, progressMode) // TODO: support stdin, hold build ref
					if err != nil {
						var be *controllererrors.BuildError
						if errors.As(err, &be) {
							curRef = be.Ref
							resultUpdated = true
						} else {
							fmt.Printf("failed to reload: %v\n", err)
						}
					} else {
						curRef = ref
						resultUpdated = true
					}
					if resultUpdated {
						// rollback the running container with the new result
						m.rollback(ctx, curRef, false)
						fmt.Fprint(stdout, "Interactive container was restarted. Press Ctrl-a-c to switch to the new container\n")
					}
				case "rollback":
					init := false
					if len(args) >= 2 && args[1] == "init" {
						init = true
					}
					m.rollback(ctx, curRef, init)
					fmt.Fprint(stdout, "Interactive container was restarted. Press Ctrl-a-c to switch to the new container\n")
				case "list":
					refs, err := c.List(ctx)
					if err != nil {
						fmt.Printf("failed to list: %v\n", err)
					}
					sort.Strings(refs)
					tw := tabwriter.NewWriter(stdout, 1, 8, 1, '\t', 0)
					fmt.Fprintln(tw, "ID\tCURRENT_SESSION")
					for _, k := range refs {
						fmt.Fprintf(tw, "%-20s\t%v\n", k, k == curRef)
					}
					tw.Flush()
				case "disconnect":
					target := curRef
					if len(args) >= 2 {
						target = args[1]
					}
					if err := c.Disconnect(ctx, target); err != nil {
						fmt.Println("disconnect error", err)
					}
				case "kill":
					if err := c.Kill(ctx); err != nil {
						fmt.Printf("failed to kill: %v\n", err)
					}
				case "attach":
					if len(args) < 2 {
						fmt.Println("attach: server name must be passed")
						continue
					}
					ref := args[1]
					m.rollback(ctx, ref, false)
					curRef = ref
				case "exit":
					return
				case "help":
					fmt.Fprint(stdout, helpMessage)
				default:
					fmt.Printf("unknown command: %q\n", l)
					fmt.Fprint(stdout, helpMessage)
				}
			}
		}()
		select {
		case <-doneCh:
			if m.curInvokeCancel != nil {
				m.curInvokeCancel()
			}
			return nil
		case err := <-errCh:
			if m.curInvokeCancel != nil {
				m.curInvokeCancel()
			}
			return err
		case <-monitorDisableCh:
		}
		monitorForwarder.SetOut(nil)
	}
}

type readWriter struct {
	io.Reader
	io.Writer
}

type monitor struct {
	muxIO           *ioset.MuxIO
	invokeIO        *ioset.Forwarder
	invokeFunc      func(context.Context, string, bool, io.ReadCloser, io.WriteCloser, io.WriteCloser) error
	curInvokeCancel func()
}

func (m *monitor) rollback(ctx context.Context, ref string, init bool) {
	if m.curInvokeCancel != nil {
		m.curInvokeCancel() // Finish the running container if exists
	}
	go func() {
		// Start a new container
		if err := m.invoke(ctx, ref, init); err != nil {
			logrus.Debugf("invoke error: %v", err)
		}
	}()
}

func (m *monitor) invoke(ctx context.Context, ref string, init bool) error {
	m.muxIO.Enable(1)
	defer m.muxIO.Disable(1)
	if ref == "" {
		return nil
	}
	invokeCtx, invokeCancel := context.WithCancel(ctx)

	containerIn, containerOut := ioset.Pipe()
	m.invokeIO.SetOut(&containerOut)
	waitInvokeDoneCh := make(chan struct{})
	var cancelOnce sync.Once
	curInvokeCancel := func() {
		cancelOnce.Do(func() {
			containerIn.Close()
			m.invokeIO.SetOut(nil)
			invokeCancel()
		})
		<-waitInvokeDoneCh
	}
	defer curInvokeCancel()
	m.curInvokeCancel = curInvokeCancel

	err := m.invokeFunc(invokeCtx, ref, init, containerIn.Stdin, containerIn.Stdout, containerIn.Stderr)
	close(waitInvokeDoneCh)

	return err
}

type nopCloser struct {
	io.Writer
}

func (c nopCloser) Close() error { return nil }
