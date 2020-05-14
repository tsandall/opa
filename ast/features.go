package ast

import (
	"bytes"
	"io/ioutil"

	"github.com/open-policy-agent/opa/util"
)

type Features struct {
	Builtins map[string]*Builtin `json:"builtins"`
}

func NewFeaturesForThisVersion() *Features {

	f := &Features{
		Builtins: map[string]*Builtin{},
	}

	for _, bi := range Builtins {
		f.Builtins[bi.Name] = bi
	}

	return f
}

func LoadFeatures(path string) (*Features, error) {

	if path == "" {
		return NewFeaturesForThisVersion(), nil
	}

	bs, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var f Features

	return &f, util.NewJSONDecoder(bytes.NewBuffer(bs)).Decode(&f)
}
