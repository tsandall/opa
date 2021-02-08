package cmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"sort"
	"strings"

	"github.com/open-policy-agent/opa/ast"
	"github.com/open-policy-agent/opa/loader"
	"github.com/open-policy-agent/opa/rego"
	"github.com/open-policy-agent/opa/storage"
	"github.com/open-policy-agent/opa/topdown"
	"github.com/open-policy-agent/opa/util"
	"github.com/spf13/cobra"
)

type debugParams struct {
	inputPath string
}

func newDebugParams() debugParams {
	var debugParams debugParams
	return debugParams
}

func init() {

	params := newDebugParams()

	debugCommand := &cobra.Command{
		Use:   "debug",
		Short: "TODO",
		Long:  `TODO`,
		Run: func(cmd *cobra.Command, args []string) {
			err := debug(args, params, os.Stdout)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		},
	}

	addInputFlag(debugCommand.Flags(), &params.inputPath)

	RootCommand.AddCommand(debugCommand)
}

func debug(args []string, params debugParams, output io.Writer) error {

	lr, err := loader.All(args)

	if err != nil {
		return err
	}

	compiler, err := lr.Compiler()
	if err != nil {
		return err
	}

	store, err := lr.Store()
	if err != nil {
		return err
	}

	bs, err := ioutil.ReadFile(params.inputPath)
	if err != nil {
		return err
	}

	input := util.MustUnmarshalJSON(bs)

	dbg := newdebugger()

	dbg.stdin = bufio.NewReader(os.Stdin)
	dbg.output = os.Stdout
	dbg.store = store
	dbg.compiler = compiler
	dbg.input = input

	return dbg.Run(context.Background())
}

type debugger struct {
	store       storage.Store       // storage layer for evaluation
	compiler    *ast.Compiler       // compiler for evaluation
	input       interface{}         // input document for evaluation
	stdin       *bufio.Reader       // input stream to read from
	output      io.Writer           // output stream to write to
	breakpoints map[string]struct{} // set of breakpoints to halt at
	step        string              // last position when step command was given
	next        *nextToken          // last position when next command was given
	last        command             // last command to be entered through prompt()
}

func newdebugger() *debugger {
	return &debugger{
		breakpoints: map[string]struct{}{},
	}
}

func (dbg *debugger) Config() topdown.TraceConfig {
	return topdown.TraceConfig{}
}

func (dbg *debugger) Enabled() bool {
	return true
}

type nextToken struct {
	loc string
	id  uint64
}

func (dbg *debugger) TraceEvent(event topdown.Event) {

	// Only consider Eval and Redo events on expressions. All other events are ignored.
	switch event.Op {
	case topdown.EvalOp, topdown.RedoOp:
		_, ok := event.Node.(*ast.Expr)
		if !ok {
			return
		}
	default:
		return
	}

	// Check if expression matches a breakpoint. If we're not on a breakpoint (in order):
	//
	//  1. Check if we're stepping to the next expression.
	//  2. Check if we're stepping to the next expression in this query (skipping over rule/function invocations).
	//  3. Return.
	loc := event.Node.Loc().String()
	_, hit := dbg.breakpoints[loc]

	if !hit {
		if dbg.step != "" {
			if dbg.step == loc {
				return
			}
			hit = true
			dbg.step = ""
		} else if dbg.next != nil {
			if dbg.next.loc == loc {
				return
			}
			if dbg.next.id != event.QueryID {
				return
			}
			hit = true
			dbg.next = nil
		}
	}

	if !hit {
		return
	}

	// Print current location.
	text := string(event.Node.Loc().Text)
	if len(text) == 0 {
		text = event.Node.String()
	}

	fmt.Println("@", loc)

	// Wait for user input.
	for {

		cmd, err := dbg.prompt(true)
		if err != nil {
			// TODO: need some way to return error -- set on struct?
			panic(err)
		}

		switch cmd.name {
		case "continue":
			return
		case "step":
			dbg.step = loc
			return
		case "next":
			dbg.next = &nextToken{loc, event.QueryID}
			return
		case "dump":
			fmt.Println(event.Vars())
		default:
			fmt.Println("unknown command:", cmd.name)
		}
	}
}

func (dbg *debugger) Run(ctx context.Context) error {

	for {
		cmd, err := dbg.prompt(false)
		if err != nil {
			return err
		}

		switch cmd.name {
		case "list":
			locs := []*ast.Location{}
			for _, module := range dbg.compiler.Modules {
				ast.WalkExprs(module, func(x *ast.Expr) bool {
					locs = append(locs, x.Loc())
					return false
				})
			}
			sort.Slice(locs, func(i, j int) bool {
				return locs[i].Compare(locs[j]) < 0
			})
			for i := range locs {
				fmt.Println(locs[i])
			}
		case "breakpoint":
			loc := strings.TrimRight(cmd.args[0], "\n")
			dbg.setBreakpoint(loc)
		case "run":
			rs, err := rego.New(
				rego.Compiler(dbg.compiler),
				rego.Store(dbg.store),
				rego.Query(cmd.args[0]),
				rego.Input(dbg.input),
				rego.QueryTracer(dbg)).Eval(ctx)
			if err != nil {
				return err
			}
			_ = rs
			fmt.Println("result:", rs)
		default:
			fmt.Println("unknown command:", cmd.name)
		}
	}

}

func (dbg *debugger) setBreakpoint(s string) {
	dbg.breakpoints[s] = struct{}{}
}

func (dbg *debugger) prompt(hit bool) (command, error) {

	if !hit {
		fmt.Fprintf(dbg.output, "> ")

	} else {
		fmt.Fprintf(dbg.output, ">> ")
	}

	line, err := dbg.stdin.ReadString('\n')
	if err != nil {
		return command{}, err
	}

	line = strings.TrimSpace(line)

	if line == "" {
		return dbg.last, nil
	}

	var cmd command

	if line[0] != ':' {
		cmd.name = "query"
		cmd.args = []string{line}
	} else {
		parts := strings.SplitN(line, " ", 2)
		if len(parts) == 0 {
			return command{}, nil
		}

		cmd.name = parts[0][1:]
		cmd.args = parts[1:]
	}

	cmd.name = expandName(cmd.name)

	dbg.last = cmd
	return cmd, nil
}

type command struct {
	name string
	args []string
}

var names = []string{
	"breakpoint",
	"dump",
	"continue",
	"step",
	"next",
	"run",
	"list",
}

func expandName(name string) string {
	for i := range names {
		if strings.HasPrefix(names[i], name) {
			return names[i]
		}
	}
	return name
}
