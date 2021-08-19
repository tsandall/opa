package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"unicode"

	"github.com/open-policy-agent/opa/ast"
	"github.com/open-policy-agent/opa/rego"
	"github.com/spf13/cobra"
)

func init() {

	execCommand := &cobra.Command{
		Use:   "exec <path>",
		Short: "Execute a Rego file as a script",
		Long:  ``,
		Run: func(cmd *cobra.Command, args []string) {
			err := doExec(context.Background(), args, os.Stdout)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		},
	}

	RootCommand.AddCommand(execCommand)
}

func doExec(ctx context.Context, args []string, out io.Writer) error {

	if len(args) != 1 {
		return fmt.Errorf("opa exec: no rego file specified")
	}

	bs, err := ioutil.ReadFile(args[0])
	if err != nil {
		return err
	}

	module, err := ast.ParseModuleWithOpts(args[0], string(bs), ast.ParserOptions{ProcessAnnotation: true})
	if err == nil {
		return execModule(ctx, module, out)
	}

	query, err := ast.ParseBody(string(bs))
	if err == nil {
		return execQuery(ctx, query, out)
	}

	stmts, _, errs := ast.NewParser().
		WithFilename(args[0]).
		WithProcessAnnotation(true).
		WithReader(bytes.NewBuffer(bs)).
		Parse()
	if errs != nil {
		return errs
	}

	if len(stmts) == 0 {
		return fmt.Errorf("opa exec: no statements to execute")
	}

	return fmt.Errorf("opa exec: cannot execute %T statement", ast.TypeName(stmts[0]))
}

func execModule(ctx context.Context, module *ast.Module, out io.Writer) error {
	return fmt.Errorf("not implemented")
}

func execQuery(ctx context.Context, query ast.Body, out io.Writer) error {

	rs, err := rego.New(rego.ParsedQuery(query)).Eval(ctx)
	if err != nil {
		return err
	}

	bindings := ast.NewSet()

	for _, r := range rs {

		cpy := make(rego.Vars, len(r.Bindings))
		for k, v := range r.Bindings {
			if unicode.IsUpper(rune(k[0])) {
				cpy[k] = v
			}
		}

		val, err := ast.InterfaceToValue(cpy)
		if err != nil {
			return err
		}

		bindings.Add(ast.NewTerm(val))
	}

	opts := ast.JSONOpt{
		SortSets: true,
	}

	x, err := ast.JSONWithOpt(bindings, opts)
	if err != nil {
		return err
	}

	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(x)
}
