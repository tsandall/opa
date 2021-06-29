package ast

import "fmt"

type requireLoader struct {
}

func (rl *requireLoader) Load(modules map[string]*Module) (map[string]*Module, error) {

	return nil, fmt.Errorf("not implemented yet")
}
