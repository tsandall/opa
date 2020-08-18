package wasm

import (
	"context"
	"fmt"

	"github.com/open-policy-agent/golang-opa-wasm/opa"
	"github.com/open-policy-agent/opa/ast"
	"github.com/open-policy-agent/opa/resolver"
)

func New(policy []byte, data interface{}) (*Resolver, error) {
	o, err := opa.New().
		WithPolicyBytes(policy).
		WithDataJSON(data).
		Init()
	if err != nil {
		return nil, err
	}
	return &Resolver{o: o}, nil
}

type Resolver struct {
	o *opa.OPA
}

func (r *Resolver) Close() {
	r.o.Close()
}

func (r *Resolver) Eval(ctx context.Context, input resolver.Input) (resolver.Result, error) {

	var inp *interface{}

	if input.Input != nil {
		x, err := ast.JSON(input.Input.Value)
		if err != nil {
			return resolver.Result{}, err
		}
		inp = &x
	}

	out, err := r.o.Eval(ctx, inp)
	if err != nil {
		return resolver.Result{}, err
	}

	result, err := getResult(out)
	if err != nil {
		return resolver.Result{}, err
	} else if result == nil {
		return resolver.Result{}, nil
	}

	v, err := ast.InterfaceToValue(*result)
	if err != nil {
		return resolver.Result{}, err
	}

	return resolver.Result{Value: v}, nil
}

func getResult(rs *opa.Result) (*interface{}, error) {

	r, ok := rs.Result.([]interface{})
	if !ok {
		return nil, fmt.Errorf("illegal result set type")
	}

	if len(r) == 0 {
		return nil, nil
	}

	m, ok := r[0].(map[string]interface{})
	if !ok || len(m) != 1 {
		return nil, fmt.Errorf("illegal result type")
	}

	result, ok := m["result"]
	if !ok {
		return nil, fmt.Errorf("missing value")
	}

	return &result, nil
}
