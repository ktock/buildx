package monitor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"

	"github.com/containerd/console"
	"github.com/docker/buildx/controller/control"
	controllererrors "github.com/docker/buildx/controller/errdefs"
	controllerapi "github.com/docker/buildx/controller/pb"
	"github.com/docker/buildx/monitor/walker"
	"github.com/docker/buildx/util/ioset"
	"github.com/moby/buildkit/client/llb"
	solverpb "github.com/moby/buildkit/solver/pb"
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

Breakpoint commands:
  show        shows the Dockerfile
  break       set a breakpoint at the specified line
  breakpoints list key-value pairs of available breakpoints
  clear       clear the breakpoint specified by the key
  clearall    clear all breakpoints
  next        proceed to the next line
  continue    resume the build until the next breakpoint
`

type buildCtx struct {
	ref         string
	def         *solverpb.Definition
	breakpoints *walker.Breakpoints

	walker        *walker.Walker
	walkCancel    func()
	walkMu        sync.Mutex
	curWalkDoneCh chan struct{}
}

// RunMonitor provides an interactive session for running and managing containers via specified IO.
func RunMonitor(ctx context.Context, curRef string, def *solverpb.Definition, options *controllerapi.BuildOptions, invokeConfig controllerapi.ContainerConfig, c control.BuildxController, progressMode string, stdin io.ReadCloser, stdout io.WriteCloser, stderr console.File) error {
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
			return c.Invoke(ctx, ref, invokeConfig, in, out, err, nil, nil)
		},
	}

	// Start container automatically
	fmt.Fprintf(stdout, "Launching interactive container. Press Ctrl-a-c to switch to monitor console\n")
	m.rollback(ctx, curRef, false)

	// Serve monitor commands
	monitorForwarder := ioset.NewForwarder()
	monitorForwarder.SetIn(&monitorIn)
	bps := walker.NewBreakpoints()
	bps.Add("stopOnEntry", walker.NewStopOnEntryBreakpoint())
	bps.Add("stopOnErr", walker.NewOnErrorBreakpoint())
	curBuild := &buildCtx{
		ref:         curRef,
		def:         def,
		breakpoints: bps,
	}
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
					if curBuild.walker != nil {
						curBuild.walker.Close()
					}
					if curBuild.walkCancel != nil {
						curBuild.walkCancel() // TODO: ensure cancellation
					}
					ref, def, err := c.Build(ctx, *bo, nil, stdout, stderr, progressMode) // TODO: support stdin, hold build ref
					if err != nil {
						var be *controllererrors.BuildError
						if errors.As(err, &be) {
							ref = be.Ref
							def = be.Definition
						} else {
							fmt.Printf("failed to reload: %v\n", err)
							continue
						}
					}
					bps := walker.NewBreakpoints()
					bps.Add("stopOnEntry", walker.NewStopOnEntryBreakpoint())
					bps.Add("stopOnErr", walker.NewOnErrorBreakpoint())
					curBuild = &buildCtx{ref: ref, def: def, breakpoints: bps}
					// rollback the running container with the new result
					m.rollback(ctx, curBuild.ref, false)
					fmt.Fprint(stdout, "Interactive container was restarted. Press Ctrl-a-c to switch to the new container\n")
				case "show":
					if curBuild.def == nil {
						fmt.Printf("list: no build definition is provided\n")
						continue
					}
					if len(curBuild.def.Source.Infos) != 1 {
						fmt.Printf("list: multiple sources isn't supported\n")
						continue
					}
					var cursors []solverpb.Range
					if curBuild.walker != nil {
						cursors = curBuild.walker.GetCursors()
					}
					printLines(stdout, curBuild.def.Source.Infos[0], cursors, curBuild.breakpoints, 0, 0, true)
				case "break":
					if len(args) < 2 {
						fmt.Println("break: specify line")
						continue
					}
					line, err := strconv.ParseInt(args[1], 10, 64)
					if err != nil {
						fmt.Printf("break: invalid line number: %q: %v\n", args[1], err)
						continue
					}
					curBuild.breakpoints.Add("", walker.NewLineBreakpoint(line))
				case "breakpoints":
					curBuild.breakpoints.ForEach(func(key string, bp walker.Breakpoint) bool {
						fmt.Printf("%s %s\n", key, bp.String())
						return true
					})
				case "clear":
					if len(args) < 2 {
						fmt.Println("clear: specify breakpoint key")
						continue
					}
					curBuild.breakpoints.Clear(args[1])
				case "clearall":
					curBuild.breakpoints.ClearAll()
					curBuild.breakpoints.Add("stopOnEntry", walker.NewStopOnEntryBreakpoint()) // always enabled
					curBuild.breakpoints.Add("stopOnErr", walker.NewOnErrorBreakpoint())
				case "next":
					if curBuild.walker == nil { // TODO:  lock on walker
						fmt.Printf("next: walker isn't running. Run it using \"continue\" command.")
						continue
					}
					curBuild.walker.BreakAllNode(true)
					if curBuild.curWalkDoneCh != nil {
						close(curBuild.curWalkDoneCh)
						curBuild.curWalkDoneCh = nil
					}
				case "continue":
					if curBuild.def == nil {
						fmt.Printf("continue: no build definition is provided\n")
						continue
					}
					if curBuild.curWalkDoneCh != nil {
						close(curBuild.curWalkDoneCh)
						curBuild.curWalkDoneCh = nil
					}
					if (len(args) >= 2 && args[1] == "init") || curBuild.walker == nil {
						if curBuild.walker != nil {
							curBuild.walker.Close()
						}
						if curBuild.walkCancel != nil {
							curBuild.walkCancel() // TODO: ensure cancellation
						}
						defOp, err := llb.NewDefinitionOp(curBuild.def)
						if err != nil {
							fmt.Printf("continue: failed to get definition op: %v\n", err)
							continue
						}
						w := walker.NewWalker(curBuild.breakpoints, func(ctx context.Context, bCtx *walker.BreakContext) error {
							curBuild.walkMu.Lock()
							defer curBuild.walkMu.Unlock()
							var keys []string
							for k := range bCtx.Hits {
								keys = append(keys, k)
							}
							fmt.Printf("Break at %+v\n", keys)
							printLines(stdout, curBuild.def.Source.Infos[0], bCtx.Cursors, curBuild.breakpoints, 0, 0, true)
							m.rollback(ctx, curBuild.ref, false)
							curBuild.curWalkDoneCh = make(chan struct{})
							<-curBuild.curWalkDoneCh
							return nil
						}, func(st llb.State) error {
							d, err := st.Marshal(ctx)
							if err != nil {
								return errors.Errorf("continue: failed to marshal definition: %v", err)
							}
							err = c.Continue(ctx, curBuild.ref, d.ToPB(), stdout, stderr, progressMode)
							if err != nil {
								fmt.Printf("failed during walk: %v\n", err)
							}
							return err
						})
						ctx, cancel := context.WithCancel(ctx)
						curBuild.walkCancel = cancel
						curBuild.walker = w
						go func() {
							if err := curBuild.walker.Walk(ctx, llb.NewState(defOp)); err != nil {
								fmt.Printf("failed to walk LLB: %v\n", err)
							}
							fmt.Printf("walker finished\n")
							curBuild.walker.Close()
							curBuild.walker = nil
						}()
					}
				case "rollback":
					init := false
					if len(args) >= 2 && args[1] == "init" {
						init = true
					}
					m.rollback(ctx, curBuild.ref, init)
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
						fmt.Fprintf(tw, "%-20s\t%v\n", k, k == curBuild.ref)
					}
					tw.Flush()
				case "disconnect":
					target := curBuild.ref
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
					bps := walker.NewBreakpoints()
					bps.Add("stopOnEntry", walker.NewStopOnEntryBreakpoint())
					bps.Add("stopOnErr", walker.NewOnErrorBreakpoint())
					curBuild = &buildCtx{ref: ref, breakpoints: bps}
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

func printLines(w io.Writer, source *solverpb.SourceInfo, positions []solverpb.Range, bps *walker.Breakpoints, before, after int, all bool) {
	fmt.Fprintf(w, "Filename: %q\n", source.Filename)
	scanner := bufio.NewScanner(bytes.NewReader(source.Data))
	lastLinePrinted := false
	firstPrint := true
	for i := 1; scanner.Scan(); i++ {
		print := false
		target := false
		if len(positions) == 0 {
			print = true
		} else {
			for _, r := range positions {
				if all || int(r.Start.Line)-before <= i && i <= int(r.End.Line)+after {
					print = true
					if int(r.Start.Line) <= i && i <= int(r.End.Line) {
						target = true
						break
					}
				}
			}
		}

		if !print {
			lastLinePrinted = false
			continue
		}
		if !lastLinePrinted && !firstPrint {
			fmt.Fprintln(w, "----------------")
		}

		prefix := " "
		bps.ForEach(func(key string, b walker.Breakpoint) bool {
			if b.IsMarked(int64(i)) {
				prefix = "*"
				return false
			}
			return true
		})
		prefix2 := "  "
		if target {
			prefix2 = "=>"
		}
		fmt.Fprintln(w, prefix+prefix2+fmt.Sprintf("%4d| ", i)+scanner.Text())
		lastLinePrinted = true
		firstPrint = false
	}
}
